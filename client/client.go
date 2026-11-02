// Package client connects to a BangDB server (package server).
//
//	c, err := client.Dial(ctx, "db.internal:7432", &client.Options{Password: "s3cret"})
//	res, err := c.Exec(ctx, "INSERT INTO t (v) VALUES (?)", 1)
//	rows, err := c.Query(ctx, "SELECT id, v FROM t WHERE v > ?", 0)
//	for rows.Next() { vals := rows.Values() ... }
//
// A Conn is one session (BEGIN/COMMIT statements apply to it) and is not
// safe for concurrent use; open one per goroutine, or use database/sql with
// the driver package ("bangdb://user:password@host:port"), which pools them.
// Errors keep their kind: errors.Is(err, bangdb.ErrConstraint) works.
package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/value"
	"github.com/knoxxr/bangdb/internal/wire"
)

// Options configures a connection.
type Options struct {
	User, Password string
	// TLS, if set, is used to encrypt the connection.
	TLS *tls.Config
	// DialTimeout bounds connecting and the handshake. Default: 10s.
	DialTimeout time.Duration
}

// ErrUnauthorized is returned when the server rejects the credentials.
var ErrUnauthorized = errors.New("bangdb: authentication failed")

// ErrForbidden is returned when the account's role may not run a
// statement (e.g. a read-only account and an INSERT).
var ErrForbidden = errors.New("bangdb: permission denied")

// Error is an error reported by the server.
type Error struct {
	Code    byte
	Message string
}

func (e *Error) Error() string { return e.Message }

// Is maps server error codes to BangDB's error values.
func (e *Error) Is(target error) bool {
	switch e.Code {
	case wire.CodeConstraint:
		return target == bangdb.ErrConstraint
	case wire.CodeBusy:
		return target == bangdb.ErrBusy
	case wire.CodeReadOnly:
		return target == bangdb.ErrReadOnly
	case wire.CodeTxDone:
		return target == bangdb.ErrTxDone
	case wire.CodeInterrupted:
		return target == context.Canceled
	case wire.CodeUnauthorized:
		return target == ErrUnauthorized
	case wire.CodeForbidden:
		return target == ErrForbidden
	}
	return false
}

// Conn is a connection (session) to a server.
type Conn struct {
	c    net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	busy *Rows // an unfinished result: the next call drains it first
	inTx bool
	err  error // set when the connection is broken

	wmu sync.Mutex // guards w: cancels are written from other goroutines
}

// Dial connects and authenticates.
func Dial(ctx context.Context, addr string, opts *Options) (*Conn, error) {
	if opts == nil {
		opts = &Options{}
	}
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	var nc net.Conn
	var err error
	if opts.TLS != nil {
		td := tls.Dialer{NetDialer: &d, Config: opts.TLS}
		nc, err = td.DialContext(ctx, "tcp", addr)
	} else {
		nc, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	c := &Conn{c: nc, r: bufio.NewReaderSize(nc, 64<<10), w: bufio.NewWriterSize(nc, 64<<10)}
	nc.SetDeadline(time.Now().Add(timeout))
	b := wire.AppendString(nil, wire.Magic)
	b = wire.AppendString(b, opts.User)
	b = wire.AppendString(b, opts.Password)
	wire.WriteMsg(c.w, wire.Hello, b)
	if err := c.w.Flush(); err != nil {
		nc.Close()
		return nil, err
	}
	typ, payload, err := wire.ReadMsg(c.r)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bangdb: handshake: %w", err)
	}
	nc.SetDeadline(time.Time{})
	switch typ {
	case wire.OK:
		return c, nil
	case wire.Error:
		nc.Close()
		return nil, decodeError(payload)
	}
	nc.Close()
	return nil, errors.New("bangdb: unexpected handshake reply")
}

func decodeError(p []byte) error {
	if len(p) == 0 {
		return &Error{Message: "bangdb: server error"}
	}
	return &Error{Code: p[0], Message: string(p[1:])}
}

// Close closes the connection; the server rolls back an open transaction.
func (c *Conn) Close() error { return c.c.Close() }

// InTx reports whether the session has an open transaction (as of the
// last completed statement).
func (c *Conn) InTx() bool { return c.inTx }

// Ping checks that the server answers.
func (c *Conn) Ping(ctx context.Context) error {
	if err := c.ready(); err != nil {
		return err
	}
	c.wmu.Lock()
	wire.WriteMsg(c.w, wire.Ping, nil)
	err := c.w.Flush()
	c.wmu.Unlock()
	if err != nil {
		return c.broken(err)
	}
	stop := c.watch(ctx)
	typ, _, err := wire.ReadMsg(c.r)
	stop()
	if err != nil {
		return c.broken(err)
	}
	if typ != wire.OK {
		return c.broken(errors.New("bangdb: unexpected reply to ping"))
	}
	return nil
}

// Result is the outcome of Exec.
type Result struct {
	Columns      []string
	Types        []string
	Rows         [][]any // rows of RETURNING (or of a query run with Exec)
	RowsAffected int64
	LastInsertID int64
}

// Exec runs one or more statements and returns the result of the last.
func (c *Conn) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	rows, err := c.send(ctx, wire.ModeExec, query, args)
	if err != nil {
		return Result{}, err
	}
	res := Result{Columns: rows.cols, Types: rows.types}
	for rows.Next() {
		res.Rows = append(res.Rows, rows.Values())
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	res.RowsAffected, res.LastInsertID = rows.affected, rows.lastID
	return res, nil
}

// Query runs a query and streams its rows. The Rows must be read to the end
// or closed before the connection is used again (Close cancels the rest
// of the query).
func (c *Conn) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	return c.send(ctx, wire.ModeQuery, query, args)
}

// ready finishes the previous result, if any.
func (c *Conn) ready() error {
	if c.err != nil {
		return c.err
	}
	if c.busy != nil {
		c.busy.Close()
	}
	return c.err
}

func (c *Conn) broken(err error) error {
	if c.err == nil {
		c.err = fmt.Errorf("bangdb: connection broken: %w", err)
		c.c.Close()
	}
	return err
}

// watch sends a cancel when ctx is done before stop is called.
func (c *Conn) watch(ctx context.Context) (stop func()) {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			c.wmu.Lock()
			wire.WriteMsg(c.w, wire.Cancel, nil)
			c.w.Flush()
			c.wmu.Unlock()
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func (c *Conn) send(ctx context.Context, mode byte, query string, args []any) (*Rows, error) {
	if err := c.ready(); err != nil {
		return nil, err
	}
	b := append([]byte{mode}, wire.AppendString(nil, query)...)
	b = binary.AppendUvarint(b, uint64(len(args)))
	vals := make([]any, len(args))
	for i, a := range args {
		name := ""
		switch na := a.(type) {
		case bangdb.NamedArg:
			name, a = na.Name, na.Value
		case sql.NamedArg:
			name, a = na.Name, na.Value
		}
		v, err := value.Normalize(a)
		if err != nil {
			return nil, err
		}
		b = wire.AppendString(b, name)
		vals[i] = v
	}
	b = append(b, value.EncodeRow(vals)...)
	c.wmu.Lock()
	wire.WriteMsg(c.w, wire.Query, b)
	err := c.w.Flush()
	c.wmu.Unlock()
	if err != nil {
		return nil, c.broken(err)
	}
	r := &Rows{c: c, stop: c.watch(ctx)}
	c.busy = r
	// The first reply: columns, or done / error.
	if !r.advance() && r.err != nil {
		return nil, r.err
	}
	return r, nil
}

// Rows is a streamed result.
type Rows struct {
	c     *Conn
	cols  []string
	types []string
	row   []any
	done  bool
	err   error
	stop  func()

	affected, lastID int64
}

// advance reads replies until the columns (true) or the end (false).
func (r *Rows) advance() bool {
	typ, payload, err := r.read()
	if err != nil {
		return false
	}
	switch typ {
	case wire.Columns:
		rd := wire.Reader{B: payload}
		names, err1 := value.DecodeRow(rd.Bytes())
		types, err2 := value.DecodeRow(rd.Bytes())
		if rd.Err != nil || err1 != nil || err2 != nil {
			r.fail(errors.New("bangdb: malformed column list"))
			return false
		}
		for i, n := range names {
			s, _ := n.(string)
			r.cols = append(r.cols, s)
			t := ""
			if i < len(types) {
				t, _ = types[i].(string)
			}
			r.types = append(r.types, t)
		}
		return true
	default:
		r.handleEnd(typ, payload)
		return false
	}
}

func (r *Rows) read() (byte, []byte, error) {
	typ, payload, err := wire.ReadMsg(r.c.r)
	if err != nil {
		r.fail(r.c.broken(err))
		return 0, nil, err
	}
	return typ, payload, nil
}

func (r *Rows) fail(err error) {
	if r.err == nil {
		r.err = err
	}
	r.finish()
}

func (r *Rows) finish() {
	if !r.done {
		r.done = true
		r.stop()
		if r.c.busy == r {
			r.c.busy = nil
		}
	}
}

// handleEnd processes done / error replies.
func (r *Rows) handleEnd(typ byte, payload []byte) {
	switch typ {
	case wire.Done:
		rd := wire.Reader{B: payload}
		r.affected, r.lastID = rd.Varint(), rd.Varint()
		r.c.inTx = rd.Byte() == 1
		r.finish()
	case wire.Error:
		r.fail(decodeError(payload))
	default:
		r.fail(r.c.broken(fmt.Errorf("unexpected message %q", typ)))
	}
}

// Columns returns the result column names.
func (r *Rows) Columns() []string { return r.cols }

// ColumnTypes returns the declared column types ("" where unknown).
func (r *Rows) ColumnTypes() []string { return r.types }

// Next advances to the next row.
func (r *Rows) Next() bool {
	if r.done {
		return false
	}
	typ, payload, err := r.read()
	if err != nil {
		return false
	}
	if typ != wire.Row {
		r.handleEnd(typ, payload)
		return false
	}
	vals, err := value.DecodeRow(payload)
	if err != nil {
		r.fail(r.c.broken(err))
		return false
	}
	r.row = vals
	return true
}

// Values returns the current row (nil, int64, float64, string or []byte).
func (r *Rows) Values() []any { return r.row }

// Scan copies the current row into dest, like bangdb.Rows.Scan.
func (r *Rows) Scan(dest ...any) error {
	if r.row == nil {
		return errors.New("bangdb: Scan called without a current row")
	}
	return bangdb.ScanValues(r.row, dest...)
}

// Err returns the error that ended the result, if any.
func (r *Rows) Err() error { return r.err }

// Close stops the query (asking the server to cancel it) and discards the
// remaining rows.
func (r *Rows) Close() error {
	if r.done {
		return nil
	}
	r.c.wmu.Lock()
	wire.WriteMsg(r.c.w, wire.Cancel, nil)
	err := r.c.w.Flush()
	r.c.wmu.Unlock()
	if err != nil {
		r.fail(r.c.broken(err))
		return nil
	}
	for r.Next() {
	}
	if r.err != nil && errors.Is(r.err, context.Canceled) {
		r.err = nil // the cancel was ours
	}
	return nil
}

// ParseDSN splits "bangdb://user:password@host:port?tls=1" into an address
// and options (tls=1 uses TLS with the system roots; tls=insecure skips
// verification, for tests).
func ParseDSN(dsn string) (string, *Options, error) {
	rest, ok := strings.CutPrefix(dsn, "bangdb://")
	if !ok {
		return "", nil, errors.New("bangdb: DSN must start with bangdb://")
	}
	opts := &Options{}
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		cred := rest[:at]
		rest = rest[at+1:]
		user, pass, _ := strings.Cut(cred, ":")
		opts.User, opts.Password = user, pass
	}
	addr, query, _ := strings.Cut(rest, "?")
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "tls" && (v == "1" || v == "true"):
			host, _, _ := net.SplitHostPort(addr)
			opts.TLS = &tls.Config{ServerName: host}
		case k == "tls" && v == "insecure":
			opts.TLS = &tls.Config{InsecureSkipVerify: true}
		}
	}
	if addr == "" {
		return "", nil, errors.New("bangdb: DSN without host")
	}
	return addr, opts, nil
}
