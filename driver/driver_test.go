package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/knoxxr/bangdb/driver"
)

func TestDatabaseSQL(t *testing.T) {
	db, err := sql.Open("bangdb", filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO kv VALUES (?, ?), (?, ?)`, "a", 1, "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("rows affected %d", n)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE kv SET v = v * 10`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	stmt, err := db.Prepare(`SELECT v FROM kv WHERE k = ?`)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := stmt.QueryRow("b").Scan(&v); err != nil || v != 2 {
		t.Fatalf("v=%d err=%v", v, err)
	}

	rows, err := db.Query(`SELECT k, v FROM kv ORDER BY k`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got = append(got, k)
	}
	if len(got) != 2 || got[0] != "a" {
		t.Fatalf("got %v", got)
	}
}

func TestMemoryIsSharedAcrossConnections(t *testing.T) {
	db, _ := sql.Open("bangdb", ":memory:")
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.Exec(`CREATE TABLE t (a INT)`)
	db.Exec(`INSERT INTO t VALUES (1)`)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
