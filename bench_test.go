package bangdb

import (
	"fmt"
	"path/filepath"
	"testing"
)

func benchDB(b *testing.B, rows int) *DB {
	db, err := Open(filepath.Join(b.TempDir(), "b.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score INT)`)
	db.Exec(`CREATE INDEX t_name ON t (name)`)
	db.Update(func(tx *Tx) error {
		for i := 0; i < rows; i++ {
			tx.Exec(`INSERT INTO t (name, score) VALUES (?, ?)`, fmt.Sprintf("user%07d", i), i%100)
		}
		return nil
	})
	return db
}

// BenchmarkInsertInTx measures bulk inserts inside one transaction.
func BenchmarkInsertInTx(b *testing.B) {
	db := benchDB(b, 0)
	b.ResetTimer()
	tx, _ := db.Begin()
	for i := 0; i < b.N; i++ {
		if _, err := tx.Exec(`INSERT INTO t (name, score) VALUES (?, ?)`, fmt.Sprintf("u%d", i), i); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkInsertAutocommit measures durable single-row commits (fsync).
func BenchmarkInsertAutocommit(b *testing.B) {
	db := benchDB(b, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec(`INSERT INTO t (name, score) VALUES (?, ?)`, fmt.Sprintf("u%d", i), i); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPointLookupPK(b *testing.B) {
	db := benchDB(b, 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var name string
		if err := db.QueryRow(`SELECT name FROM t WHERE id = ?`, i%100000+1).Scan(&name); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPointLookupIndex(b *testing.B) {
	db := benchDB(b, 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var id int64
		if err := db.QueryRow(`SELECT id FROM t WHERE name = ?`, fmt.Sprintf("user%07d", i%100000)).Scan(&id); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFullScanAggregate100k(b *testing.B) {
	db := benchDB(b, 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.Query(`SELECT score, COUNT(*) FROM t GROUP BY score`)
		if err != nil {
			b.Fatal(err)
		}
		if all, err := rows.All(); err != nil || len(all) != 100 {
			b.Fatalf("%d groups: %v", len(all), err)
		}
	}
}

// BenchmarkRangeScan reads 100 consecutive rows through an index.
func BenchmarkRangeScan(b *testing.B) {
	db := benchDB(b, 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.Query(`SELECT id, score FROM t WHERE name >= ? ORDER BY name LIMIT 100`, fmt.Sprintf("user%07d", i%99000))
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 100 {
			b.Fatalf("%d rows", n)
		}
	}
}
