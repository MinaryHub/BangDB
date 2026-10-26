package bangdb

import (
	"errors"
	"testing"
)

func TestUpsertAndInsertSelect(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE kv (k TEXT PRIMARY KEY, v INT, hits INT DEFAULT 0);
		CREATE TABLE counter (day TEXT, page TEXT, n INT, PRIMARY KEY (day, page));
		CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE, name TEXT)`)

	// ON CONFLICT DO UPDATE with excluded.*
	for i := 0; i < 3; i++ {
		exec(t, db, `INSERT INTO kv (k, v) VALUES ('a', ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v, hits = hits + 1`, i)
	}
	expect(t, db, `SELECT k, v, hits FROM kv`, "a|2|2")
	// DO UPDATE ... WHERE: only update when newer
	exec(t, db, `INSERT INTO kv (k, v) VALUES ('a', 1) ON CONFLICT (k) DO UPDATE SET v = excluded.v WHERE excluded.v > kv.v`)
	expect(t, db, `SELECT v FROM kv`, "2")
	// DO NOTHING and no target
	r := exec(t, db, `INSERT INTO kv (k, v) VALUES ('a', 9), ('b', 9) ON CONFLICT DO NOTHING`)
	if r.RowsAffected != 1 {
		t.Fatalf("DO NOTHING affected %d", r.RowsAffected)
	}
	expect(t, db, `SELECT k, v FROM kv ORDER BY k`, "a|2;b|9")
	// Composite-key counter
	for i := 0; i < 4; i++ {
		exec(t, db, `INSERT INTO counter VALUES ('mon', 'home', 1) ON CONFLICT (day, page) DO UPDATE SET n = n + 1`)
	}
	expect(t, db, `SELECT n FROM counter WHERE day = 'mon' AND page = 'home'`, "4")
	// Target that is not a key
	if _, err := db.Exec(`INSERT INTO kv VALUES ('a', 1, 0) ON CONFLICT (v) DO NOTHING`); err == nil {
		t.Fatal("ON CONFLICT on a non-unique column must fail")
	}
	// Conflict on a different key than the target still raises an error
	exec(t, db, `INSERT INTO users VALUES (1, 'a@x', 'A')`)
	if _, err := db.Exec(`INSERT INTO users VALUES (2, 'a@x', 'B') ON CONFLICT (id) DO NOTHING`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("unmatched conflict: %v", err)
	}

	// INSERT OR IGNORE / OR REPLACE
	exec(t, db, `INSERT OR IGNORE INTO users VALUES (1, 'z@x', 'ignored')`)
	expect(t, db, `SELECT name FROM users WHERE id = 1`, "A")
	exec(t, db, `INSERT INTO users VALUES (2, 'b@x', 'B')`)
	exec(t, db, `INSERT OR REPLACE INTO users VALUES (3, 'b@x', 'C')`) // replaces row 2 via email
	expect(t, db, `SELECT id, name FROM users ORDER BY id`, "1|A;3|C")

	// INSERT ... SELECT, including from the same table
	exec(t, db, `INSERT INTO kv (k, v) SELECT name, id FROM users WHERE id > 0`)
	expect(t, db, `SELECT COUNT(*) FROM kv`, "4")
	exec(t, db, `INSERT INTO kv (k, v) SELECT k || '2', v * 10 FROM kv`)
	expect(t, db, `SELECT COUNT(*) FROM kv`, "8")
	if _, err := db.Exec(`INSERT INTO kv (k) SELECT k, v FROM kv`); err == nil {
		t.Fatal("column count mismatch must fail")
	}
}

func TestReturning(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, n INT DEFAULT 7)`)
	expect(t, db, `INSERT INTO t (name) VALUES ('a'), ('b') RETURNING id, name, n * 2 AS dbl`, "1|a|14;2|b|14")
	rows, err := db.Query(`INSERT INTO t (name) VALUES ('c') RETURNING *`)
	if err != nil {
		t.Fatal(err)
	}
	if cols := rows.Columns(); len(cols) != 3 || cols[0] != "id" {
		t.Fatalf("columns %v", cols)
	}
	rows.Close()
	expect(t, db, `UPDATE t SET n = n + 1 WHERE id >= 2 RETURNING id, n`, "2|8;3|8")
	expect(t, db, `DELETE FROM t WHERE name = 'a' RETURNING rowid, name`, "1|a")
	expect(t, db, `INSERT INTO t (id, name) VALUES (2, 'x') ON CONFLICT (id) DO UPDATE SET name = excluded.name RETURNING id, name`, "2|x")

	var id int64
	if err := db.QueryRow(`INSERT INTO t (name) VALUES ('d') RETURNING id`).Scan(&id); err != nil || id != 4 {
		t.Fatalf("QueryRow RETURNING: %d %v", id, err)
	}
	// RETURNING is atomic with the statement: a failing row rolls back all.
	if _, err := db.Query(`INSERT INTO t (id, name) VALUES (10, 'ok'), (2, 'dup') RETURNING id`); err == nil {
		t.Fatal("expected conflict")
	}
	expect(t, db, `SELECT COUNT(*) FROM t WHERE id = 10`, "0")
}
