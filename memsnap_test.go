package bangdb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentMemoryTables(t *testing.T) {
	durableOnly(t)
	for _, key := range [][]byte{nil, bytes.Repeat([]byte{3}, 32)} {
		path := filepath.Join(t.TempDir(), "m.db")
		openDB := func() *DB {
			db, err := OpenWithOptions(path, &Options{EncryptionKey: key})
			if err != nil {
				t.Fatal(err)
			}
			return db
		}
		db := openDB()
		exec(t, db, `CREATE TABLE cache (k TEXT PRIMARY KEY, v BLOB, hits INT) USING memory WITH (persist = true);
			CREATE INDEX cache_hits ON cache (hits);
			CREATE TABLE scratch (x INT) USING memory;
			CREATE TABLE durable (id INTEGER PRIMARY KEY, s TEXT)`)
		exec(t, db, "INSERT INTO cache VALUES ('a', x'0102', 5), ('b', NULL, 7), ('secret-key', 'SECRETVALUE', 1)")
		exec(t, db, "INSERT INTO scratch VALUES (1); INSERT INTO durable (s) VALUES ('d')")
		seq := db.Seq()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path + "-mem"); err != nil {
			t.Fatal(err)
		} else if key != nil && bytes.Contains(b, []byte("SECRETVALUE")) {
			t.Fatal("the snapshot of an encrypted database is plaintext")
		}

		db = openDB()
		expect(t, db, "SELECT k, hex(v), hits FROM cache ORDER BY k", "a|0102|5;b||7;secret-key|53454352455456414C5545|1")
		expect(t, db, "SELECT k FROM cache WHERE hits > 4 ORDER BY hits", "a;b") // the index was restored
		expect(t, db, "SELECT count(*) FROM scratch", "0")                       // not persistent
		expect(t, db, "SELECT s FROM durable", "d")
		if db.Seq() != seq {
			t.Fatalf("restoring changed the commit sequence: %d -> %d", seq, db.Seq())
		}
		if _, err := os.Stat(path + "-mem"); !os.IsNotExist(err) {
			t.Fatal("the snapshot file was not removed after loading")
		}
		exec(t, db, "UPDATE cache SET hits = hits + 1 WHERE k = 'a'")
		db.Close()

		// A snapshot older than the database is ignored (here: a commit
		// made after the snapshot, as when a crash is recovered).
		db = openDB()
		expect(t, db, "SELECT hits FROM cache WHERE k = 'a'", "6")
		db.Close()
		stale, _ := os.ReadFile(path + "-mem")
		db = openDB()
		exec(t, db, "INSERT INTO durable (s) VALUES ('later')")
		db.Close()
		os.WriteFile(path+"-mem", stale, 0o644)
		db = openDB()
		expect(t, db, "SELECT count(*) FROM cache", "0")
		db.Close()
	}
	db := open(t)
	if _, err := db.Exec("CREATE TABLE bad (x) USING memory WITH (persist = maybe)"); err == nil {
		t.Error("bad persist value accepted")
	}
	if _, err := db.Exec("CREATE TABLE bad (x) USING memory WITH (other = 1)"); err == nil {
		t.Error("unknown memory option accepted")
	}
}
