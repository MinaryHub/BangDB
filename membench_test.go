package bangdb

import (
	"fmt"
	"testing"
)

// Benchmarks for in-memory databases (ROADMAP phase 4).

// Each benchmark runs on a :memory: database twice: with the default
// B+tree tables (pages) and with USING memory tables (Go values).
var memKinds = []string{"btree", "memory"}

func memBenchDB(b *testing.B, kind string, rows int) *DB {
	db, err := Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score INT) USING ` + kind); err != nil {
		b.Fatal(err)
	}
	db.Exec(`CREATE INDEX t_name ON t (name)`)
	db.Update(func(tx *Tx) error {
		for i := 0; i < rows; i++ {
			tx.Exec(`INSERT INTO t (name, score) VALUES (?, ?)`, fmt.Sprintf("user%07d", i), i%100)
		}
		return nil
	})
	return db
}

func BenchmarkMemInsertInTx(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 0)
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
		})
	}
}

func BenchmarkMemInsertAutocommit(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 0)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := db.Exec(`INSERT INTO t (name, score) VALUES (?, ?)`, fmt.Sprintf("u%d", i), i); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemUpdateAutocommit(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 100000)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := db.Exec(`UPDATE t SET score = score + 1 WHERE id = ?`, i%100000+1); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemPointLookupPK(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 100000)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var name string
				if err := db.QueryRow(`SELECT name FROM t WHERE id = ?`, i%100000+1).Scan(&name); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemPointLookupIndex(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 100000)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var id int64
				if err := db.QueryRow(`SELECT id FROM t WHERE name = ?`, fmt.Sprintf("user%07d", i%100000)).Scan(&id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemRangeScan(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 100000)
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
		})
	}
}

func BenchmarkMemFullScanAggregate(b *testing.B) {
	for _, kind := range memKinds {
		b.Run(kind, func(b *testing.B) {
			db := memBenchDB(b, kind, 100000)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var n, s int64
				if err := db.QueryRow(`SELECT count(*), sum(score) FROM t`).Scan(&n, &s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
