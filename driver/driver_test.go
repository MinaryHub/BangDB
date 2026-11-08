package driver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/minaryhub/bangdb/driver"
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

func TestContextCancellation(t *testing.T) {
	db, _ := sql.Open("bangdb", ":memory:")
	defer db.Close()
	db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	tx, _ := db.Begin()
	for i := 0; i < 3000; i++ {
		tx.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprint(i))
	}
	tx.Commit()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t a JOIN t b ON a.v < b.v`).Scan(&n)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestReturningThroughDriver(t *testing.T) {
	db, _ := sql.Open("bangdb", ":memory:")
	defer db.Close()
	db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE)`)
	var id int64
	if err := db.QueryRow(`INSERT INTO t (name) VALUES (?) RETURNING id`, "a").Scan(&id); err != nil || id != 1 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if err := db.QueryRow(`INSERT INTO t (name) VALUES (?) ON CONFLICT (name) DO UPDATE SET name = excluded.name RETURNING id`, "a").Scan(&id); err != nil || id != 1 {
		t.Fatalf("upsert id=%d err=%v", id, err)
	}
}

func TestDriverNamedParamsAndColumnTypes(t *testing.T) {
	db, _ := sql.Open("bangdb", ":memory:")
	defer db.Close()
	db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name VARCHAR(20), score REAL, data BLOB)`)
	if _, err := db.Exec(`INSERT INTO t (name, score) VALUES (:name, :score)`, sql.Named("name", "kim"), sql.Named("score", 1.5)); err != nil {
		t.Fatal(err)
	}
	stmt, err := db.Prepare(`SELECT id, name, score, data, score * 2 FROM t WHERE name = ?`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.Query(); err == nil {
		t.Fatal("NumInput should make database/sql reject a missing argument")
	}
	rows, err := stmt.Query("kim")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cts, _ := rows.ColumnTypes()
	var got []string
	for _, ct := range cts {
		got = append(got, ct.DatabaseTypeName()+"/"+ct.ScanType().String())
	}
	want := "INTEGER/int64 VARCHAR(20)/string REAL/float64 BLOB/[]uint8 /interface {}"
	if s := fmt.Sprint(strings.Join(got, " ")); s != want {
		t.Fatalf("column types %q, want %q", s, want)
	}
}

func TestDecimalThroughDatabaseSQL(t *testing.T) {
	db, err := sql.Open("bangdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`CREATE TABLE m (amount DECIMAL(10,2))`)
	if _, err := db.Exec(`INSERT INTO m VALUES (?), (?)`, "0.10", 0.2); err != nil {
		t.Fatal(err)
	}
	var s string
	var f float64
	if err := db.QueryRow(`SELECT sum(amount), sum(amount) FROM m`).Scan(&s, &f); err != nil {
		t.Fatal(err)
	}
	if s != "0.30" || f != 0.3 {
		t.Fatalf("sum: %q %v", s, f)
	}
}
