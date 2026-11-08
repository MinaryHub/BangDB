package engine

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/minaryhub/bangdb/internal/pager"
	"github.com/minaryhub/bangdb/internal/value"
)

// Stats describes the activity of a database since it was opened. The
// embedded pager statistics cover storage: commits, WAL, checkpoints,
// caches and open snapshots.
type Stats struct {
	pager.Stats

	Statements  uint64 // statements executed (each statement of a script counts)
	Queries     uint64 // of which read-only (SELECT, EXPLAIN)
	Errors      uint64 // statements that failed
	SlowQueries uint64 // calls slower than Options.SlowQueryThreshold

	TxCommits   uint64        // write transactions committed (explicit or per statement)
	TxRollbacks uint64        // write transactions rolled back
	CommitTime  time.Duration // total time spent committing (including the WAL fsync)

	LockWaits    uint64        // writers that had to wait for the write lock
	LockWaitTime time.Duration // total time they waited
	BusyErrors   uint64        // waits that ended in ErrBusy

	OpenCursors int // streaming result sets not yet closed
}

type dbCounters struct {
	statements, queries, errors, slow atomic.Uint64
	commits, rollbacks                atomic.Uint64
	commitNanos                       atomic.Int64
	lockWaits, busy                   atomic.Uint64
	lockWaitNanos                     atomic.Int64
}

// SlowQuery describes a call that took longer than Options.SlowQueryThreshold.
type SlowQuery struct {
	SQL      string
	Args     []any
	Duration time.Duration // for streamed queries: until the result was read to the end or closed
	Err      error
}

// Stats returns the database's statistics.
func (db *DB) Stats() Stats {
	db.swapMu.RLock()
	s := Stats{Stats: db.p.Stats()}
	db.swapMu.RUnlock()
	c := &db.counters
	s.Statements, s.Queries, s.Errors, s.SlowQueries = c.statements.Load(), c.queries.Load(), c.errors.Load(), c.slow.Load()
	s.TxCommits, s.TxRollbacks = c.commits.Load(), c.rollbacks.Load()
	s.CommitTime = time.Duration(c.commitNanos.Load())
	s.LockWaits, s.BusyErrors = c.lockWaits.Load(), c.busy.Load()
	s.LockWaitTime = time.Duration(c.lockWaitNanos.Load())
	db.cursors.mu.Lock()
	s.OpenCursors = len(db.cursors.m)
	db.cursors.mu.Unlock()
	return s
}

// countStmt records one executed statement.
func (db *DB) countStmt(readOnly bool, err error) {
	db.counters.statements.Add(1)
	if readOnly {
		db.counters.queries.Add(1)
	}
	if err != nil {
		db.counters.errors.Add(1)
	}
}

// finishCall reports a call that took longer than the slow query threshold.
func (db *DB) finishCall(src string, args []any, start time.Time, err error) {
	if db.slowThreshold <= 0 {
		return
	}
	d := time.Since(start)
	if d < db.slowThreshold {
		return
	}
	db.counters.slow.Add(1)
	q := SlowQuery{SQL: src, Args: args, Duration: d, Err: err}
	if db.onSlow != nil {
		db.onSlow(q)
		return
	}
	log.Printf("bangdb: slow query (%s): %s%s", d.Round(time.Microsecond), src, formatArgs(args))
}

func formatArgs(args []any) string {
	if len(args) == 0 {
		return ""
	}
	s := " ["
	for i, a := range args {
		if i > 0 {
			s += ", "
		}
		if v, err := value.Normalize(a); err == nil {
			s += value.Format(v)
		} else {
			s += fmt.Sprint(a)
		}
	}
	return s + "]"
}
