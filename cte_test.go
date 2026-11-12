package bangdb

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCTE(t *testing.T) {
	db := setupShop(t) // users(id,name,email,age), orders(id,user_id,item,amount)

	// A plain CTE, with and without column names.
	expect(t, db, `WITH big AS (SELECT * FROM orders WHERE amount > 10)
		SELECT item, amount FROM big ORDER BY id`, "book|12.5;book|12.5;laptop|1200")
	expect(t, db, `WITH totals (uid, total) AS (SELECT user_id, SUM(amount) FROM orders GROUP BY user_id)
		SELECT u.name, t.total FROM users u JOIN totals t ON t.uid = u.id ORDER BY u.id`,
		"kim|14.5;lee|12.5;park|1200")
	// Several CTEs: a later one sees the earlier ones; one may be used twice.
	expect(t, db, `WITH a AS (SELECT id, age FROM users WHERE age IS NOT NULL),
		     b AS (SELECT MAX(age) AS m FROM a)
		SELECT a.id FROM a, b WHERE a.age = b.m`, "3")
	expect(t, db, `WITH a AS (SELECT id FROM users WHERE id <= 2)
		SELECT x.id, y.id FROM a x JOIN a y ON y.id > x.id`, "1|2")
	// In subqueries, derived tables and set operations.
	expect(t, db, `WITH buyers AS (SELECT DISTINCT user_id FROM orders)
		SELECT name FROM users WHERE id IN (SELECT user_id FROM buyers) ORDER BY id`, "kim;lee;park")
	expect(t, db, `WITH buyers AS (SELECT DISTINCT user_id AS id FROM orders)
		SELECT name FROM users u WHERE NOT EXISTS (SELECT 1 FROM buyers b WHERE b.id = u.id)`, "choi")
	expect(t, db, `SELECT n FROM (WITH c AS (SELECT COUNT(*) AS n FROM users) SELECT n FROM c) AS d`, "4")
	expect(t, db, `SELECT (WITH c AS (SELECT 7 AS v) SELECT v FROM c)`, "7")
	expect(t, db, `WITH a AS (SELECT 1 AS v), b AS (SELECT 2 AS v)
		SELECT v FROM a UNION ALL SELECT v FROM b UNION ALL SELECT v + 10 FROM a ORDER BY v DESC`, "11;2;1")
	// A CTE hides a table of the same name; without RECURSIVE its body
	// still sees the table.
	expect(t, db, `WITH users AS (SELECT name FROM users WHERE id = 2) SELECT * FROM users`, "lee")
	// The inner WITH wins.
	expect(t, db, `WITH c AS (SELECT 1 AS v) SELECT * FROM (WITH c AS (SELECT 2 AS v) SELECT v FROM c) d`, "2")
	// Parameters.
	expect(t, db, `WITH c AS (SELECT id FROM users WHERE id > ?) SELECT COUNT(*) FROM c WHERE id < ?`, "2", 1, 4)

	// Views.
	exec(t, db, `CREATE VIEW spenders AS WITH t AS (SELECT user_id, SUM(amount) AS s FROM orders GROUP BY user_id)
		SELECT user_id FROM t WHERE s > 13`)
	expect(t, db, `SELECT * FROM spenders ORDER BY 1`, "1;3")
	// A view does not see the caller's CTEs.
	exec(t, db, `CREATE VIEW names AS SELECT name FROM users WHERE id = 1`)
	expect(t, db, `WITH users AS (SELECT 'x' AS name, 1 AS id) SELECT * FROM names`, "kim")

	// DML.
	exec(t, db, `CREATE TABLE log (uid INT, note TEXT)`)
	exec(t, db, `WITH b AS (SELECT DISTINCT user_id FROM orders WHERE amount > 100) INSERT INTO log SELECT user_id, 'big' FROM b`)
	exec(t, db, `INSERT INTO log WITH b AS (SELECT id FROM users WHERE age < 30) SELECT id, 'young' FROM b`)
	expect(t, db, `SELECT uid, note FROM log ORDER BY uid`, "2|young;3|big")
	exec(t, db, `WITH l AS (SELECT uid FROM log) UPDATE users SET age = 0 WHERE id IN (SELECT uid FROM l)`)
	expect(t, db, `SELECT id FROM users WHERE age = 0 ORDER BY id`, "2;3")
	// The target of UPDATE / DELETE is the table, even under a CTE's name.
	r := exec(t, db, `WITH log AS (SELECT 2 AS uid) DELETE FROM log WHERE uid IN (SELECT uid FROM log)`)
	if r.RowsAffected != 1 {
		t.Errorf("deleted %d rows, want 1", r.RowsAffected)
	}
	expect(t, db, `SELECT uid FROM log`, "3")

	// EXPLAIN works.
	if got := query(t, db, `EXPLAIN WITH c AS (SELECT 1 AS v) SELECT * FROM c`); !strings.Contains(got, "SCAN SUBQUERY c") {
		t.Errorf("explain: %s", got)
	}

	for _, q := range []string{
		`WITH c AS (SELECT 1) SELECT * FROM d`,
		`WITH c (a, b) AS (SELECT 1) SELECT * FROM c`,
		`WITH c AS (SELECT 1), c AS (SELECT 2) SELECT * FROM c`,
		`WITH c AS (SELECT * FROM c) SELECT * FROM c`, // not RECURSIVE: no table c
		`WITH c AS (SELECT 1) CREATE TABLE z (a INT)`,
		`WITH a AS (SELECT * FROM b), b AS (SELECT 1) SELECT * FROM a`, // no forward references
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}
}

func TestRecursiveCTE(t *testing.T) {
	db := open(t)
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < 5) SELECT n FROM c`, "1;2;3;4;5")
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < 100) SELECT SUM(n), COUNT(*) FROM c`, "5050|100")
	// LIMIT stops an endless recursion; OFFSET skips.
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c LIMIT 4) SELECT n FROM c`, "1;2;3;4")
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c LIMIT 3 OFFSET 2) SELECT n FROM c`, "3;4;5")
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c LIMIT 0) SELECT COUNT(*) FROM c`, "0")
	// UNION drops rows seen before, so a cycle ends.
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 0 UNION SELECT (n + 1) % 4 FROM c) SELECT n FROM c`, "0;1;2;3")
	// Fibonacci: several columns.
	expect(t, db, `WITH RECURSIVE f(i, a, b) AS (SELECT 1, 0, 1 UNION ALL SELECT i + 1, b, a + b FROM f WHERE i < 10)
		SELECT a FROM f`, "0;1;1;2;3;5;8;13;21;34")
	// Parameters inside the recursion.
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT ? UNION ALL SELECT n + 1 FROM c WHERE n < ?) SELECT COUNT(*) FROM c`, "8", 3, 10)

	exec(t, db, `
		CREATE TABLE emp (id INTEGER PRIMARY KEY, name TEXT, boss INT);
		CREATE INDEX emp_boss ON emp (boss);
		INSERT INTO emp VALUES (1, 'ceo', NULL), (2, 'cto', 1), (3, 'cfo', 1), (4, 'dev1', 2), (5, 'dev2', 2),
			(6, 'acct', 3), (7, 'intern', 4);
	`)
	// Everyone under the CTO, by generation (breadth first).
	expect(t, db, `WITH RECURSIVE under(id, name, depth) AS (
			SELECT id, name, 0 FROM emp WHERE id = 2
			UNION ALL
			SELECT e.id, e.name, u.depth + 1 FROM emp e JOIN under u ON e.boss = u.id)
		SELECT name, depth FROM under`, "cto|0;dev1|1;dev2|1;intern|2")
	// The recursive table first in the join.
	expect(t, db, `WITH RECURSIVE under(id) AS (
			SELECT 1 UNION ALL SELECT e.id FROM under u JOIN emp e ON e.boss = u.id)
		SELECT COUNT(*) FROM under`, "7")
	// Upwards: the chain of command of the intern.
	expect(t, db, `WITH RECURSIVE up(id, name, boss) AS (
			SELECT id, name, boss FROM emp WHERE name = 'intern'
			UNION ALL
			SELECT e.id, e.name, e.boss FROM emp e, up WHERE e.id = up.boss)
		SELECT GROUP_CONCAT(name) FROM up`, "intern,dev1,cto,ceo")
	// ORDER BY orders the queue: depth first with DESC on the depth, and a
	// path to keep siblings in order.
	expect(t, db, `WITH RECURSIVE tree(id, depth, path) AS (
			SELECT id, 0, name FROM emp WHERE boss IS NULL
			UNION ALL
			SELECT e.id, t.depth + 1, t.path || '/' || e.name FROM emp e JOIN tree t ON e.boss = t.id
			ORDER BY 2 DESC, 3)
		SELECT path FROM tree`,
		"ceo;ceo/cfo;ceo/cfo/acct;ceo/cto;ceo/cto/dev1;ceo/cto/dev1/intern;ceo/cto/dev2")
	expect(t, db, `WITH RECURSIVE tree(id, depth) AS (
			SELECT id, 0 FROM emp WHERE boss IS NULL
			UNION ALL
			SELECT e.id, t.depth + 1 FROM emp e JOIN tree t ON e.boss = t.id
			ORDER BY depth, id DESC)
		SELECT id FROM tree`, "1;3;2;6;5;4;7")
	// Several initial and several recursive SELECTs.
	expect(t, db, `WITH RECURSIVE c(n) AS (
			SELECT 1 UNION ALL SELECT 100
			UNION ALL SELECT n * 2 FROM c WHERE n < 4
			UNION ALL SELECT n + 100 FROM c WHERE n >= 100 AND n < 300)
		SELECT n FROM c ORDER BY n`, "1;2;4;100;200;300")
	// A recursive CTE next to plain ones, and used by another CTE.
	expect(t, db, `WITH RECURSIVE
			roots AS (SELECT id FROM emp WHERE boss = 1),
			under(id, root) AS (SELECT id, id FROM roots UNION ALL SELECT e.id, u.root FROM emp e JOIN under u ON e.boss = u.id),
			sizes AS (SELECT root, COUNT(*) AS n FROM under GROUP BY root)
		SELECT e.name, s.n FROM sizes s JOIN emp e ON e.id = s.root ORDER BY e.id`, "cto|4;cfo|2")
	// RECURSIVE without a self reference is a plain CTE.
	expect(t, db, `WITH RECURSIVE c AS (SELECT COUNT(*) AS n FROM emp) SELECT n FROM c`, "7")
	// Subqueries that do not name the recursive table are fine.
	expect(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < (SELECT COUNT(*) FROM emp))
		SELECT COUNT(*) FROM c`, "7")
	// DML with a recursive CTE.
	exec(t, db, `WITH RECURSIVE under(id) AS (SELECT 2 UNION ALL SELECT e.id FROM emp e JOIN under u ON e.boss = u.id)
		DELETE FROM emp WHERE id IN (SELECT id FROM under)`)
	expect(t, db, `SELECT GROUP_CONCAT(name) FROM emp`, "ceo,cfo,acct")
	exec(t, db, `CREATE TABLE nums (n INT)`)
	exec(t, db, `INSERT INTO nums WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < 1000) SELECT n FROM c`)
	expect(t, db, `SELECT COUNT(*), MIN(n), MAX(n) FROM nums`, "1000|1|1000")

	for _, q := range []string{
		`WITH RECURSIVE c(n) AS (SELECT n + 1 FROM c) SELECT * FROM c`,                                             // no initial SELECT
		`WITH RECURSIVE c(n) AS (SELECT 1 INTERSECT SELECT n FROM c) SELECT * FROM c`,                              // not a UNION
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n FROM c WHERE n IN (SELECT n FROM c)) SELECT * FROM c`, // in a subquery
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT a.n FROM c a, c b) SELECT * FROM c`,                     // named twice
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n, n FROM c) SELECT * FROM c`,                           // column count
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c ORDER BY x) SELECT * FROM c`,               // unknown ORDER BY
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c UNION ALL SELECT 5) SELECT * FROM c`,       // initial after recursive
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}

	// An endless recursion is stopped by the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c) SELECT COUNT(*) FROM c`); err == nil {
		t.Error("endless recursion: expected a timeout")
	}
}

// A statement is parsed once and its plan may be cached: running a CTE
// query again, concurrently, must not share its rows.
func TestCTERepeated(t *testing.T) {
	db := setupShop(t)
	const q = `WITH RECURSIVE c(n) AS (SELECT ? UNION ALL SELECT n + 1 FROM c WHERE n < ?),
		u AS (SELECT id FROM users WHERE id <= ?)
		SELECT (SELECT COUNT(*) FROM c), (SELECT COUNT(*) FROM u)`
	done := make(chan bool)
	for g := 1; g <= 4; g++ {
		go func(g int) {
			ok := true
			for i := 0; i < 200; i++ {
				rows, err := db.Query(q, 1, 10*g, g)
				if err != nil {
					t.Error(err)
					ok = false
					break
				}
				if got, want := render(rows), strings.Join([]string{itoa(10 * g), itoa(g)}, "|"); got != want {
					t.Errorf("got %s, want %s", got, want)
					ok = false
					break
				}
			}
			done <- ok
		}(g)
	}
	for g := 0; g < 4; g++ {
		<-done
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
