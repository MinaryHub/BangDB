package bangdb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func mustFail(t *testing.T, db *DB, q, want string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: got %v, want error containing %q", q, err, want)
	}
}

func TestCheckConstraints(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE acct (
		id INTEGER PRIMARY KEY,
		balance INT CHECK (balance >= 0),
		kind TEXT CHECK (kind IN ('a', 'b')),
		lo INT, hi INT,
		CHECK (lo <= hi))`)
	exec(t, db, `INSERT INTO acct VALUES (1, 10, 'a', 1, 2), (2, NULL, NULL, NULL, 5)`)
	if _, err := db.Exec(`INSERT INTO acct VALUES (3, -1, 'a', 1, 2)`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("negative balance: %v", err)
	}
	mustFail(t, db, `INSERT INTO acct VALUES (3, 1, 'c', 1, 2)`, "CHECK constraint failed")
	mustFail(t, db, `UPDATE acct SET lo = 9 WHERE id = 1`, "CHECK constraint failed")
	mustFail(t, db, `UPDATE acct SET balance = balance - 20`, "CHECK constraint failed")
	expect(t, db, `SELECT balance FROM acct ORDER BY id`, "10;NULL") // statement rolled back
	exec(t, db, `UPDATE acct SET balance = balance - 10 WHERE id = 1`)
	exec(t, db, `INSERT INTO acct (id, balance, kind, lo, hi) VALUES (5, 0, 'b', 0, 0) ON CONFLICT DO NOTHING`)
	mustFail(t, db, `INSERT INTO acct VALUES (5, 0, 'b', 0, 0) ON CONFLICT (id) DO UPDATE SET balance = -5`, "CHECK")

	mustFail(t, db, `CREATE TABLE bad (a INT CHECK (a > (SELECT 1)))`, "not allowed")
	mustFail(t, db, `CREATE TABLE bad (a INT CHECK (count(*) > 0))`, "not allowed")
	mustFail(t, db, `CREATE TABLE bad (a INT CHECK (a > ?))`, "not allowed", 1)
	mustFail(t, db, `CREATE TABLE bad (a INT CHECK (b > 0))`, "no such column")
	mustFail(t, db, `ALTER TABLE acct DROP COLUMN lo`, "CHECK")

	exec(t, db, `ALTER TABLE acct RENAME COLUMN balance TO amount`)
	mustFail(t, db, `UPDATE acct SET amount = -1`, "CHECK constraint failed")
	if d := db.Engine().Catalog().Describe("acct"); !strings.Contains(d, "CHECK ((amount >= 0))") {
		t.Fatalf("describe: %s", d)
	}
}

func TestForeignKeys(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE dept (id INTEGER PRIMARY KEY, code TEXT UNIQUE, name TEXT);
		CREATE TABLE emp (
			id INTEGER PRIMARY KEY,
			dept_id INT REFERENCES dept ON DELETE CASCADE ON UPDATE CASCADE,
			dept_code TEXT,
			boss INT REFERENCES emp(id) ON DELETE SET NULL,
			FOREIGN KEY (dept_code) REFERENCES dept(code));
		CREATE INDEX emp_dept ON emp (dept_id);
		INSERT INTO dept VALUES (1, 'rd', 'R&D'), (2, 'ops', 'Ops'), (3, 'hr', 'HR');
		INSERT INTO emp VALUES (10, 1, 'rd', NULL), (11, 1, NULL, 10), (12, 2, 'ops', 10), (13, NULL, NULL, 13)`)

	if _, err := db.Exec(`INSERT INTO emp VALUES (20, 9, NULL, NULL)`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("missing parent: %v", err)
	}
	mustFail(t, db, `INSERT INTO emp VALUES (20, 1, 'xx', NULL)`, "FOREIGN KEY constraint failed")
	mustFail(t, db, `INSERT INTO emp VALUES (20, 1, NULL, 99)`, "FOREIGN KEY constraint failed")
	mustFail(t, db, `UPDATE emp SET dept_id = 7 WHERE id = 10`, "FOREIGN KEY")
	exec(t, db, `INSERT INTO emp VALUES (20, '3', NULL, 20)`) // coerced to the parent's type; self reference

	// NO ACTION on dept_code: a referenced code cannot go away.
	mustFail(t, db, `DELETE FROM dept WHERE id = 2`, "FOREIGN KEY constraint failed")
	mustFail(t, db, `UPDATE dept SET code = 'ops2' WHERE id = 2`, "FOREIGN KEY constraint failed")
	exec(t, db, `UPDATE dept SET code = 'hr2' WHERE id = 3`) // unreferenced

	// ON UPDATE CASCADE through the rowid.
	exec(t, db, `UPDATE dept SET id = 100 WHERE id = 1`)
	expect(t, db, `SELECT id, dept_id FROM emp ORDER BY id`, "10|100;11|100;12|2;13|NULL;20|3")

	// ON DELETE CASCADE, then SET NULL on the self reference.
	exec(t, db, `UPDATE emp SET dept_code = NULL WHERE id = 10`)
	exec(t, db, `DELETE FROM dept WHERE id = 100`)
	expect(t, db, `SELECT id, boss FROM emp ORDER BY id`, "12|NULL;13|13;20|20")

	// REPLACE deletes the old parent row, which runs ON DELETE.
	mustFail(t, db, `INSERT OR REPLACE INTO dept VALUES (2, 'ops', 'Ops again')`, "FOREIGN KEY")
	exec(t, db, `INSERT OR REPLACE INTO dept VALUES (3, 'hr2', 'People')`)
	expect(t, db, `SELECT count(*) FROM emp WHERE dept_id = 3`, "0")

	mustFail(t, db, `DROP TABLE dept`, "referenced by a foreign key of emp")
	mustFail(t, db, `ALTER TABLE emp DROP COLUMN dept_id`, "foreign key")
	mustFail(t, db, `ALTER TABLE dept DROP COLUMN code`, "referenced by a foreign key")

	mustFail(t, db, `CREATE TABLE x (a INT REFERENCES nope)`, "unknown table")
	mustFail(t, db, `CREATE TABLE x (a INT REFERENCES dept(name))`, "not a PRIMARY KEY or UNIQUE")
	mustFail(t, db, `CREATE TABLE x (a INT, b INT, FOREIGN KEY (a, b) REFERENCES dept(id))`, "references 1")

	// Renames keep the references working.
	exec(t, db, `ALTER TABLE dept RENAME TO division`)
	exec(t, db, `ALTER TABLE division RENAME COLUMN code TO short`)
	mustFail(t, db, `INSERT INTO emp VALUES (30, NULL, 'zz', NULL)`, "FOREIGN KEY")
	exec(t, db, `INSERT INTO emp VALUES (30, NULL, 'ops', NULL)`)
	d := db.Engine().Catalog().Describe("emp")
	if !strings.Contains(d, "REFERENCES division (short)") || !strings.Contains(d, "REFERENCES division (id) ON DELETE CASCADE ON UPDATE CASCADE") {
		t.Fatalf("describe: %s", d)
	}
	exec(t, db, `DROP TABLE emp; DROP TABLE division`)
}

func TestForeignKeyCascadeChainAndPersistence(t *testing.T) {
	durableOnly(t)
	path := filepath.Join(t.TempDir(), "fk.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `
		CREATE TABLE node (id INTEGER PRIMARY KEY, parent INT REFERENCES node ON DELETE CASCADE);
		CREATE TABLE a (k TEXT, n INT, PRIMARY KEY (k, n));
		CREATE TABLE b (id INTEGER PRIMARY KEY, k TEXT, n INT, FOREIGN KEY (k, n) REFERENCES a ON DELETE SET NULL)`)
	exec(t, db, `INSERT INTO node VALUES (1, NULL)`)
	for i := 2; i <= 200; i++ {
		exec(t, db, `INSERT INTO node VALUES (?, ?)`, i, i/2)
	}
	db.Close()

	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustFail(t, db, `INSERT INTO node VALUES (500, 999)`, "FOREIGN KEY")
	exec(t, db, `DELETE FROM node WHERE id = 2`) // removes the subtree under 2
	expect(t, db, `SELECT count(*) FROM node`, "73")
	exec(t, db, `DELETE FROM node WHERE id = 1`)
	expect(t, db, `SELECT count(*) FROM node`, "0")

	exec(t, db, `INSERT INTO a VALUES ('x', 1), ('x', 2); INSERT INTO b VALUES (1, 'x', 1), (2, 'x', 2), (3, NULL, 7)`)
	mustFail(t, db, `INSERT INTO b VALUES (4, 'x', 3)`, "FOREIGN KEY")
	exec(t, db, `DELETE FROM a WHERE n = 1`)
	expect(t, db, `SELECT id, k, n FROM b ORDER BY id`, "1|NULL|NULL;2|x|2;3|NULL|7")
	checkIntegrity(t, db)
}

func TestViews(t *testing.T) {
	durableOnly(t)
	path := filepath.Join(t.TempDir(), "v.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `
		CREATE TABLE sales (id INTEGER PRIMARY KEY, region TEXT, amount INT);
		INSERT INTO sales (region, amount) VALUES ('east', 10), ('east', 5), ('west', 7);
		CREATE VIEW totals (region, total) AS SELECT region, sum(amount) FROM sales GROUP BY region;
		CREATE VIEW big AS SELECT region FROM totals WHERE total > 10`)
	expect(t, db, `SELECT * FROM totals ORDER BY region`, "east|15;west|7")
	expect(t, db, `SELECT region FROM big`, "east")
	expect(t, db, `SELECT s.id FROM sales s JOIN big b ON s.region = b.region ORDER BY s.id`, "1;2")
	expect(t, db, `SELECT t.total FROM totals AS t WHERE t.region = 'west'`, "7")
	db.Close()

	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, `INSERT INTO sales (region, amount) VALUES ('west', 20)`)
	expect(t, db, `SELECT region FROM big ORDER BY region`, "east;west") // views are not materialized
	expect(t, db, `SELECT (SELECT max(total) FROM totals)`, "27")
	if v := db.Views(); len(v) != 2 {
		t.Fatalf("views: %v", v)
	}

	mustFail(t, db, `INSERT INTO totals VALUES ('x', 1)`, "is a view")
	mustFail(t, db, `DELETE FROM big`, "is a view")
	mustFail(t, db, `CREATE VIEW totals AS SELECT 1`, "already exists")
	mustFail(t, db, `CREATE TABLE totals (a INT)`, "view named totals")
	mustFail(t, db, `CREATE VIEW sales AS SELECT 1`, "table named sales")
	mustFail(t, db, `CREATE VIEW p AS SELECT ?`, "parameters", 1)
	mustFail(t, db, `CREATE VIEW c (a, b) AS SELECT 1`, "column names")
	mustFail(t, db, `CREATE VIEW n AS SELECT * FROM nope`, "no such table")
	mustFail(t, db, `DROP TABLE big`, "DROP VIEW")
	exec(t, db, `CREATE VIEW IF NOT EXISTS big AS SELECT 1`)

	// A view whose table went away fails when used, not before.
	exec(t, db, `CREATE TABLE tmp (a INT); CREATE VIEW vt AS SELECT a FROM tmp; DROP TABLE tmp`)
	mustFail(t, db, `SELECT * FROM vt`, "no such table")
	exec(t, db, `DROP VIEW vt; DROP VIEW big; DROP VIEW IF EXISTS big`)
	mustFail(t, db, `DROP VIEW big`, "no such view")
	if got := db.Engine().Catalog().DescribeView("totals"); got != "CREATE VIEW totals (region, total) AS SELECT region, sum(amount) FROM sales GROUP BY region;" {
		t.Fatalf("describe view: %q", got)
	}
}
