package bangdb

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// File format compatibility: testdata/compat holds databases written by
// earlier releases (format 3, and format 4 = encrypted). Every later build
// must open them, read the same data, pass the integrity check and accept
// writes. Never regenerate an existing fixture: add a new one when the
// format changes (BANGDB_WRITE_FIXTURES=1 writes missing ones).
var compatKey = bytes.Repeat([]byte{0x42}, 32)

const compatSchema = `
PRAGMA user_version = 3;
CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL COLLATE NOCASE UNIQUE, age INT CHECK (age >= 0), data BLOB);
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT NOT NULL REFERENCES users ON DELETE CASCADE, amount REAL, note TEXT DEFAULT 'none');
CREATE INDEX orders_user ON orders (user_id, amount);
CREATE TABLE metrics (ts INTEGER NOT NULL, host TEXT, v REAL) USING timeseries WITH (time_column = ts, block_rows = 16);
CREATE VIEW totals AS SELECT user_id, sum(amount) AS total FROM orders GROUP BY user_id;
INSERT INTO users (name, age, data) VALUES ('kim', 31, x'00ff'), ('Lee', 25, NULL), ('park', NULL, x'');
DELETE FROM users WHERE name = 'park';
INSERT INTO orders (user_id, amount) VALUES (1, 12.5), (1, 2.25), (2, 1200);
`

// compatQueries and their results as of the fixture's release.
var compatQueries = [][2]string{
	{"SELECT id, name, age, hex(data) FROM users ORDER BY id", "1|kim|31|00FF;2|Lee|25|"},
	{"SELECT * FROM orders ORDER BY id", "1|1|12.5|none;2|1|2.25|none;3|2|1200|none"},
	{"SELECT * FROM totals ORDER BY user_id", "1|14.75;2|1200"},
	{"SELECT count(*), min(ts), max(ts), round(sum(v), 2) FROM metrics", "40|1000|1039|780"},
	{"SELECT name FROM users WHERE name = 'LEE'", "Lee"},
	{"PRAGMA user_version", "3"},
}

func writeCompatFixture(t *testing.T, path string, key []byte) {
	db, err := OpenWithOptions(path, &Options{EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, compatSchema)
	for i := 0; i < 40; i++ {
		exec(t, db, "INSERT INTO metrics VALUES (?, ?, ?)", 1000+i, "h", float64(i))
	}
	if err := db.Engine().Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	os.Remove(path + "-wal")
}

func TestFormatCompatibility(t *testing.T) {
	durableOnly(t)
	for _, f := range []struct {
		name string
		key  []byte
	}{{"format3.db", nil}, {"format4-encrypted.db", compatKey}} {
		t.Run(f.name, func(t *testing.T) {
			src := filepath.Join("testdata", "compat", f.name)
			if _, err := os.Stat(src); os.IsNotExist(err) {
				if os.Getenv("BANGDB_WRITE_FIXTURES") == "" {
					t.Fatalf("missing fixture %s (run with BANGDB_WRITE_FIXTURES=1 once)", src)
				}
				writeCompatFixture(t, src, f.key)
			}
			// Work on a copy: opening may recover or write.
			path := filepath.Join(t.TempDir(), f.name)
			in, _ := os.Open(src)
			out, _ := os.Create(path)
			io.Copy(out, in)
			in.Close()
			out.Close()
			db, err := OpenWithOptions(path, &Options{EncryptionKey: f.key})
			if err != nil {
				t.Fatalf("cannot open a file of an earlier release: %v", err)
			}
			defer db.Close()
			for _, q := range compatQueries {
				expect(t, db, q[0], q[1])
			}
			checkIntegrity(t, db)
			exec(t, db, "INSERT INTO users (name) VALUES ('new')")
			expect(t, db, "SELECT id FROM users WHERE name = 'new'", "4") // AUTOINCREMENT state kept
			exec(t, db, "DELETE FROM users WHERE id = 1")
			expect(t, db, "SELECT count(*) FROM orders", "1") // FK action kept
			var buf bytes.Buffer
			if err := db.Dump(context.Background(), &buf); err != nil {
				t.Fatal(err)
			}
		})
	}
}
