package bangdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A transaction larger than TxMemory spills its changed pages to a temporary
// file; reads within the transaction, statement rollback, ROLLBACK, commit
// and reopening must all behave as without spilling.
func TestTxSpill(t *testing.T) {
	durableOnly(t)
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, "tmp")
			os.Mkdir(tmp, 0o755)
			opts := &Options{TxMemory: 64 << 10, TempDir: tmp} // 16 pages
			if encrypted {
				opts.EncryptionKey = make([]byte, 32)
			}
			path := filepath.Join(dir, "s.db")
			db, err := OpenWithOptions(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, k TEXT, v TEXT)")
			exec(t, db, "CREATE INDEX t_k ON t (k)")
			pad := strings.Repeat("x", 200)
			fill := func(tx *Tx, from, to int) {
				for i := from; i < to; i++ {
					if _, err := tx.Exec("INSERT INTO t VALUES (?, ?, ?)", i, fmt.Sprintf("k%05d", (i*7919)%5000), pad); err != nil {
						t.Fatal(err)
					}
				}
			}

			// A rolled back transaction leaves nothing behind.
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			fill(tx, 0, 3000)
			if s := db.Stats(); s.TxSpills == 0 {
				t.Fatal("the transaction did not spill")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			expect(t, db, "SELECT count(*) FROM t", "0")

			// A committed one: reads inside it see spilled pages; a failed
			// statement is undone even when its pages were spilled.
			if tx, err = db.Begin(); err != nil {
				t.Fatal(err)
			}
			fill(tx, 0, 5000)
			var n, nk int
			if err := tx.QueryRow("SELECT count(*), count(DISTINCT k) FROM t").Scan(&n, &nk); err != nil || n != 5000 || nk != 5000 {
				t.Fatalf("inside the transaction: %d %d %v", n, nk, err)
			}
			// Changes every row (spilling as it goes), then fails on the last
			// one with a duplicate primary key.
			if _, err := tx.Exec("UPDATE t SET v = 'moved', k = 'm' || k, id = CASE WHEN id = 4999 THEN 0 ELSE id END"); err == nil {
				t.Fatal("conflicting update succeeded")
			}
			if _, err := tx.Exec("UPDATE t SET v = 'changed' WHERE id % 2 = 0"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("DELETE FROM t WHERE id % 5 = 0"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			check := func(db *DB) {
				t.Helper()
				expect(t, db, "SELECT count(*) FROM t", "4000")
				expect(t, db, "SELECT count(*) FROM t WHERE v = 'changed'", "2000")
				expect(t, db, "SELECT count(*) FROM t WHERE v = 'moved'", "0")
				expect(t, db, "SELECT count(*) FROM t WHERE k >= 'k'", "4000") // through the index
				expect(t, db, "PRAGMA integrity_check", "ok")
			}
			check(db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = OpenWithOptions(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			check(db)
			if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
				t.Fatalf("spill files left behind: %v", ents)
			}
		})
	}
}

// Cumulative statistics do not go backwards when VACUUM replaces the file.
func TestStatsSurviveVacuum(t *testing.T) {
	durableOnly(t)
	db, err := Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "CREATE TABLE t (v INT)")
	for i := 0; i < 50; i++ {
		exec(t, db, "INSERT INTO t VALUES (?)", i)
	}
	before := db.Stats()
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := db.Stats()
	if after.Commits < before.Commits || after.WALSyncs < before.WALSyncs || after.PageReads+after.PageCacheHits < before.PageReads+before.PageCacheHits {
		t.Fatalf("counters went backwards: %+v -> %+v", before, after)
	}
}
