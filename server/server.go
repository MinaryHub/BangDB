// Package server serves a BangDB database to other processes over TCP (or
// TLS), for applications where several services share one database. The
// client package (and the database/sql driver with a "bangdb://" DSN)
// connects to it.
//
//	db, _ := bangdb.Open("app.db")
//	s := server.New(db, server.Options{Password: "s3cret"})
//	go s.ListenAndServe("127.0.0.1:7432")
//
// Each connection is a session: BEGIN/COMMIT/ROLLBACK statements work as
// with a local Conn, and an open transaction is rolled back when the
// connection closes. Queries stream their rows; a client can cancel the
// running statement.
package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
	"github.com/knoxxr/bangdb/internal/wire"
)

// Options configures a server.
type Options struct {
	// Authenticate checks the user and password of a connection. If nil
	// and Password is set, any user with that password is accepted; with
	// neither, every connection is accepted (bind to localhost then).
	Authenticate func(user, password string) bool
	Password     string
	// MaxConns limits concurrent connections (0: unlimited).
	MaxConns int
	// IdleTimeout closes connections idle for this long (0: never).
	IdleTimeout time.Duration
}

// Stats counts server activity.
type Stats struct {
	Connections int    // open now
	Accepted    uint64 // accepted since start
	Rejected    uint64 // failed authentication or over MaxConns
	Statements  uint64
}

// Server serves one database.
type Server struct {
	db   *engine.DB
	opts Options

	mu     sync.Mutex
	lns    []net.Listener
	conns  map[*session]struct{}
	closed bool
	wg     sync.WaitGroup

	accepted, rejected, statements atomic.Uint64
}

// New returns a server for db.
func New(db *bangdb.DB, opts Options) *Server {
	return &Server{db: db.Engine(), opts: opts, conns: map[*session]struct{}{}}
}

// ListenAndServe serves on a TCP address.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// ListenAndServeTLS serves on a TCP address with TLS.
func (s *Server) ListenAndServeTLS(addr string, cfg *tls.Config) error {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve accepts connections on ln until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return errors.New("server: closed")
	}
	s.lns = append(s.lns, ln)
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

// Stats returns the server's counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	return Stats{Connections: n, Accepted: s.accepted.Load(), Rejected: s.rejected.Load(), Statements: s.statements.Load()}
}

// Close stops listening, interrupts running statements, closes every
// connection (rolling back open transactions) and waits for them.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	for _, ln := range s.lns {
		ln.Close()
	}
	for c := range s.conns {
		c.close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

type session struct {
	s      *Server
	c      net.Conn
	r      *bufio.Reader
	w      *bufio.Writer
	conn   *engine.Conn
	cancel atomic.Pointer[context.CancelFunc] // of the running statement
	msgs   chan message
	done   chan struct{}
	once   sync.Once
}

type message struct {
	typ     byte
	payload []byte
	err     error
}

func (ss *session) close() {
	ss.once.Do(func() {
		close(ss.done)
		ss.c.Close()
		if f := ss.cancel.Load(); f != nil {
			(*f)()
		}
	})
}

func (s *Server) handle(c net.Conn) {
	ss := &session{s: s, c: c, r: bufio.NewReaderSize(c, 64<<10), w: bufio.NewWriterSize(c, 64<<10),
		msgs: make(chan message, 8), done: make(chan struct{})}
	defer ss.close()

	// Hello and authentication.
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := wire.ReadMsg(ss.r)
	if err != nil || typ != wire.Hello {
		return
	}
	c.SetReadDeadline(time.Time{})
	rd := wire.Reader{B: payload}
	magic, user, password := rd.String(), rd.String(), rd.String()
	if rd.Err != nil || magic != wire.Magic {
		ss.fail(wire.CodeGeneric, "server: bad hello (or a client of another version)")
		return
	}
	if !s.authenticate(user, password) {
		s.rejected.Add(1)
		time.Sleep(100 * time.Millisecond) // slow down guessing
		ss.fail(wire.CodeUnauthorized, "server: authentication failed")
		return
	}
	s.mu.Lock()
	if s.closed || s.opts.MaxConns > 0 && len(s.conns) >= s.opts.MaxConns {
		s.mu.Unlock()
		s.rejected.Add(1)
		ss.fail(wire.CodeTooMany, "server: too many connections")
		return
	}
	s.conns[ss] = struct{}{}
	s.mu.Unlock()
	s.accepted.Add(1)
	ss.conn = s.db.Conn()
	defer func() {
		ss.conn.Close() // rolls back an open transaction
		s.mu.Lock()
		delete(s.conns, ss)
		s.mu.Unlock()
	}()
	wire.WriteMsg(ss.w, wire.OK, nil)
	if ss.w.Flush() != nil {
		return
	}

	go ss.readLoop()
	for {
		var m message
		select {
		case m = <-ss.msgs:
		case <-ss.done:
			return
		}
		if m.err != nil {
			return
		}
		switch m.typ {
		case wire.Ping:
			wire.WriteMsg(ss.w, wire.OK, nil)
		case wire.Query:
			s.statements.Add(1)
			if err := ss.query(m.payload); err != nil {
				return
			}
		case wire.Cancel:
			// Arrived after its statement ended.
		default:
			return
		}
		if ss.w.Flush() != nil {
			return
		}
	}
}

func (s *Server) authenticate(user, password string) bool {
	switch {
	case s.opts.Authenticate != nil:
		return s.opts.Authenticate(user, password)
	case s.opts.Password != "":
		return subtle.ConstantTimeCompare([]byte(password), []byte(s.opts.Password)) == 1
	}
	return true
}

// readLoop reads client messages; a cancel stops the running statement
// at once, everything else is handled in order by the session.
func (ss *session) readLoop() {
	for {
		if t := ss.s.opts.IdleTimeout; t > 0 {
			ss.c.SetReadDeadline(time.Now().Add(t))
		}
		typ, payload, err := wire.ReadMsg(ss.r)
		if err == nil && typ == wire.Cancel {
			if f := ss.cancel.Load(); f != nil {
				(*f)()
			}
			continue
		}
		select {
		case ss.msgs <- message{typ, payload, err}:
		case <-ss.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (ss *session) fail(code byte, msg string) {
	b := append([]byte{code}, msg...)
	wire.WriteMsg(ss.w, wire.Error, b)
	ss.w.Flush()
}

// query runs one statement and writes its answer. Only I/O errors are
// returned (they end the session).
func (ss *session) query(payload []byte) error {
	rd := wire.Reader{B: payload}
	mode := rd.Byte()
	src := rd.String()
	n := rd.Uvarint()
	if n > 1<<16 {
		rd.Err = errors.New("too many arguments")
	}
	names := make([]string, 0, n)
	for i := uint64(0); i < n && rd.Err == nil; i++ {
		names = append(names, rd.String())
	}
	if rd.Err != nil {
		return ss.writeErr(rd.Err)
	}
	vals, err := value.DecodeRow(rd.B)
	if err != nil || uint64(len(vals)) != n {
		return ss.writeErr(errors.New("server: malformed arguments"))
	}
	args := make([]any, n)
	for i, v := range vals {
		if names[i] != "" {
			args[i] = engine.NamedArg{Name: names[i], Value: v}
		} else {
			args[i] = v
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss.cancel.Store(&cancel)
	defer ss.cancel.Store(nil)
	select {
	case <-ss.done:
		return errors.New("closed")
	default:
	}

	if mode == wire.ModeExec {
		res, err := ss.conn.ExecContext(ctx, src, args...)
		if err != nil {
			return ss.writeErr(err)
		}
		if len(res.Columns) > 0 {
			ss.writeColumns(res.Columns, res.Types)
			for _, row := range res.Rows {
				if err := wire.WriteMsg(ss.w, wire.Row, value.EncodeRow(row)); err != nil {
					return err
				}
			}
		}
		return ss.writeDone(res.RowsAffected, res.LastInsertID)
	}
	rows, err := ss.conn.QueryRows(ctx, src, args...)
	if err != nil {
		return ss.writeErr(err)
	}
	defer rows.Close()
	ss.writeColumns(rows.Columns(), rows.ColumnTypes())
	for rows.Next() {
		if err := wire.WriteMsg(ss.w, wire.Row, value.EncodeRow(rows.Row())); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return ss.writeErr(err)
	}
	return ss.writeDone(0, 0)
}

func (ss *session) writeColumns(cols, types []string) {
	names := make([]any, len(cols))
	for i, c := range cols {
		names[i] = c
	}
	ts := make([]any, len(cols))
	for i := range ts {
		ts[i] = ""
		if i < len(types) {
			ts[i] = types[i]
		}
	}
	b := wire.AppendBytes(nil, value.EncodeRow(names))
	b = wire.AppendBytes(b, value.EncodeRow(ts))
	wire.WriteMsg(ss.w, wire.Columns, b)
}

func (ss *session) writeDone(affected, lastID int64) error {
	b := binary.AppendVarint(nil, affected)
	b = binary.AppendVarint(b, lastID)
	inTx := byte(0)
	if ss.conn.InTx() {
		inTx = 1
	}
	return wire.WriteMsg(ss.w, wire.Done, append(b, inTx))
}

func (ss *session) writeErr(err error) error {
	code := byte(wire.CodeGeneric)
	switch {
	case errors.Is(err, bangdb.ErrConstraint):
		code = wire.CodeConstraint
	case errors.Is(err, bangdb.ErrBusy):
		code = wire.CodeBusy
	case errors.Is(err, bangdb.ErrReadOnly):
		code = wire.CodeReadOnly
	case errors.Is(err, bangdb.ErrTxDone):
		code = wire.CodeTxDone
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = wire.CodeInterrupted
	}
	return wire.WriteMsg(ss.w, wire.Error, append([]byte{code}, err.Error()...))
}
