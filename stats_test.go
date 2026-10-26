package bangdb

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	db, err := OpenWithOptions(filepath.Join(t.TempDir(), "s.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	for i := 0; i < 20; i++ {
		exec(t, db, "INSERT INTO t (v) VALUES (?)", "x")
	}
	query(t, db, "SELECT count(*) FROM t")
	if _, err := db.Exec("SELECT * FROM nope"); err == nil {
		t.Fatal("expected an error")
	}
	tx, _ := db.Begin()
	tx.Exec("INSERT INTO t (v) VALUES ('y')")
	tx.Rollback()

	s := db.Stats()
	if s.Statements != 24 || s.Queries != 2 || s.Errors != 1 {
		t.Errorf("statements=%d queries=%d errors=%d", s.Statements, s.Queries, s.Errors)
	}
	if s.TxCommits != 21 || s.TxRollbacks != 1 || s.CommitTime <= 0 {
		t.Errorf("commits=%d rollbacks=%d time=%v", s.TxCommits, s.TxRollbacks, s.CommitTime)
	}
	if s.Commits != 21 || s.WALSyncs == 0 || s.WALSyncs > s.Commits || s.WALPages == 0 || s.WALBytes == 0 {
		t.Errorf("pager: %+v", s.Stats)
	}
	if s.Seq != db.Seq() || s.PageCount == 0 {
		t.Errorf("seq=%d pages=%d", s.Seq, s.PageCount)
	}
	if s.NodeHits == 0 {
		t.Errorf("no node cache hits: %+v", s.Stats)
	}
	if err := db.Engine().Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if s := db.Stats(); s.Checkpoints != 1 || s.CheckpointPages == 0 || s.WALPages != 0 {
		t.Errorf("after checkpoint: %+v", s.Stats)
	}

	// Open cursors and snapshots.
	for i := 0; i < 2000; i++ {
		exec(t, db, "INSERT INTO t (v) VALUES ('z')")
	}
	rows, err := db.Query("SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	exec(t, db, "INSERT INTO t (v) VALUES ('w')")
	if s := db.Stats(); s.OpenCursors != 1 || s.Snapshots != 1 || s.SnapshotLag != 1 {
		t.Errorf("cursors=%d snapshots=%d lag=%d", s.OpenCursors, s.Snapshots, s.SnapshotLag)
	}
	rows.Close()
	if s := db.Stats(); s.OpenCursors != 0 || s.Snapshots != 0 {
		t.Errorf("after close: cursors=%d snapshots=%d", s.OpenCursors, s.Snapshots)
	}
}

func TestStatsLockWaits(t *testing.T) {
	db, err := OpenWithOptions(":memory:", &Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	tx, _ := db.Begin()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		db.Exec("INSERT INTO t VALUES (1)") // waits, then fails with ErrBusy
	}()
	wg.Wait()
	tx.Rollback()
	s := db.Stats()
	if s.LockWaits != 1 || s.BusyErrors != 1 || s.LockWaitTime < 40*time.Millisecond {
		t.Errorf("waits=%d busy=%d time=%v", s.LockWaits, s.BusyErrors, s.LockWaitTime)
	}
}

func TestSlowQueryHook(t *testing.T) {
	var mu sync.Mutex
	var got []SlowQuery
	db, err := OpenWithOptions(":memory:", &Options{SlowQueryThreshold: time.Nanosecond, OnSlowQuery: func(q SlowQuery) {
		mu.Lock()
		got = append(got, q)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)")
	exec(t, db, "INSERT INTO t (v) VALUES (?)", 7)
	db.Exec("SELECT * FROM missing")
	rows, _ := db.QueryContext(context.Background(), "SELECT * FROM t WHERE v = ?", 7)
	rows.All()
	tx, _ := db.Begin()
	tx.Exec("UPDATE t SET v = 8")
	tx.Commit()
	mu.Lock()
	defer mu.Unlock()
	var sqls []string
	for _, q := range got {
		sqls = append(sqls, q.SQL)
		if q.Duration <= 0 {
			t.Errorf("%s: duration %v", q.SQL, q.Duration)
		}
	}
	want := "CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)|INSERT INTO t (v) VALUES (?)|SELECT * FROM missing|SELECT * FROM t WHERE v = ?|UPDATE t SET v = 8"
	if strings.Join(sqls, "|") != want {
		t.Fatalf("slow queries:\n%s\nwant\n%s", strings.Join(sqls, "|"), want)
	}
	if got[1].Args[0] != 7 || got[2].Err == nil || got[0].Err != nil {
		t.Errorf("args/err: %+v", got)
	}
	if db.Stats().SlowQueries != 5 {
		t.Errorf("SlowQueries = %d", db.Stats().SlowQueries)
	}
}

func TestExplainAnalyze(t *testing.T) {
	db := setupShop(t)
	out := query(t, db, `EXPLAIN ANALYZE SELECT u.name, count(*) FROM users u JOIN orders o ON o.user_id = u.id
		GROUP BY u.name ORDER BY 2 DESC`)
	re := regexp.MustCompile(`^SCAN users \(loops=1 scanned=(\d+) rows=(\d+)\);  SEARCH orders .*\(loops=(\d+) scanned=(\d+) rows=(\d+)\);AGGREGATE;SORT;RESULT rows=(\d+) time=\S+$`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("unexpected output: %s", out)
	}
	users := query(t, db, "SELECT count(*) FROM users")
	if m[1] != users || m[2] != users || m[3] != users {
		t.Errorf("level 1: %v (users %s)", m[1:4], users)
	}
	if orders := query(t, db, "SELECT count(*) FROM orders WHERE user_id IN (SELECT id FROM users)"); m[5] != orders {
		t.Errorf("joined rows %s, orders %s", m[5], orders)
	}
	groups := query(t, db, "SELECT count(DISTINCT user_id) FROM orders WHERE user_id IN (SELECT id FROM users)")
	if m[6] != groups {
		t.Errorf("result rows %s, want %s", m[6], groups)
	}
	if _, err := db.Query("EXPLAIN ANALYZE DELETE FROM users"); err == nil || !strings.Contains(err.Error(), "only SELECT") {
		t.Errorf("EXPLAIN ANALYZE DELETE: %v", err)
	}
	expect(t, db, "SELECT count(*) FROM users", users) // nothing was deleted
	out = query(t, db, "EXPLAIN ANALYZE SELECT id FROM users WHERE id = 1 UNION SELECT user_id FROM orders")
	if !strings.Contains(out, ";UNION;") || !strings.HasSuffix(strings.Split(out, ";RESULT")[0], ")") {
		t.Errorf("compound: %s", out)
	}
}
