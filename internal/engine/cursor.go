package engine

import (
	"context"
	"runtime"
	"sync"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
)

// Rows is a streaming result set. Rows are produced on demand from a pinned
// snapshot, so memory use does not grow with the result size for queries
// without ORDER BY / GROUP BY / DISTINCT. A Rows must be closed (iterating to
// the end closes it automatically); an open Rows pins its snapshot, which
// prevents WAL checkpoints past it.
type Rows struct {
	cols []string

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
func RowsFromResult(res Result) *Rows { return &Rows{cols: res.Columns, static: res.Rows} }

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
	nargs, err := normalizeArgs(args, pr.nparams)
	if err != nil {
		return nil, err
	}
	return c.db.stream(ctx, sel, nargs)
}

// QueryRows is Conn.QueryRows on a fresh session.
func (db *DB) QueryRows(ctx context.Context, src string, args ...any) (*Rows, error) {
	return db.Conn().QueryRows(ctx, src, args...)
}

func (db *DB) stream(ctx context.Context, sel *sql.Select, args []any) (*Rows, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stopTimeout := db.stmtContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	cancelAll := func() { cancel(); stopTimeout() }

	db.swapMu.RLock()
	if db.isClosed() {
		db.swapMu.RUnlock()
		cancelAll()
		return nil, pager.ErrClosed
	}
	db.pubMu.Lock()
	snap, err := db.p.Snapshot()
	cat := db.committedCatalog()
	db.pubMu.Unlock()
	if err != nil {
		db.swapMu.RUnlock()
		cancelAll()
		return nil, err
	}
	release := func() {
		snap.Release()
		db.swapMu.RUnlock()
	}
	x := &execCtx{v: snap.View(), cat: cat, args: args, ctx: ctx}
	run, err := x.prepareSelect(sel)
	if err != nil {
		release()
		cancelAll()
		return nil, err
	}

	ch := make(chan []any, 64)
	errc := make(chan error, 1)
	done := make(chan struct{})
	r := &Rows{cols: run.p.cols, ch: ch, errc: errc, cancel: cancelAll, done: done}
	r.unreg = db.cursors.add(cancelAll)
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
		errc <- err
		close(ch)
		cancelAll()
	}()
	// Safety net for a Rows dropped without Close: the producer goroutine
	// only references the channels, so r can be collected.
	runtime.SetFinalizer(r, (*Rows).Close)
	return r, nil
}
