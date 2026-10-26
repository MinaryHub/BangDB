package bangdb

import (
	"errors"
	"sync"
	"testing"
)

func TestPreparedStatements(t *testing.T) {
	db := open(t)
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, n INT)")
	ins, err := db.Prepare("INSERT INTO t (name, n) VALUES (:name, :n)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		r, err := ins.Exec(Named("name", string(rune('a'+i))), Named("n", i))
		if err != nil || r.RowsAffected != 1 || r.LastInsertID != int64(i+1) {
			t.Fatal(r, err)
		}
	}
	if _, err := ins.Exec(Named("name", "a"), Named("n", 1)); !errors.Is(err, ErrConstraint) {
		t.Fatalf("duplicate: %v", err)
	}
	get, err := db.Prepare("SELECT name, n FROM t WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= 20; i++ {
				var name string
				var n int
				if err := get.QueryRow(i).Scan(&name, &n); err != nil || n != i-1 {
					t.Error(i, name, n, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := get.QueryRow(999).Scan(new(string), new(int)); !errors.Is(err, ErrNoRows) {
		t.Fatalf("missing row: %v", err)
	}
	// The plan follows schema changes.
	exec(t, db, "ALTER TABLE t RENAME COLUMN n TO m")
	if err := get.QueryRow(1).Scan(new(string), new(int)); err == nil {
		t.Fatal("prepared statement still uses a dropped column")
	}
	sel, _ := db.Prepare("SELECT count(*) FROM t WHERE m >= ?")
	rows, err := sel.Query(10)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	if rows.Values()[0] != int64(10) {
		t.Fatalf("count %v", rows.Values())
	}
	rows.Close()
	if _, err := db.Prepare("SELEC 1"); err == nil {
		t.Fatal("syntax error not reported by Prepare")
	}
	script, _ := db.Prepare("BEGIN; INSERT INTO t (name) VALUES ('zz')")
	if _, err := script.Exec(); err == nil {
		t.Fatal("a prepared script may not leave a transaction open")
	}
	expect(t, db, "SELECT count(*) FROM t WHERE name = 'zz'", "0")
}
