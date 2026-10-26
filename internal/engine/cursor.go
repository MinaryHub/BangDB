package engine

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
)

// Rows is a streaming result set. Rows are produced on demand from a pinned
// snapshot, so memory use does not grow with the result size for queries
// without ORDER BY / GROUP BY / DISTINCT. A Rows must be closed (iterating to
// the end closes it automatically); an open Rows pins its snapshot, which
// prevents WAL checkpoints past it.
type Rows struct {
	cols  []string
	types []string

	// streaming mode
	ch     chan []any
	errc   chan error
	cancel context.CancelFunc
	done   chan struct{}

	// materialized mode
	static [][]any

	row    []any
	err    error
	closed bool
	unreg  func()
}

// Columns returns the result column names.
func (r *Rows) Columns() []string { return r.cols }

// ColumnTypes returns the declared type of each result column, or "" where
// it is not a plain table column.
func (r *Rows) ColumnTypes() []string {
	if len(r.types) == len(r.cols) {
		return r.types
	}
	return make([]string, len(r.cols))
}

// Next advances to the next row; it returns false at the end or on error
// (see Err).
func (r *Rows) Next() bool {
	if r.closed {
		return false
	}
	if r.ch == nil {
		if len(r.static) == 0 {
			r.Close()
			return false
		}
		r.row, r.static = r.static[0], r.static[1:]
		return true
	}
	row, ok := <-r.ch
	if !ok {
		r.err = <-r.errc
		r.Close()
		return false
	}
	r.row = row
	return true
}

// Row returns the current row.
func (r *Rows) Row() []any { return r.row }

// Err returns the error that ended iteration, if any.
func (r *Rows) Err() error { return r.err }

// Close stops the query and releases its snapshot. It is safe to call more
// than once.
func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.row = nil
	if r.ch != nil {
		r.cancel()
		for range r.ch { // let the producer finish
		}
		<-r.done
	}
	if r.unreg != nil {
		r.unreg()
	}
	runtime.SetFinalizer(r, nil)
	return nil
}

// RowsFromResult wraps a materialized result.
func RowsFromResult(res Result) *Rows {
	return &Rows{cols: res.Columns, types: res.Types, static: res.Rows}
}

// cursors tracks open streaming cursors so Close can stop them.
type cursorSet struct {
	mu   sync.Mutex
	next int
	m    map[int]context.CancelFunc
}

func (s *cursorSet) add(cancel context.CancelFunc) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[int]context.CancelFunc{}
	}
	id := s.next
	s.next++
	s.m[id] = cancel
	return func() {
		s.mu.Lock()
		delete(s.m, id)
		s.mu.Unlock()
	}
}

func (s *cursorSet) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.m {
		c()
	}
}

// QueryRows runs a query and returns a cursor. A single SELECT outside a
// transaction is streamed; anything else is executed and materialized.
func (c *Conn) QueryRows(ctx context.Context, src string, args ...any) (*Rows, error) {
	return c.query(ctx, src, args, 0)
}

// QueryFirst is QueryRows for callers that read at most one row: the query
// runs to its first row before returning, without a producer goroutine.
func (c *Conn) QueryFirst(ctx context.Context, src string, args ...any) (*Rows, error) {
	return c.query(ctx, src, args, 1)
}

func (c *Conn) query(ctx context.Context, src string, args []any, maxRows int) (*Rows, error) {
	pr, err := c.db.parse(src)
	if err != nil {
		return nil, err
	}
	if len(pr.stmts) != 1 || c.tx != nil {
		res, err := c.ExecContext(ctx, src, args...)
		if err != nil {
			return nil, err
		}
		return RowsFromResult(res), nil
	}
	sel, ok := pr.stmts[0].(*sql.Select)
	if !ok {
		res, err := c.ExecContext(ctx, src, args...)
		if err != nil {
			return nil, err
		}
		return RowsFromResult(res), nil
	}
	nargs, err := normalizeArgs(args, pr.names)
	if err != nil {
		return nil, err
	}
	return c.db.stream(ctx, sel, nargs, maxRows, src, args, nil)
}

// QueryRows is Conn.QueryRows on a fresh session.
func (db *DB) QueryRows(ctx context.Context, src string, args ...any) (*Rows, error) {
	return db.Conn().QueryRows(ctx, src, args...)
}

// QueryFirst is Conn.QueryFirst.
func (db *DB) QueryFirst(ctx context.Context, src string, args ...any) (*Rows, error) {
	return db.Conn().QueryFirst(ctx, src, args...)
}

// stream runs sel on a snapshot. Queries with a small, bounded result (and
// all of them when maxRows > 0, which stops after that many rows) run
// synchronously; others produce rows from a goroutine as they are read.
func (db *DB) stream(ctx context.Context, sel *sql.Select, args []any, maxRows int, src string, rawArgs []any, plans *planCache) (rows *Rows, err error) {
	if plans == nil {
		plans = &db.plans
	}
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	streamed := false
	defer func() {
		if !streamed {
			db.countStmt(true, err)
			db.finishCall(src, rawArgs, start, err)
		}
	}()
	ctx, stopTimeout := db.stmtContext(ctx)

	db.swapMu.RLock()
	if db.isClosed() {
		db.swapMu.RUnlock()
		stopTimeout()
		return nil, pager.ErrClosed
	}
	snap, err := db.p.Snapshot()
	if err != nil {
		db.swapMu.RUnlock()
		stopTimeout()
		return nil, err
	}
	x := &execCtx{v: snap.View(), cat: snap.Meta().(*Catalog), args: args, ctx: ctx, spill: db.spill, plans: plans}
	run, err := x.prepareSelect(sel)
	if err != nil {
		snap.Release()
		db.swapMu.RUnlock()
		stopTimeout()
		return nil, err
	}

	if maxRows > 0 || run.small() {
		r := &Rows{cols: run.p.cols, types: run.p.types}
		if maxRows == 1 {
			r.static = make([][]any, 0, 1)
		}
		err := x.runSelect(run, func(vals []any) (bool, error) {
			r.static = append(r.static, vals)
			return maxRows <= 0 || len(r.static) < maxRows, nil
		})
		snap.Release()
		db.swapMu.RUnlock()
		stopTimeout()
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	release := func() {
		snap.Release()
		db.swapMu.RUnlock()
	}

	// Streamed from a goroutine: Close and DB.Close must be able to stop it.
	ctx, cancel := context.WithCancel(ctx)
	cancelAll := func() { cancel(); stopTimeout() }
	x.ctx = ctx

	ch := make(chan []any, 64)
	errc := make(chan error, 1)
	done := make(chan struct{})
	r := &Rows{cols: run.p.cols, types: run.p.types, ch: ch, errc: errc, cancel: cancelAll, done: done}
	r.unreg = db.cursors.add(cancelAll)
	streamed = true
	go func() {
		defer close(done)
		defer release()
		err := x.runSelect(run, func(vals []any) (bool, error) {
			select {
			case ch <- vals:
				return true, nil
			case <-ctx.Done():
				return false, interrupted(ctx.Err())
			}
		})
		db.countStmt(true, err)
		db.finishCall(src, rawArgs, start, err)
		errc <- err
		close(ch)
		cancelAll()
	}()
	// Safety net for a Rows dropped without Close: the producer goroutine
	// only references the channels, so r can be collected.
	runtime.SetFinalizer(r, (*Rows).Close)
	return r, nil
}

// NumParams returns the number of argument slots of src and whether any of
// them is a named placeholder.
func (c *Conn) NumParams(src string) (int, bool, error) {
	pr, err := c.db.parse(src)
	if err != nil {
		return 0, false, err
	}
	for _, n := range pr.names {
		if n != "" {
			return len(pr.names), true, nil
		}
	}
	return len(pr.names), false, nil
}
