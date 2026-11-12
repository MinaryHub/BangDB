package bangdb

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// explainHas fails unless the plan of q mentions want.
func explainHas(t *testing.T, db *DB, q, want string, args ...any) {
	t.Helper()
	if plan := query(t, db, "EXPLAIN "+q, args...); !strings.Contains(plan, want) {
		t.Errorf("%s\n plan: %s\n want: %s", q, plan, want)
	}
}

func explainLacks(t *testing.T, db *DB, q, unwanted string, args ...any) {
	t.Helper()
	if plan := query(t, db, "EXPLAIN "+q, args...); strings.Contains(plan, unwanted) {
		t.Errorf("%s\n plan: %s\n must not use: %s", q, plan, unwanted)
	}
}

func TestExpressionIndex(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, Email TEXT, first TEXT, last TEXT, age INT, doc TEXT);
		INSERT INTO users (email, first, last, age, doc) VALUES
			('Kim@X.io', 'min', 'kim', 31, '{"city":"seoul","n":1}'),
			('lee@x.io', 'ha', 'lee', 25, '{"city":"busan","n":2}'),
			('PARK@x.io', 'jun', 'park', 40, '{"city":"seoul","n":3.0}'),
			(NULL, 'su', 'choi', NULL, NULL);
		CREATE UNIQUE INDEX users_email_lower ON users (lower(email));
		CREATE INDEX users_name ON users (last || ', ' || first);
		CREATE INDEX users_city ON users (json_extract(doc, '$.city'), age);
		CREATE INDEX users_n ON users (doc ->> '$.n');
		CREATE INDEX users_len ON users (length(last), id);`)

	// The index is used when the condition is written like its expression,
	// whatever the case of names, with or without the table's name or alias.
	explainHas(t, db, `SELECT id FROM users WHERE lower(email) = 'kim@x.io'`, "users_email_lower")
	explainHas(t, db, `SELECT id FROM users u WHERE LOWER(u.EMAIL) = ?`, "users_email_lower", "x")
	explainHas(t, db, `SELECT id FROM users WHERE 'kim@x.io' = lower(users.email)`, "users_email_lower")
	explainLacks(t, db, `SELECT id FROM users WHERE upper(email) = 'KIM@X.IO'`, "USING INDEX")
	explainLacks(t, db, `SELECT id FROM users WHERE lower(email) = 'a' COLLATE NOCASE`, "users_email_lower")
	expect(t, db, `SELECT id FROM users WHERE lower(email) = 'kim@x.io'`, "1")
	expect(t, db, `SELECT id FROM users WHERE lower(email) = 'KIM@x.io'`, "")
	expect(t, db, `SELECT id FROM users WHERE lower(email) IN ('park@x.io', 'lee@x.io', 'none') ORDER BY id`, "2;3")
	// A text expression is keyed in order: ranges and ORDER BY use it.
	explainHas(t, db, `SELECT id FROM users WHERE lower(email) >= 'l' AND lower(email) < 'p'`, "users_email_lower RANGE")
	expect(t, db, `SELECT id FROM users WHERE lower(email) >= 'l' AND lower(email) < 'p'`, "2")
	explainHas(t, db, `SELECT id FROM users ORDER BY lower(email) LIMIT 2`, "ordered by index")
	expect(t, db, `SELECT id FROM users ORDER BY lower(email) LIMIT 3`, "4;1;2") // NULL first
	expect(t, db, `SELECT id FROM users ORDER BY lower(email) DESC LIMIT 2`, "3;2")
	explainHas(t, db, `SELECT id FROM users WHERE last || ', ' || first = 'kim, min'`, "users_name")
	expect(t, db, `SELECT id FROM users WHERE last || ', ' || first = 'kim, min'`, "1")
	// An expression with a column after it.
	explainHas(t, db, `SELECT id FROM users WHERE json_extract(doc, '$.city') = 'seoul' AND age > 35`, "users_city RANGE")
	expect(t, db, `SELECT id FROM users WHERE json_extract(doc, '$.city') = 'seoul' AND age > 35`, "3")
	expect(t, db, `SELECT id FROM users WHERE json_extract(doc, '$.city') = 'seoul' ORDER BY id`, "1;3")
	// An expression of unknown type: equality and IN only; 3 finds 3.0.
	explainHas(t, db, `SELECT id FROM users WHERE doc ->> '$.n' = 3`, "users_n")
	expect(t, db, `SELECT id FROM users WHERE doc ->> '$.n' = 3`, "3")
	expect(t, db, `SELECT id FROM users WHERE doc ->> '$.n' = 2.0`, "2")
	expect(t, db, `SELECT id FROM users WHERE doc ->> '$.n' IN (1, 2) ORDER BY id`, "1;2")
	explainLacks(t, db, `SELECT id FROM users WHERE doc ->> '$.n' > 1`, "users_n")
	expect(t, db, `SELECT id FROM users WHERE doc ->> '$.n' > 1 ORDER BY id`, "2;3")
	explainLacks(t, db, `SELECT id FROM users ORDER BY doc ->> '$.n' LIMIT 1`, "users_n")
	// An integer expression, then the rowid.
	explainHas(t, db, `SELECT id FROM users WHERE length(last) = 3 ORDER BY id DESC`, "users_len")
	expect(t, db, `SELECT id FROM users WHERE length(last) = 3 ORDER BY id DESC`, "2;1")
	expect(t, db, `SELECT id FROM users WHERE length(last) BETWEEN 4 AND 9 ORDER BY id`, "3;4")
	// Joins look the expression up per outer row.
	exec(t, db, `CREATE TABLE logins (addr TEXT); INSERT INTO logins VALUES ('park@x.io'), ('kim@x.io'), ('nobody@x.io')`)
	explainHas(t, db, `SELECT u.id FROM logins l JOIN users u ON lower(u.email) = l.addr`, "users_email_lower")
	expect(t, db, `SELECT u.id FROM logins l JOIN users u ON lower(u.email) = l.addr ORDER BY u.id`, "1;3")

	// Uniqueness is on the expression's value; NULLs do not conflict.
	if _, err := db.Exec(`INSERT INTO users (email) VALUES ('KIM@x.IO')`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate lower(email): %v", err)
	}
	exec(t, db, `INSERT INTO users (email, last) VALUES (NULL, 'x')`)
	if _, err := db.Exec(`UPDATE users SET email = 'LEE@X.IO' WHERE id = 1`); err == nil {
		t.Fatal("update to a duplicate lower(email) succeeded")
	}
	// Updates and deletes maintain the entries.
	exec(t, db, `UPDATE users SET email = 'Kim2@x.io', doc = '{"city":"jeju","n":9}' WHERE id = 1`)
	expect(t, db, `SELECT id FROM users WHERE lower(email) = 'kim@x.io'`, "")
	expect(t, db, `SELECT id FROM users WHERE lower(email) = 'kim2@x.io'`, "1")
	expect(t, db, `SELECT id FROM users WHERE json_extract(doc, '$.city') = 'jeju'`, "1")
	exec(t, db, `DELETE FROM users WHERE id = 3`)
	expect(t, db, `SELECT COUNT(*) FROM users WHERE json_extract(doc, '$.city') = 'seoul'`, "0")
	exec(t, db, `INSERT OR REPLACE INTO users (id, email) VALUES (7, 'LEE@x.io')`) // replaces row 2
	expect(t, db, `SELECT id FROM users WHERE lower(email) = 'lee@x.io'`, "7")
	exec(t, db, `INSERT INTO users (email) VALUES ('lee@X.io') ON CONFLICT DO NOTHING`)
	expect(t, db, `SELECT COUNT(*) FROM users WHERE lower(email) = 'lee@x.io'`, "1")

	// A partial expression index.
	exec(t, db, `CREATE INDEX adults ON users (upper(last)) WHERE age >= 30`)
	explainHas(t, db, `SELECT id FROM users WHERE upper(last) = 'KIM' AND age >= 30`, "adults")
	explainLacks(t, db, `SELECT id FROM users WHERE upper(last) = 'KIM'`, "adults")
	expect(t, db, `SELECT id FROM users WHERE upper(last) = 'KIM' AND age >= 30`, "1")

	// Schema changes.
	exec(t, db, `ALTER TABLE users RENAME COLUMN email TO mail`)
	explainHas(t, db, `SELECT id FROM users WHERE lower(mail) = 'kim2@x.io'`, "users_email_lower")
	expect(t, db, `SELECT id FROM users WHERE lower(mail) = 'kim2@x.io'`, "1")
	if _, err := db.Exec(`ALTER TABLE users DROP COLUMN mail`); err == nil || !strings.Contains(err.Error(), "users_email_lower") {
		t.Fatalf("dropping a column used by an index expression: %v", err)
	}
	exec(t, db, `ALTER TABLE users ADD COLUMN note TEXT DEFAULT 'n'`)
	exec(t, db, `CREATE INDEX users_note ON users (upper(note))`) // rows written before the column existed
	expect(t, db, `SELECT COUNT(*) FROM users WHERE upper(note) = 'N'`, "4")
	exec(t, db, `ANALYZE`)
	expect(t, db, `SELECT id FROM users WHERE lower(mail) = 'kim2@x.io'`, "1")
	if got := query(t, db, `PRAGMA index_info(users_city)`); got != "0|-2|NULL;1|4|age" {
		t.Errorf("index_info: %s", got)
	}
	if desc := db.Engine().Catalog().Describe("users"); !strings.Contains(desc, "CREATE UNIQUE INDEX users_email_lower ON users (LOWER(mail));") ||
		!strings.Contains(desc, `(JSON_EXTRACT(doc, '$.city'), age)`) {
		t.Errorf("describe:\n%s", desc)
	}
	exec(t, db, `DROP INDEX users_n`)
	expect(t, db, `SELECT id FROM users WHERE doc ->> '$.n' = 9`, "1")

	// A value the expression cannot be computed for is rejected, and so is
	// an index over rows that have one.
	exec(t, db, `CREATE TABLE m (a, b); CREATE INDEX m_sum ON m (a + b)`)
	exec(t, db, `INSERT INTO m VALUES (1, 2), (1.5, 1.5), (NULL, 1)`)
	if _, err := db.Exec(`INSERT INTO m VALUES ('x', 1)`); err == nil || !strings.Contains(err.Error(), "m_sum") {
		t.Fatalf("row the index expression fails for: %v", err)
	}
	expect(t, db, `SELECT COUNT(*) FROM m WHERE a + b = 3`, "2")
	explainHas(t, db, `SELECT COUNT(*) FROM m WHERE a + b = 3`, "m_sum")
	exec(t, db, `CREATE TABLE m2 (a); INSERT INTO m2 VALUES ('x')`)
	if _, err := db.Exec(`CREATE INDEX m2_i ON m2 (a + 1)`); err == nil {
		t.Fatal("index over a row its expression fails for")
	}

	for _, q := range []string{
		`CREATE INDEX bad ON users (lower(nope))`,
		`CREATE INDEX bad ON users (lower(mail), lower(mail))`,
		`CREATE INDEX bad ON users ((SELECT 1))`,
		`CREATE INDEX bad ON users (count(mail))`,
		`CREATE INDEX bad ON users (mail || ?)`,
		`CREATE INDEX bad ON users (datetime('now'))`,
		`CREATE INDEX bad ON users (date())`,
		`CREATE INDEX bad ON users (current_timestamp)`,
		`CREATE INDEX bad ON users (mail COLLATE NOCASE)`,
		`CREATE INDEX bad ON users (users.mail)`,
		`CREATE INDEX bad ON users (lower(rowid))`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}
}

func TestGeneratedColumns(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			price DECIMAL(10,2),
			qty INT DEFAULT 1,
			total DECIMAL(12,2) GENERATED ALWAYS AS (price * qty) STORED,
			slug TEXT AS (lower(replace(name, ' ', '-'))) UNIQUE,
			label AS (name || ' #' || id) VIRTUAL,
			CHECK (total >= 0)
		);
		CREATE INDEX items_total ON items (total);`)
	// INSERT without a column list gives the ordinary columns only.
	exec(t, db, `INSERT INTO items VALUES (1, 'Red Pen', 1.50, 4)`)
	exec(t, db, `INSERT INTO items (name, price) VALUES ('Blue Ink', 12.25)`) // the id is assigned, then used
	expect(t, db, `SELECT id, total, slug, label FROM items ORDER BY id`, "1|6.00|red-pen|Red Pen #1;2|12.25|blue-ink|Blue Ink #2")
	// Updates recompute; the index on the generated column follows.
	exec(t, db, `UPDATE items SET qty = 10, name = 'Red Pencil' WHERE id = 1`)
	expect(t, db, `SELECT total, slug, label FROM items WHERE id = 1`, "15.00|red-pencil|Red Pencil #1")
	explainHas(t, db, `SELECT id FROM items WHERE total > 13`, "items_total")
	expect(t, db, `SELECT id FROM items WHERE total > 13`, "1")
	expect(t, db, `SELECT id FROM items WHERE slug = 'blue-ink'`, "2")
	// RETURNING and UPSERT see the computed values.
	expect(t, db, `INSERT INTO items (name, price, qty) VALUES ('Cap', 2, 3) RETURNING total, slug`, "6.00|cap")
	expect(t, db, `INSERT INTO items (id, name, price) VALUES (3, 'x', 1)
		ON CONFLICT (id) DO UPDATE SET qty = items.qty + 1 RETURNING qty, total`, "4|8.00")
	// Constraints on generated values.
	if _, err := db.Exec(`INSERT INTO items (name, price) VALUES ('BLUE ink', 1)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate generated slug: %v", err)
	}
	if _, err := db.Exec(`UPDATE items SET price = -1 WHERE id = 2`); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("CHECK on a generated column: %v", err)
	}
	// They cannot be assigned.
	for _, q := range []string{
		`INSERT INTO items (name, total) VALUES ('a', 1)`,
		`UPDATE items SET slug = 'x'`,
		`INSERT INTO items (id, name) VALUES (1, 'q') ON CONFLICT (id) DO UPDATE SET total = 1`,
		`INSERT INTO items VALUES (9, 'a', 1, 1, 1, 'a', 'a')`,
		`ALTER TABLE items ADD COLUMN g INT AS (qty * 2)`,
		`ALTER TABLE items DROP COLUMN qty`, // used by total
		`CREATE TABLE bad (a INT, b INT AS (b + 1))`,
		`CREATE TABLE bad (a INT, b INT AS (a + 1), c INT AS (b + 1))`,
		`CREATE TABLE bad (a INT, b INT AS (nope))`,
		`CREATE TABLE bad (a INT, b TEXT AS (datetime('now')))`,
		`CREATE TABLE bad (a INT, b INT AS (a) PRIMARY KEY)`,
		`CREATE TABLE bad (a INT, b INT DEFAULT 1 AS (a))`,
		`CREATE TABLE bad (b INT AS (1))`,
		`CREATE TABLE bad (a INT, b INT AS ((SELECT 1)))`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}
	// Triggers see the new generated value.
	exec(t, db, `CREATE TABLE audit (msg TEXT);
		CREATE TRIGGER items_au BEFORE UPDATE ON items BEGIN INSERT INTO audit VALUES (old.total || ' -> ' || new.total); END;`)
	exec(t, db, `UPDATE items SET qty = 2 WHERE id = 2`)
	expect(t, db, `SELECT msg FROM audit`, "12.25 -> 24.50")
	exec(t, db, `DROP TRIGGER items_au`)

	exec(t, db, `ALTER TABLE items RENAME COLUMN qty TO quantity`)
	exec(t, db, `UPDATE items SET quantity = 3 WHERE id = 2`)
	expect(t, db, `SELECT total FROM items WHERE id = 2`, "36.75")
	exec(t, db, `ALTER TABLE items DROP COLUMN label`)
	expect(t, db, `SELECT id, slug FROM items WHERE id = 1`, "1|red-pencil")
	if desc := db.Engine().Catalog().Describe("items"); !strings.Contains(desc, "total DECIMAL(12,2) GENERATED ALWAYS AS ((price * quantity)) STORED") {
		t.Errorf("describe:\n%s", desc)
	}
}

// Expression indexes and generated columns survive a dump and a reopen.
func TestStoredExpressionsPersist(t *testing.T) {
	durableOnly(t)
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `
		CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT, n INT, twice INT AS (n * 2), up TEXT AS (upper(name)));
		CREATE INDEX p_low ON p (lower(name), n);
		CREATE INDEX p_calc ON p ((n + 1) * 2);
		INSERT INTO p (name, n) VALUES ('Ab', 1), ('cD', 2), ('ef', 3);`)
	var dump bytes.Buffer
	if err := db.Dump(context.Background(), &dump); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	check := func(db *DB) {
		t.Helper()
		expect(t, db, `SELECT id, twice, up FROM p ORDER BY id`, "1|2|AB;2|4|CD;3|6|EF")
		explainHas(t, db, `SELECT id FROM p WHERE lower(name) = 'cd'`, "p_low")
		expect(t, db, `SELECT id FROM p WHERE lower(name) = 'cd'`, "2")
		explainHas(t, db, `SELECT id FROM p WHERE (n + 1) * 2 = 8`, "p_calc")
		expect(t, db, `SELECT id FROM p WHERE (n + 1) * 2 = 8`, "3")
		exec(t, db, `INSERT INTO p (name, n) VALUES ('GH', 4)`)
		expect(t, db, `SELECT twice, up FROM p WHERE lower(name) = 'gh'`, "8|GH")
		checkIntegrity(t, db)
	}
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	check(db)
	db.Close()

	db2 := open(t)
	if _, err := db2.ImportSQL(context.Background(), &dump); err != nil {
		t.Fatalf("import: %v\n%s", err, dump.String())
	}
	check(db2)
}
