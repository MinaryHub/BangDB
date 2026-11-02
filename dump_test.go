package bangdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportSQLiteDump(t *testing.T) {
	db := open(t)
	f, err := os.Open("testdata/sqlite_dump.sql")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res, err := db.ImportSQL(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 7 || len(res.Skipped) != 3 { // BEGIN, sqlite_sequence cleanup, COMMIT
		t.Errorf("rows %d, skipped %q", res.Rows, res.Skipped)
	}
	expect(t, db, `SELECT id, customer, note, hex(data), "group" FROM orders ORDER BY id`,
		"1|Kim|it's; fine|00FF|a;2|lee|multi\nline||NULL") // hex(NULL) is ''
	expect(t, db, `SELECT * FROM totals ORDER BY order_id`, "1|119.00") // DECIMAL(10,2) is exact
	expect(t, db, `SELECT k, typeof(v) FROM kv ORDER BY k`, "x|real;y|text;z|null")
	expect(t, db, `SELECT customer FROM orders WHERE customer = 'KIM'`, "Kim") // COLLATE NOCASE kept
	// The dump's trigger came along and works.
	exec(t, db, `INSERT INTO orders (id, customer) VALUES (10, 'trig')`)
	expect(t, db, `SELECT note FROM orders WHERE id = 10`, "new")
	// The foreign key, the CHECK and the AUTOINCREMENT high-water mark came along.
	if _, err := db.Exec(`INSERT INTO "order item" (order_id, sku) VALUES (9, 'x')`); err == nil {
		t.Error("foreign key not enforced")
	}
	if _, err := db.Exec(`INSERT INTO "order item" (order_id, sku, qty) VALUES (1, 'x', 0)`); err == nil {
		t.Error("CHECK not enforced")
	}
	exec(t, db, `INSERT INTO "order item" (order_id, sku) VALUES (2, 'D')`)
	expect(t, db, `SELECT id, qty FROM "order item" WHERE sku = 'D'`, "4|1")
	exec(t, db, "DELETE FROM orders WHERE id = 1")
	expect(t, db, `SELECT count(*) FROM "order item"`, "1") // ON DELETE CASCADE
}

func TestImportSQLiteCLIStyle(t *testing.T) {
	db := open(t)
	script := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE child(id INTEGER PRIMARY KEY, p INT REFERENCES parent(id), s TEXT);
INSERT INTO child VALUES(1,1,replace('a\nb','\n',char(10)));
INSERT INTO child VALUES(2,NULL,'x');
CREATE TABLE IF NOT EXISTS "parent" ("id" INTEGER PRIMARY KEY, "na""me" TEXT) STRICT;
INSERT INTO parent VALUES(1,'p');
CREATE VIEW v2 AS SELECT * FROM v1;
CREATE VIEW v1 AS SELECT s FROM child;
ANALYZE sqlite_schema;
INSERT INTO sqlite_stat1 VALUES('child','x','1');
COMMIT;`
	res, err := db.ImportSQL(context.Background(), strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 3 {
		t.Errorf("rows = %d", res.Rows)
	}
	expect(t, db, `SELECT s FROM v2 ORDER BY 1`, "a\nb;x")
	expect(t, db, `SELECT "na""me" FROM parent`, "p")

	// Everything or nothing: a broken foreign key fails the whole import.
	db2 := open(t)
	_, err = db2.ImportSQL(context.Background(), strings.NewReader(`
		CREATE TABLE p (id INTEGER PRIMARY KEY);
		CREATE TABLE c (id INTEGER PRIMARY KEY, pid INT REFERENCES p);
		INSERT INTO c VALUES (1, 5);`))
	if err == nil || !strings.Contains(err.Error(), "table c row 1") {
		t.Fatalf("broken foreign key: %v", err)
	}
	if len(db2.Tables()) != 0 {
		t.Errorf("tables after a failed import: %v", db2.Tables())
	}
	_, err = db2.ImportSQL(context.Background(), strings.NewReader("CREATE TABLE a (x);\nINSERT INTO a VALUES (1);\nINSERT INTO nope VALUES (1);"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error position: %v", err)
	}
}

func TestDumpRoundTrip(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE "select" ("from" TEXT PRIMARY KEY, n REAL, b BLOB, "odd ""col""" INT DEFAULT 7 CHECK ("odd ""col""" > 0));
		CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL COLLATE NOCASE UNIQUE, ref TEXT REFERENCES "select" ON DELETE SET NULL);
		CREATE TABLE a_child (id INTEGER PRIMARY KEY, uid INT NOT NULL REFERENCES users ON DELETE CASCADE, x INT, y INT, UNIQUE (x, y));
		CREATE INDEX users_ref ON users (ref);
		CREATE TABLE m (k INTEGER PRIMARY KEY, v TEXT) USING memory;
		CREATE TABLE ts (t INTEGER NOT NULL, v REAL) USING timeseries WITH (time_column = t, block_rows = 16, retention = '1 day');
		CREATE VIEW names (n) AS SELECT name FROM users WHERE id > 1;
		INSERT INTO "select" VALUES ('it''s', 1.5, x'00ff', 3), ('b', -0.0, NULL, 7), ('c', 1e300, x'', 1);
		INSERT INTO users (name, ref) VALUES ('kim', 'b'), ('lee', NULL), ('park', 'it''s');
		DELETE FROM users WHERE name = 'park';
		INSERT INTO a_child VALUES (1, 1, 1, 2), (2, 2, NULL, NULL);
		INSERT INTO m VALUES (1, 'mem');
		INSERT INTO ts VALUES (10, 1.0), (20, 2.0), (30, 3.0), (40, 4.0), (50, 5.0);`)
	var buf bytes.Buffer
	if err := db.Dump(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	dump := buf.String()
	if !strings.Contains(dump, "INSERT INTO sqlite_sequence VALUES('users',3);") {
		t.Errorf("no high-water mark in dump:\n%s", dump)
	}
	db2, err := OpenWithOptions(filepath.Join(t.TempDir(), "copy.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := db2.ImportSQL(context.Background(), strings.NewReader(dump)); err != nil {
		t.Fatalf("%v\n%s", err, dump)
	}
	for _, q := range []string{
		`SELECT "from", n, hex(b), "odd ""col""" FROM "select" ORDER BY 1`,
		`SELECT * FROM users ORDER BY id`,
		`SELECT * FROM a_child ORDER BY id`,
		`SELECT * FROM m`,
		`SELECT * FROM ts ORDER BY t`,
		`SELECT * FROM names`,
	} {
		if a, b := query(t, db, q), query(t, db2, q); a != b {
			t.Errorf("%s:\n original %s\n copy     %s", q, a, b)
		}
	}
	for _, name := range []string{"select", "users", "a_child", "m", "ts"} {
		if a, b := db.Engine().Catalog().Describe(name), db2.Engine().Catalog().Describe(name); a != b {
			t.Errorf("schema of %s:\n%s\n%s", name, a, b)
		}
	}
	exec(t, db2, "INSERT INTO users (name) VALUES ('new')")
	expect(t, db2, "SELECT id FROM users WHERE name = 'new'", "4") // AUTOINCREMENT kept its high-water mark
	var buf2 bytes.Buffer
	db2.Dump(context.Background(), &buf2)
	if !strings.Contains(buf2.String(), "'new'") {
		t.Error("second dump misses the new row")
	}
}

func TestCSV(t *testing.T) {
	db := open(t)
	exec(t, db, "CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT, score REAL, note)")
	in := "\ufeffname,score,id,note\nkim,1.5,1,\n\"lee, jr\",2,2,\"say \"\"hi\"\"\"\npark,,3,007\n"
	n, err := db.ImportCSV(context.Background(), "p", strings.NewReader(in), nil)
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	expect(t, db, "SELECT id, name, score, typeof(score), note, typeof(note) FROM p ORDER BY id",
		`1|kim|1.5|real|NULL|null;2|lee, jr|2|real|say "hi"|text;3|park|NULL|null|007|text`)
	n, err = db.ImportCSV(context.Background(), "q", strings.NewReader("a;b\n1;x\n2.5;y\n"), &CSVOptions{Comma: ';', Create: true})
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	expect(t, db, "SELECT a, typeof(a), b FROM q ORDER BY b", "1|integer|x;2.5|real|y")
	if _, err := db.ImportCSV(context.Background(), "p", strings.NewReader("id,bogus\n9,1\n"), nil); err == nil {
		t.Error("unknown column accepted")
	}
	if _, err := db.ImportCSV(context.Background(), "p", strings.NewReader("id,name\n10,a\n1,dup\n"), nil); err == nil {
		t.Error("duplicate key accepted")
	}
	expect(t, db, "SELECT count(*) FROM p", "3") // the failed import left nothing

	var out bytes.Buffer
	n, err = db.ExportCSV(context.Background(), &out, &CSVOptions{Null: "\\N"}, "SELECT id, name, note, x'0a' FROM p WHERE id < ? ORDER BY id", 3)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	want := "id,name,note,x'0a'\n1,kim,\\N,0a\n2,\"lee, jr\",\"say \"\"hi\"\"\",0a\n"
	if out.String() != want {
		t.Errorf("export:\n%s\nwant\n%s", out.String(), want)
	}
}
