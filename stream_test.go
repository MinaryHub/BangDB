package bangdb

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestStreamingMemoryIsBounded(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "m.db"))
	defer db.Close()
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	const n = 200000
	pad := strings.Repeat("p", 100)
	db.Update(func(tx *Tx) error {
		for i := 0; i < n; i++ {
			tx.Exec(`INSERT INTO t (v) VALUES (?)`, pad)
		}
		return nil
	})
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapInuse
	peak := uint64(0)
	rows, err := db.Query(`SELECT * FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		count++
		if count%20000 == 0 {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapInuse)
		}
	}
	if rows.Err() != nil || count != n {
		t.Fatalf("count %d err %v", count, rows.Err())
	}
	grown := int64(peak) - int64(base)
	t.Logf("heap growth while streaming %d rows: %d KB", n, grown>>10)
	if grown > 40<<20 { // a materialized result of this size needs >100 MB
		t.Fatalf("streaming used %d MB", grown>>20)
	}
}

func TestStreamingSeesSnapshot(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)`)
	for i := 0; i < 1000; i++ {
		exec(t, db, `INSERT INTO t (v) VALUES (?)`, i)
	}
	rows, _ := db.Query(`SELECT v FROM t`)
	seen := 0
	for rows.Next() {
		if seen%100 == 0 {
			exec(t, db, `INSERT INTO t (v) VALUES (-1)`) // commits while iterating
			exec(t, db, `DELETE FROM t WHERE v = ?`, 999-seen)
		}
		seen++
	}
	if seen != 1000 || rows.Err() != nil {
		t.Fatalf("iteration saw %d rows (err %v), want the snapshot's 1000", seen, rows.Err())
	}
}

func TestAbandonedCursors(t *testing.T) {
	db, _ := Open(":memory:")
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	db.Update(func(tx *Tx) error {
		for i := 0; i < 10000; i++ {
			tx.Exec(`INSERT INTO t VALUES (?)`, i+1)
		}
		return nil
	})
	early, _ := db.Query(`SELECT * FROM t`)
	early.Next()
	early.Close() // stops the producer

	_, _ = db.Query(`SELECT * FROM t`) // never read or closed
	done := make(chan error)
	go func() { done <- db.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hangs on an abandoned cursor")
	}
}

func TestOrderByLimitTopK(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, a INT, b TEXT)`)
	rng := rand.New(rand.NewSource(3))
	type rec struct {
		id int64
		a  int64
		b  string
	}
	var all []rec
	db.Update(func(tx *Tx) error {
		for i := 0; i < 5000; i++ {
			r := rec{int64(i + 1), int64(rng.Intn(50)), fmt.Sprintf("s%02d", rng.Intn(20))}
			all = append(all, r)
			tx.Exec(`INSERT INTO t (a, b) VALUES (?, ?)`, r.a, r.b)
		}
		return nil
	})
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].a != all[j].a {
			return all[i].a > all[j].a
		}
		return all[i].b < all[j].b
	})
	var want []string
	for _, r := range all[7 : 7+25] {
		want = append(want, fmt.Sprint(r.id))
	}
	expect(t, db, `SELECT id FROM t ORDER BY a DESC, b LIMIT 25 OFFSET 7`, strings.Join(want, ";"))
}
