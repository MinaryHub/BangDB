// Package engine ties storage, catalog and SQL execution together.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

var (
	// ErrBusy is returned when the write lock cannot be acquired in time.
	ErrBusy = errors.New("bangdb: database is locked (another write transaction is active)")
	// ErrTxDone is returned when using a committed or rolled back transaction.
	ErrTxDone = errors.New("bangdb: transaction has already been committed or rolled back")
	// ErrConstraint wraps constraint violations (UNIQUE, NOT NULL, ...).
	ErrConstraint = errors.New("constraint failed")
)

// Options configures a database.
type Options struct {
	// BusyTimeout is how long a writer waits for another write transaction
	// to finish. Default: 5s.
	BusyTimeout time.Duration
	// QueryTimeout limits the run time of every statement whose context has
	// no deadline of its own. Default: no limit.
	QueryTimeout time.Duration
}

// Result is the outcome of executing SQL.
type Result struct {
	Columns      []string
	Rows         [][]any
	RowsAffected int64
	LastInsertID int64
}

// DB is an open database. It is safe for concurrent use.
//
// Concurrency: any number of readers run in parallel with at most one write
// transaction. Each statement outside a transaction reads a pager snapshot of
// the latest commit, so readers and writers never wait for each other (a
// reader only waits for the in-memory publication of a commit, never for its
// I/O).
type DB struct {
	p        *pager.Pager
	writeSem chan struct{} // capacity 1: the single-writer lock
	// pubMu makes "publish a commit + install its catalog" atomic with
	// respect to "take a snapshot + read the catalog".
	pubMu sync.Mutex
	// swapMu guards the pager pointer: readers hold R for a statement,
	// replacing or closing the pager takes W.
	swapMu       sync.RWMutex
	catMu        sync.Mutex
	cat          *Catalog // committed catalog (immutable)
	busyTimeout  time.Duration
	queryTimeout time.Duration

	cacheMu   sync.Mutex
	stmtCache map[string]parsed

	path     string // "" for in-memory
	readOnly atomic.Bool
	cursors  cursorSet
	repl     *replLog

	closeOnce sync.Once
	closed    chan struct{}
}

type parsed struct {
	stmts   []sql.Stmt
	nparams int
}

// Open opens a database file. The path ":memory:" (or "") creates a private
// in-memory database.
func Open(path string, opts *Options) (*DB, error) {
	var p *pager.Pager
	if path == "" || path == ":memory:" {
		path = ""
		p = pager.OpenMemory()
	} else {
		var err error
		if p, err = pager.Open(path); err != nil {
			return nil, err
		}
	}
	db := &DB{p: p, writeSem: make(chan struct{}, 1), busyTimeout: 5 * time.Second,
		stmtCache: map[string]parsed{}, closed: make(chan struct{}), path: path}
	db.repl = newReplLog(p.Header().Seq)
	p.SetOnCommit(db.repl.onCommit)
	if opts != nil && opts.BusyTimeout > 0 {
		db.busyTimeout = opts.BusyTimeout
	}
	if opts != nil {
		db.queryTimeout = opts.QueryTimeout
	}
	cat, err := loadCatalog(p.NewView())
	if err != nil {
		p.Close()
		return nil, err
	}
	db.cat = cat
	return db, nil
}

// Close closes the database. Pending write transactions are waited for up to
// the busy timeout.
func (db *DB) Close() error {
	var err error
	db.closeOnce.Do(func() {
		if lerr := db.lockWrite(context.Background()); lerr != nil {
			err = fmt.Errorf("bangdb: close: %w", lerr)
			return
		}
		close(db.closed)
		db.cursors.cancelAll() // open cursors release their snapshots
		db.swapMu.Lock()
		err = db.p.Close()
		db.swapMu.Unlock()
		db.repl.mu.Lock()
		db.repl.disabled = true
		close(db.repl.notify)
		db.repl.notify = make(chan struct{})
		db.repl.mu.Unlock()
	})
	return err
}

func (db *DB) isClosed() bool {
	select {
	case <-db.closed:
		return true
	default:
		return false
	}
}

// lockWrite takes the single-writer lock, waiting up to the busy timeout
// or until ctx is done.
func (db *DB) lockWrite(ctx context.Context) error {
	select {
	case db.writeSem <- struct{}{}:
		return nil
	default:
	}
	t := time.NewTimer(db.busyTimeout)
	defer t.Stop()
	select {
	case db.writeSem <- struct{}{}:
		return nil
	case <-t.C:
		return ErrBusy
	case <-ctx.Done():
		return interrupted(ctx.Err())
	}
}

// interrupted wraps a context error; errors.Is(err, context.Canceled /
// context.DeadlineExceeded) still works.
func interrupted(err error) error { return fmt.Errorf("bangdb: statement interrupted: %w", err) }

// stmtContext applies the default query timeout to ctx.
func (db *DB) stmtContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, has := ctx.Deadline(); !has && db.queryTimeout > 0 {
		return context.WithTimeout(ctx, db.queryTimeout)
	}
	return ctx, func() {}
}

func (db *DB) unlockWrite() { <-db.writeSem }

func (db *DB) committedCatalog() *Catalog {
	db.catMu.Lock()
	defer db.catMu.Unlock()
	return db.cat
}

// Catalog returns the committed schema.
func (db *DB) Catalog() *Catalog { return db.committedCatalog() }

func (db *DB) parse(src string) (parsed, error) {
	db.cacheMu.Lock()
	pr, ok := db.stmtCache[src]
	db.cacheMu.Unlock()
	if ok {
		return pr, nil
	}
	stmts, n, err := sql.Parse(src)
	if err != nil {
		return parsed{}, err
	}
	pr = parsed{stmts, n}
	db.cacheMu.Lock()
	if len(db.stmtCache) >= 512 {
		clear(db.stmtCache)
	}
	db.stmtCache[src] = pr
	db.cacheMu.Unlock()
	return pr, nil
}

func normalizeArgs(args []any, want int) ([]any, error) {
	if len(args) != want {
		return nil, fmt.Errorf("bangdb: expected %d arguments, got %d", want, len(args))
	}
	out := make([]any, len(args))
	for i, a := range args {
		v, err := value.Normalize(a)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Exec runs one or more statements. Scripts may contain BEGIN/COMMIT; a
// transaction left open at the end of the script is rolled back.
func (db *DB) Exec(src string, args ...any) (Result, error) {
	return db.ExecContext(context.Background(), src, args...)
}

// ExecContext is Exec with cancellation: when ctx is done, the running
// statement stops and is rolled back.
func (db *DB) ExecContext(ctx context.Context, src string, args ...any) (Result, error) {
	c := db.Conn()
	res, err := c.ExecContext(ctx, src, args...)
	if c.tx != nil {
		c.tx.Rollback()
		if err == nil {
			err = errors.New("bangdb: script left a transaction open; it was rolled back (use DB.Begin or Conn for multi-call transactions)")
		}
	}
	return res, err
}

// Query is Exec; it exists for readability at call sites.
func (db *DB) Query(src string, args ...any) (Result, error) { return db.Exec(src, args...) }

// QueryContext is ExecContext; it exists for readability at call sites.
func (db *DB) QueryContext(ctx context.Context, src string, args ...any) (Result, error) {
	return db.ExecContext(ctx, src, args...)
}

// Begin starts an explicit write transaction. It blocks (up to the busy
// timeout) while another write transaction is active.
func (db *DB) Begin() (*Tx, error) { return db.BeginContext(context.Background()) }

// BeginContext is Begin; ctx only bounds the wait for the write lock.
func (db *DB) BeginContext(ctx context.Context) (*Tx, error) {
	if db.isClosed() {
		return nil, pager.ErrClosed
	}
	if db.readOnly.Load() {
		return nil, ErrReadOnly
	}
	if err := db.lockWrite(ctx); err != nil {
		return nil, err
	}
	if db.isClosed() {
		db.unlockWrite()
		return nil, pager.ErrClosed
	}
	return &Tx{db: db, v: db.p.NewView(), cat: db.committedCatalog(), rowids: map[string]int64{}}, nil
}

// read runs fn against a snapshot of the latest commit.
func (db *DB) read(ctx context.Context, fn func(x *execCtx) (Result, error), args []any) (Result, error) {
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
	if db.isClosed() {
		return Result{}, pager.ErrClosed
	}
	db.pubMu.Lock()
	snap, err := db.p.Snapshot()
	cat := db.committedCatalog()
	db.pubMu.Unlock()
	if err != nil {
		return Result{}, err
	}
	defer snap.Release()
	x := &execCtx{v: snap.View(), cat: cat, args: args, ctx: ctx}
	return fn(x)
}

// publish makes a prepared commit and its catalog visible, then runs a
// checkpoint if the WAL is large. The caller holds the write lock.
func (db *DB) publish(c *pager.Prepared, cat func() (*Catalog, error)) error {
	db.pubMu.Lock()
	db.p.Publish(c)
	var err error
	if c != nil {
		var nc *Catalog
		if nc, err = cat(); err == nil {
			db.catMu.Lock()
			db.cat = nc
			db.catMu.Unlock()
		}
	}
	db.pubMu.Unlock()
	if err == nil && db.p.NeedsCheckpoint() {
		// A failed checkpoint does not undo the commit; it is retried later
		// and a broken file is reported by the next operation.
		db.p.Checkpoint()
	}
	return err
}

// Checkpoint copies committed pages from the WAL into the database file as
// far as active readers allow. It runs automatically; calling it explicitly
// is only useful before copying the database file by hand.
func (db *DB) Checkpoint() error {
	if err := db.lockWrite(context.Background()); err != nil {
		return err
	}
	defer db.unlockWrite()
	if db.isClosed() {
		return pager.ErrClosed
	}
	return db.p.Checkpoint()
}

// Tx is a write transaction. It must be finished with Commit or Rollback.
type Tx struct {
	db     *DB
	v      *pager.View
	cat    *Catalog
	rowids map[string]int64 // next-rowid cache per table
	done   bool
}

// Exec runs statements inside the transaction. A failing statement is rolled
// back on its own; the transaction stays usable.
func (tx *Tx) Exec(src string, args ...any) (Result, error) {
	return tx.ExecContext(context.Background(), src, args...)
}

// ExecContext is Exec with cancellation of the running statement.
func (tx *Tx) ExecContext(ctx context.Context, src string, args ...any) (Result, error) {
	if tx.done {
		return Result{}, ErrTxDone
	}
	pr, err := tx.db.parse(src)
	if err != nil {
		return Result{}, err
	}
	nargs, err := normalizeArgs(args, pr.nparams)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, s := range pr.stmts {
		switch s.(type) {
		case *sql.Begin, *sql.Commit, *sql.Rollback:
			return res, errors.New("bangdb: use Tx.Commit / Tx.Rollback instead of transaction statements")
		}
		if res, err = tx.execStmt(ctx, s, nargs); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Query is Exec; it exists for readability at call sites.
func (tx *Tx) Query(src string, args ...any) (Result, error) { return tx.Exec(src, args...) }

// QueryContext is ExecContext; it exists for readability at call sites.
func (tx *Tx) QueryContext(ctx context.Context, src string, args ...any) (Result, error) {
	return tx.ExecContext(ctx, src, args...)
}

func (tx *Tx) execStmt(ctx context.Context, s sql.Stmt, args []any) (Result, error) {
	ctx, cancel := tx.db.stmtContext(ctx)
	defer cancel()
	sp := tx.v.Savepoint()
	cat := tx.cat
	rowids := make(map[string]int64, len(tx.rowids))
	for k, v := range tx.rowids {
		rowids[k] = v
	}
	x := &execCtx{v: tx.v, cat: tx.cat, tx: tx, args: args, ctx: ctx}
	res, err := x.exec(s)
	if err != nil {
		tx.v.Restore(sp)
		tx.cat = cat
		tx.rowids = rowids
		return Result{}, err
	}
	tx.cat = x.cat
	return res, nil
}

// Commit makes the transaction's changes durable and visible.
func (tx *Tx) Commit() error {
	if tx.done {
		return ErrTxDone
	}
	tx.done = true
	defer tx.db.unlockWrite()
	c, err := tx.db.p.PrepareCommit(tx.v)
	if err != nil {
		return err
	}
	return tx.db.publish(c, func() (*Catalog, error) { return tx.cat, nil })
}

// Rollback discards the transaction's changes.
func (tx *Tx) Rollback() error {
	if tx.done {
		return ErrTxDone
	}
	tx.done = true
	tx.db.unlockWrite()
	return nil
}

// Conn is a session that understands BEGIN / COMMIT / ROLLBACK statements.
// It is not safe for concurrent use; use one Conn per goroutine.
type Conn struct {
	db *DB
	tx *Tx
}

// Conn returns a new session.
func (db *DB) Conn() *Conn { return &Conn{db: db} }

// InTx reports whether the session has an open transaction.
func (c *Conn) InTx() bool { return c.tx != nil }

// Close rolls back any open transaction.
func (c *Conn) Close() error {
	if c.tx != nil {
		err := c.tx.Rollback()
		c.tx = nil
		return err
	}
	return nil
}

// Exec runs one or more statements in the session.
func (c *Conn) Exec(src string, args ...any) (Result, error) {
	return c.ExecContext(context.Background(), src, args...)
}

// ExecContext is Exec with cancellation of the running statement.
func (c *Conn) ExecContext(ctx context.Context, src string, args ...any) (Result, error) {
	pr, err := c.db.parse(src)
	if err != nil {
		return Result{}, err
	}
	nargs, err := normalizeArgs(args, pr.nparams)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, s := range pr.stmts {
		if res, err = c.execStmt(ctx, s, nargs); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (c *Conn) execStmt(ctx context.Context, s sql.Stmt, args []any) (Result, error) {
	switch s.(type) {
	case *sql.Begin:
		if c.tx != nil {
			return Result{}, errors.New("bangdb: cannot start a transaction within a transaction")
		}
		tx, err := c.db.BeginContext(ctx)
		if err != nil {
			return Result{}, err
		}
		c.tx = tx
		return Result{}, nil
	case *sql.Commit:
		if c.tx == nil {
			return Result{}, errors.New("bangdb: cannot commit - no transaction is active")
		}
		tx := c.tx
		c.tx = nil
		return Result{}, tx.Commit()
	case *sql.Rollback:
		if c.tx == nil {
			return Result{}, errors.New("bangdb: cannot rollback - no transaction is active")
		}
		tx := c.tx
		c.tx = nil
		return Result{}, tx.Rollback()
	}
	if c.tx != nil {
		return c.tx.execStmt(ctx, s, args)
	}
	if isReadOnly(s) {
		ctx, cancel := c.db.stmtContext(ctx)
		defer cancel()
		return c.db.read(ctx, func(x *execCtx) (Result, error) { return x.exec(s) }, args)
	}
	tx, err := c.db.BeginContext(ctx)
	if err != nil {
		return Result{}, err
	}
	res, err := tx.execStmt(ctx, s, args)
	if err != nil {
		tx.Rollback()
		return Result{}, err
	}
	return res, tx.Commit()
}

func isReadOnly(s sql.Stmt) bool {
	switch s.(type) {
	case *sql.Select, *sql.Explain:
		return true
	}
	return false
}

// PageCount returns the number of pages in the committed database.
func (db *DB) PageCount() uint32 {
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
	return db.p.Header().PageCount
}
