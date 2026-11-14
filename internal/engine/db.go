// Package engine ties storage, catalog and SQL execution together.
package engine

import (
	"context"
	dbsql "database/sql"
	"errors"
	"fmt"
	"strings"
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
	// SortMemory is the memory one query may use for ORDER BY, DISTINCT and
	// GROUP BY before it spills to temporary files. Default: 64 MiB;
	// negative: never spill.
	SortMemory int
	// TempDir holds the spill files. Default: os.TempDir().
	TempDir string
	// TxMemory is the memory a write transaction may use for the pages it
	// changed; beyond it they move to a temporary file in TempDir, so one
	// transaction can change more data than fits in memory. Default:
	// 64 MiB; negative: never spill.
	TxMemory int
	// CacheSize is the memory for cached file pages and decoded B+tree
	// nodes. Default: 32 MiB.
	CacheSize int
	// SlowQueryThreshold, if positive, reports every call (Exec, Query,
	// ...) that takes at least this long to OnSlowQuery, or to the standard
	// logger when OnSlowQuery is nil.
	SlowQueryThreshold time.Duration
	// OnSlowQuery receives slow calls. It runs on the calling goroutine and
	// must not use the database.
	OnSlowQuery func(SlowQuery)
	// EncryptionKey encrypts the database file and its WAL (AES-256-XTS
	// per page; see package pager). Use 32 random bytes, kept outside the
	// database. A new file is created encrypted; an existing file must
	// have been created with the same key. Backups and VACUUM keep the
	// key; BackupWithKey makes a copy with another key or none. Ignored
	// for in-memory databases.
	EncryptionKey []byte
}

// DefaultCacheSize is the default Options.CacheSize.
const DefaultCacheSize = 32 << 20

// Result is the outcome of executing SQL.
type Result struct {
	Columns      []string
	Types        []string // declared column types where known ("" otherwise)
	Rows         [][]any
	RowsAffected int64
	LastInsertID int64
}

// DB is an open database. It is safe for concurrent use.
//
// Concurrency: any number of readers run in parallel with at most one write
// transaction. Each statement outside a transaction reads a pager snapshot of
// the latest commit, so readers and writers never wait for each other (a
// reader only takes the pager lock briefly). Commits are made durable in
// groups: a committing writer releases the write lock before its WAL fsync,
// so concurrent writers share fsyncs.
type DB struct {
	p        *pager.Pager
	writeSem chan struct{} // capacity 1: the single-writer lock
	// swapMu guards the pager pointer: readers hold R for a statement,
	// replacing or closing the pager takes W.
	swapMu       sync.RWMutex
	catMu        sync.Mutex
	cat          *Catalog // catalog of the latest prepared commit (immutable); readers use their snapshot's
	busyTimeout  time.Duration
	queryTimeout time.Duration
	spill        *spillConfig
	txSpillPages int // pager spill limit (Options.TxMemory)
	cacheSize    int
	plans        planCache

	cacheMu   sync.Mutex
	stmtCache map[string]parsed

	path     string // "" for in-memory
	readOnly atomic.Bool
	cursors  cursorSet
	repl     *replLog

	key           []byte // encryption key of the file (nil: plaintext)
	commitWaiter  atomic.Pointer[func(seq uint64)]
	noMemSave     bool // Close must not write the memory table snapshot
	counters      dbCounters
	slowThreshold time.Duration
	onSlow        func(SlowQuery)

	closeOnce sync.Once
	closed    chan struct{}
}

type parsed struct {
	stmts []sql.Stmt
	names []string // per argument slot: placeholder name or ""
}

// Open opens a database file. The path ":memory:" (or "") creates a private
// in-memory database.
func Open(path string, opts *Options) (*DB, error) {
	var p *pager.Pager
	var key []byte
	if path == "" || path == ":memory:" {
		path = ""
		p = pager.OpenMemory()
	} else {
		if opts != nil && len(opts.EncryptionKey) > 0 {
			key = append([]byte(nil), opts.EncryptionKey...)
		}
		var err error
		if p, err = pager.OpenEncrypted(path, key); err != nil {
			return nil, err
		}

	}
	db := &DB{p: p, writeSem: make(chan struct{}, 1), busyTimeout: 5 * time.Second,
		stmtCache: map[string]parsed{}, closed: make(chan struct{}), path: path, key: key}
	db.repl = newReplLog(p.Header().Seq)
	if opts != nil && opts.BusyTimeout > 0 {
		db.busyTimeout = opts.BusyTimeout
	}
	db.spill = &spillConfig{limit: DefaultSortMemory}
	db.cacheSize = DefaultCacheSize
	if opts != nil && opts.CacheSize > 0 {
		db.cacheSize = opts.CacheSize
	}
	if opts != nil {
		db.queryTimeout = opts.QueryTimeout
		db.slowThreshold, db.onSlow = opts.SlowQueryThreshold, opts.OnSlowQuery
		db.spill.dir = opts.TempDir
		if opts.SortMemory != 0 {
			db.spill.limit = opts.SortMemory
		}
		db.txSpillPages = txSpillPages(opts.TxMemory)
	}
	db.configurePager()
	cat, err := loadCatalog(p.NewView())
	if err != nil {
		p.Close()
		return nil, err
	}
	db.cat = cat
	p.SetMeta(cat)
	if err := db.loadMemoryTables(); err != nil {
		db.noMemSave = true // keep the file that could not be loaded
		db.Close()
		return nil, err
	}
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
		var merr error
		if !db.noMemSave {
			merr = db.saveMemoryTables()
		}
		close(db.closed)
		db.cursors.cancelAll() // open cursors release their snapshots
		db.swapMu.Lock()
		err = errors.Join(merr, db.p.Close())
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
	start := time.Now()
	db.counters.lockWaits.Add(1)
	defer func() { db.counters.lockWaitNanos.Add(int64(time.Since(start))) }()
	t := time.NewTimer(db.busyTimeout)
	defer t.Stop()
	select {
	case db.writeSem <- struct{}{}:
		return nil
	case <-t.C:
		db.counters.busy.Add(1)
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
	stmts, names, err := sql.ParseNamed(src)
	if err != nil {
		return parsed{}, err
	}
	pr = parsed{stmts, names}
	db.cacheMu.Lock()
	if len(db.stmtCache) >= 512 {
		clear(db.stmtCache)
	}
	db.stmtCache[src] = pr
	db.cacheMu.Unlock()
	return pr, nil
}

// NamedArg binds a value to a named placeholder (:name, @name or $name).
type NamedArg struct {
	Name  string
	Value any
}

// normalizeArgs binds args to the argument slots of a statement: positional
// arguments fill the slots in order, named ones (NamedArg or database/sql's
// sql.NamedArg) go to the slot of their placeholder.
// positionalOnly reports whether every parameter is positional and no
// argument is named (the common case, handled without bookkeeping).
func positionalOnly(args []any, names []string) bool {
	for _, n := range names {
		if n != "" {
			return false
		}
	}
	for _, a := range args {
		switch a.(type) {
		case NamedArg, dbsql.NamedArg:
			return false
		}
	}
	return true
}

func normalizeArgs(args []any, names []string) ([]any, error) {
	if len(args) == len(names) && positionalOnly(args, names) {
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
	out := make([]any, len(names))
	set := make([]bool, len(names))
	pos := 0
	for _, a := range args {
		name, val, named := "", a, false
		switch na := a.(type) {
		case NamedArg:
			name, val, named = na.Name, na.Value, true
		case dbsql.NamedArg:
			name, val, named = na.Name, na.Value, true
		}
		v, err := value.Normalize(val)
		if err != nil {
			return nil, err
		}
		if !named {
			if pos >= len(names) {
				return nil, fmt.Errorf("bangdb: too many arguments: the statement has %d parameters", len(names))
			}
			out[pos], set[pos] = v, true
			pos++
			continue
		}
		name = strings.TrimLeft(name, ":@$")
		found := false
		for i, n := range names {
			if n != "" && strings.EqualFold(n, name) {
				out[i], set[i], found = v, true, true
			}
		}
		if !found {
			return nil, fmt.Errorf("bangdb: the statement has no parameter named %s", name)
		}
	}
	for i, ok := range set {
		if !ok {
			if names[i] != "" {
				return nil, fmt.Errorf("bangdb: missing argument for parameter :%s", names[i])
			}
			return nil, fmt.Errorf("bangdb: expected %d arguments, got %d", len(names), len(args))
		}
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
			err = errScriptTx
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
	snap, err := db.p.Snapshot()
	if err != nil {
		return Result{}, err
	}
	defer snap.Release()
	x := &execCtx{v: snap.View(), cat: snap.Meta().(*Catalog), args: args, ctx: ctx, spill: db.spill, plans: &db.plans}
	return fn(x)
}

// prepared records the catalog of a prepared commit (which carries it as
// its meta) as the base of the next write transaction. The caller holds the
// write lock.
func (db *DB) prepared(cat *Catalog) {
	db.catMu.Lock()
	db.cat = cat
	db.catMu.Unlock()
}

// maybeCheckpoint runs a checkpoint when the WAL is large and the write lock
// is free (a busy writer will do it later). A failed checkpoint does not
// undo any commit; it is retried and a broken file is reported by the next
// operation.
func (db *DB) maybeCheckpoint() {
	if !db.p.NeedsCheckpoint() {
		return
	}
	select {
	case db.writeSem <- struct{}{}:
		if !db.isClosed() {
			db.p.Checkpoint()
		}
		db.unlockWrite()
	default:
	}
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
	// deferFK skips foreign key checks of new and changed rows (ImportSQL
	// checks every row at the end).
	deferFK bool
	// closureDirty holds, per closure, the child nodes whose edges changed
	// in the running statement (applied when it finishes).
	closureDirty map[string]map[string]any
	// rawClosures lets statements write closure tables (ForceClosureWrite).
	rawClosures bool
}

// ForceClosureWrite lets the transaction's statements write closure tables
// directly and stops maintaining them, for repairs and tests. Changes made
// this way are not checked: IntegrityCheck reports a closure that no longer
// matches its source, and DROP CLOSURE + CREATE CLOSURE rebuilds it.
func (tx *Tx) ForceClosureWrite(on bool) { tx.rawClosures = on }

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
	start := time.Now()
	res, err := tx.execScript(ctx, src, args)
	tx.db.finishCall(src, args, start, err)
	return res, err
}

func (tx *Tx) execScript(ctx context.Context, src string, args []any) (Result, error) {
	pr, err := tx.db.parse(src)
	if err != nil {
		return Result{}, err
	}
	nargs, err := normalizeArgs(args, pr.names)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, s := range pr.stmts {
		switch s.(type) {
		case *sql.Begin, *sql.Commit, *sql.Rollback:
			return res, errors.New("bangdb: use Tx.Commit / Tx.Rollback instead of transaction statements")
		case *sql.Vacuum:
			return res, errors.New("bangdb: cannot VACUUM from within a transaction")
		}
		res, err = tx.execStmt(ctx, s, nargs)
		tx.db.countStmt(isReadOnly(s), err)
		if err != nil {
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
	x := &execCtx{v: tx.v, cat: tx.cat, tx: tx, args: args, ctx: ctx, spill: tx.db.spill, closureWrite: tx.rawClosures}
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
	start := time.Now()
	defer func() { tx.db.counters.commitNanos.Add(int64(time.Since(start))) }()
	tx.db.counters.commits.Add(1)
	p := tx.db.p // stays valid after unlocking (VACUUM may swap db.p)
	c, err := p.PrepareCommit(tx.v, tx.cat)
	if err != nil || c == nil {
		tx.db.unlockWrite()
		return err
	}
	tx.db.prepared(tx.cat)
	// Group commit: release the write lock before waiting for the fsync, so
	// the next writer can prepare and share it.
	tx.db.unlockWrite()
	if err := p.Flush(c); err != nil {
		return err
	}
	tx.db.maybeCheckpoint()
	if w := tx.db.commitWaiter.Load(); w != nil {
		(*w)(c.Seq())
	}
	return nil
}

// Rollback discards the transaction's changes.
func (tx *Tx) Rollback() error {
	if tx.done {
		return ErrTxDone
	}
	tx.done = true
	tx.db.counters.rollbacks.Add(1)
	tx.v.Discard()
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
	start := time.Now()
	res, err := c.execScript(ctx, src, args)
	c.db.finishCall(src, args, start, err)
	return res, err
}

var errScriptTx = errors.New("bangdb: script left a transaction open; it was rolled back (use DB.Begin or Conn for multi-call transactions)")

func (c *Conn) execScript(ctx context.Context, src string, args []any) (Result, error) {
	pr, err := c.db.parse(src)
	if err != nil {
		return Result{}, err
	}
	return c.execParsed(ctx, pr, args)
}

func (c *Conn) execParsed(ctx context.Context, pr parsed, args []any) (Result, error) {
	nargs, err := normalizeArgs(args, pr.names)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, s := range pr.stmts {
		res, err = c.execStmt(ctx, s, nargs)
		if !isTxControl(s) {
			c.db.countStmt(isReadOnly(s), err)
		}
		if err != nil {
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
	case *sql.Vacuum:
		if c.tx != nil {
			return Result{}, errors.New("bangdb: cannot VACUUM from within a transaction")
		}
		return Result{}, c.db.Vacuum(ctx)
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

func isTxControl(s sql.Stmt) bool {
	switch s.(type) {
	case *sql.Begin, *sql.Commit, *sql.Rollback:
		return true
	}
	return false
}

func isReadOnly(s sql.Stmt) bool {
	switch st := s.(type) {
	case *sql.Select, *sql.Explain:
		return true
	case *sql.Pragma:
		return st.Value == nil || st.Call
	}
	return false
}

// PageCount returns the number of pages in the committed database.
func (db *DB) PageCount() uint32 {
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
	return db.p.Header().PageCount
}

// WALPages returns the number of page versions not yet checkpointed.
func (db *DB) WALPages() int { return db.p.WALPages() }

// txSpillPages converts Options.TxMemory to the pager's spill limit.
func txSpillPages(mem int) int {
	switch {
	case mem == 0:
		return 0 // the default
	case mem < 0:
		return -1
	}
	return max(mem/pager.PageSize, 16)
}

// configurePager applies the database's settings to db.p (after Open, and
// when VACUUM or a replica snapshot installs a new file).
func (db *DB) configurePager() {
	db.p.SetOnCommit(db.repl.onCommit)
	db.p.SetCommitWantsPages(db.repl.wantsPages)
	db.p.SetCacheSize(db.cacheSize)
	db.p.SetSpill(db.spill.dir, db.txSpillPages)
}
