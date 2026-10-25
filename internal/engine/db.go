// Package engine ties storage, catalog and SQL execution together.
package engine

import (
	"errors"
	"fmt"
	"sync"
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
// transaction. Readers see the last committed state (snapshot isolation for
// the duration of a statement); they are only paused while a commit is being
// applied.
type DB struct {
	p           *pager.Pager
	writeSem    chan struct{} // capacity 1: the single-writer lock
	commitMu    sync.RWMutex  // readers hold R, commit holds W
	catMu       sync.Mutex
	cat         *Catalog // committed catalog (immutable)
	busyTimeout time.Duration

	cacheMu   sync.Mutex
	stmtCache map[string]parsed

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
		p = pager.OpenMemory()
	} else {
		var err error
		if p, err = pager.Open(path); err != nil {
			return nil, err
		}
	}
	db := &DB{p: p, writeSem: make(chan struct{}, 1), busyTimeout: 5 * time.Second,
		stmtCache: map[string]parsed{}, closed: make(chan struct{})}
	if opts != nil && opts.BusyTimeout > 0 {
		db.busyTimeout = opts.BusyTimeout
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
		if lerr := db.lockWrite(); lerr != nil {
			err = fmt.Errorf("bangdb: close: %w", lerr)
			return
		}
		close(db.closed)
		db.commitMu.Lock()
		err = db.p.Close()
		db.commitMu.Unlock()
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

func (db *DB) lockWrite() error {
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
	}
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
	c := db.Conn()
	res, err := c.Exec(src, args...)
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

// Begin starts an explicit write transaction. It blocks (up to the busy
// timeout) while another write transaction is active.
func (db *DB) Begin() (*Tx, error) {
	if db.isClosed() {
		return nil, pager.ErrClosed
	}
	if err := db.lockWrite(); err != nil {
		return nil, err
	}
	if db.isClosed() {
		db.unlockWrite()
		return nil, pager.ErrClosed
	}
	return &Tx{db: db, v: db.p.NewView(), cat: db.committedCatalog(), rowids: map[string]int64{}}, nil
}

// read runs fn against the committed state.
func (db *DB) read(fn func(x *execCtx) (Result, error), args []any) (Result, error) {
	db.commitMu.RLock()
	defer db.commitMu.RUnlock()
	if db.isClosed() {
		return Result{}, pager.ErrClosed
	}
	x := &execCtx{v: db.p.NewView(), cat: db.committedCatalog(), args: args}
	return fn(x)
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
		if res, err = tx.execStmt(s, nargs); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Query is Exec; it exists for readability at call sites.
func (tx *Tx) Query(src string, args ...any) (Result, error) { return tx.Exec(src, args...) }

func (tx *Tx) execStmt(s sql.Stmt, args []any) (Result, error) {
	sp := tx.v.Savepoint()
	cat := tx.cat
	rowids := make(map[string]int64, len(tx.rowids))
	for k, v := range tx.rowids {
		rowids[k] = v
	}
	x := &execCtx{v: tx.v, cat: tx.cat, tx: tx, args: args}
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
	tx.db.commitMu.Lock()
	defer tx.db.commitMu.Unlock()
	if err := tx.db.p.Commit(tx.v); err != nil {
		return err
	}
	tx.db.catMu.Lock()
	tx.db.cat = tx.cat
	tx.db.catMu.Unlock()
	return nil
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
		if res, err = c.execStmt(s, nargs); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (c *Conn) execStmt(s sql.Stmt, args []any) (Result, error) {
	switch s.(type) {
	case *sql.Begin:
		if c.tx != nil {
			return Result{}, errors.New("bangdb: cannot start a transaction within a transaction")
		}
		tx, err := c.db.Begin()
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
		return c.tx.execStmt(s, args)
	}
	if isReadOnly(s) {
		return c.db.read(func(x *execCtx) (Result, error) { return x.exec(s) }, args)
	}
	tx, err := c.db.Begin()
	if err != nil {
		return Result{}, err
	}
	res, err := tx.execStmt(s, args)
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
func (db *DB) PageCount() uint32 { return db.p.NewView().PageCount() }
