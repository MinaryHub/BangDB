package bangdb

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCachesUnderConcurrency runs the same queries from many goroutines
// (sharing cached plans and decoded nodes) while a writer updates rows and
// changes indexes, which invalidates plans.
func TestCachesUnderConcurrency(t *testing.T) {
	db, err := OpenWithOptions(filepath.Join(t.TempDir(), "c.db"), &Options{CacheSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, k TEXT UNIQUE, v INT, w INT)`)
	err = db.Update(func(tx *Tx) error {
		for i := 1; i <= 3000; i++ {
			if _, err := tx.Exec(`INSERT INTO t VALUES (?, ?, 0, 0)`, i, fmt.Sprintf("k%05d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var reads atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				id := (i*7+g*13)%3000 + 1
				var v, w int
				var err error
				switch i % 3 {
				case 0:
					err = db.QueryRow(`SELECT v, w FROM t WHERE id = ?`, id).Scan(&v, &w)
				case 1:
					err = db.QueryRow(`SELECT v, w FROM t WHERE k = ?`, fmt.Sprintf("k%05d", id)).Scan(&v, &w)
				default:
					err = db.QueryRow(`SELECT sum(v), sum(w) FROM t WHERE v >= 0 AND id BETWEEN ? AND ?`, id, id+50).Scan(&v, &w)
				}
				if err == nil && v != w {
					err = fmt.Errorf("torn read: v=%d w=%d", v, w)
				}
				if err != nil {
					errs <- err
					return
				}
				reads.Add(1)
			}
		}(g)
	}
	deadline := time.Now().Add(2 * time.Second)
	for n := 0; time.Now().Before(deadline); n++ {
		exec(t, db, `UPDATE t SET v = v + 1, w = w + 1 WHERE id % 7 = ?`, n%7)
		switch n % 10 {
		case 3:
			exec(t, db, `CREATE INDEX t_v ON t (v)`)
		case 8:
			exec(t, db, `DROP INDEX t_v`)
		}
	}
	stop.Store(true)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if reads.Load() < 100 {
		t.Fatalf("only %d reads", reads.Load())
	}
	checkIntegrity(t, db)
}
