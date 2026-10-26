package bangdb

import (
	"strings"
	"testing"
)

func TestCoveringIndex(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, k TEXT, n INT, f REAL, b BLOB, ci TEXT COLLATE NOCASE, pad TEXT);
		CREATE INDEX t_k ON t (k, n);
		CREATE INDEX t_f ON t (f, b);
		CREATE INDEX t_ci ON t (ci)`)
	long := strings.Repeat("z", 3000)
	exec(t, db, `INSERT INTO t (k, n, f, b, ci, pad) VALUES
		('a', 1, 1.5, x'00ff', 'Abc', 'p1'), ('a', 2, -0.25, x'', 'abd', 'p2'),
		('b', NULL, NULL, NULL, 'X', 'p3'), ('a'||char(0)||'z', 3, 1e300, x'0001', 'y', 'p4'), (?, 4, 2.0, NULL, NULL, 'p5')`, long+"1")
	exec(t, db, "INSERT INTO t (k, n, pad) VALUES (?, 5, 'p6')", long+"2")

	for _, c := range []struct{ q, plan, want string }{
		{"SELECT id, k, n FROM t WHERE k = 'a' ORDER BY n", "COVERING INDEX t_k", "1|a|1;2|a|2"},
		{"SELECT count(*) FROM t WHERE k >= 'a'", "COVERING INDEX t_k", "6"},
		{"SELECT length(k), n FROM t WHERE k > 'b' ORDER BY n", "COVERING INDEX t_k", "3001|4;3001|5"}, // cut long keys: rows are read
		{"SELECT hex(k), n FROM t WHERE k > 'a' AND k < 'b'", "COVERING INDEX t_k", "61007A|3"},
		{"SELECT f, hex(b) FROM t WHERE f > 0 ORDER BY f", "COVERING INDEX t_f", "1.5|00FF;2|;1e+300|0001"},
		{"SELECT f, typeof(b) FROM t WHERE f < 0", "COVERING INDEX t_f", "-0.25|blob"},
		{"SELECT k, pad FROM t WHERE k = 'a' ORDER BY pad", "USING INDEX t_k", "a|p1;a|p2"},
		{"SELECT ci FROM t WHERE ci = 'ABC'", "USING INDEX t_ci", "Abc"}, // folded key: not covering
		{"SELECT id FROM t WHERE ci = 'ABC'", "USING INDEX t_ci", "1"},   // the filter rechecks ci
		{"SELECT k, (SELECT pad FROM t t2 WHERE t2.id = t.id) FROM t WHERE k = 'b'", "COVERING INDEX t_k", "b|p3"},
		{"SELECT k FROM t WHERE k = 'b' AND EXISTS (SELECT 1 WHERE t.pad = 'p3')", "USING INDEX t_k", "b"},
	} {
		plan := query(t, db, "EXPLAIN "+c.q)
		if !strings.Contains(plan, c.plan) {
			t.Errorf("%s: plan %s, want %s", c.q, plan, c.plan)
		}
		expect(t, db, c.q, c.want)
	}
	// Joins: the inner level can be covered too.
	exec(t, db, "CREATE TABLE u (id INTEGER PRIMARY KEY, tk TEXT, w INT); CREATE INDEX u_tk ON u (tk, w); INSERT INTO u (tk, w) VALUES ('a', 10), ('a', 20), ('b', 30)")
	expect(t, db, "SELECT t.id, u.w FROM t JOIN u ON u.tk = t.k WHERE t.n < 3 ORDER BY 1, 2", "1|10;1|20;2|10;2|20")
}
