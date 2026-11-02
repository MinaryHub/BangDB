package bangdb

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestLongReadDoesNotBlockCommits is the regression test for ROADMAP M2: a
// slow query must neither delay commits nor see them.
func TestLongReadDoesNotBlockCommits(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	const n = 2500
	db.Update(func(tx *Tx) error {
		for i := 0; i < n; i++ {
			tx.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprintf("v%05d", i))
		}
		return nil
	})

	done := make(chan struct{})
	var joinCount int
	var readTime time.Duration
	go func() {
		defer close(done)
		start := time.Now()
		// Non-equi self-join: n*n comparisons (cannot use a hash join).
		if err := db.QueryRow(`SELECT COUNT(*) FROM t a JOIN t b ON a.v < b.v`).Scan(&joinCount); err != nil {
			t.Error(err)
		}
		readTime = time.Since(start)
	}()
	time.Sleep(20 * time.Millisecond)

	var worst time.Duration
	commits := 0
	for {
		start := time.Now()
		exec(t, db, `INSERT INTO t (v) VALUES (?)`, fmt.Sprintf("new%d", commits))
		worst = max(worst, time.Since(start))
		commits++
		if commits >= 20 && (commits > 2000 || closed(done)) {
			break
		}
	}
	<-done
	if want := n * (n - 1) / 2; joinCount != want {
		t.Fatalf("long read counted %d pairs, want %d from its snapshot", joinCount, want)
	}
	if readTime < 200*time.Millisecond {
		t.Skipf("join finished too fast (%v) to test overlap", readTime)
	}
	// A blocked commit would wait for (most of) the whole read; allow for
	// slow fsyncs and pauses on shared CI machines, which have reached a
	// fifth of the read on Windows.
	if worst > max(150*time.Millisecond, readTime/2) {
		t.Fatalf("a commit waited %v while the long read (%v) ran", worst, readTime)
	}
	t.Logf("read %v, %d commits during it, worst commit %v", readTime, commits, worst)
	expect(t, db, `SELECT COUNT(*) FROM t`, fmt.Sprint(n+commits))
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
