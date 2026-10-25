package bangdb

import (
	"database/sql"
	"testing"
)

func TestNamedAndNumberedParameters(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b INT)`)
	exec(t, db, `INSERT INTO t (a, b) VALUES (:a, :b)`, Named("a", "x"), Named("b", 1))
	exec(t, db, `INSERT INTO t (a, b) VALUES (@a, $b)`, Named("@a", "y"), sql.Named("b", 2))
	exec(t, db, `INSERT INTO t (a, b) VALUES ($2, $1)`, 3, "z")
	expect(t, db, `SELECT a, b FROM t ORDER BY id`, "x|1;y|2;z|3")
	// The same name used twice binds one value.
	expect(t, db, `SELECT COUNT(*) FROM t WHERE b >= :min AND id >= :min`, "2", Named("min", 2))
	expect(t, db, `SELECT a FROM t WHERE b = ?1 OR id = ?1`, "x", 1)
	for _, c := range []struct {
		q    string
		args []any
	}{
		{`SELECT :a`, nil},
		{`SELECT :a`, []any{Named("b", 1)}},
		{`SELECT ?, ?`, []any{1}},
		{`SELECT ?`, []any{1, 2}},
	} {
		if _, err := db.Query(c.q, c.args...); err == nil {
			t.Errorf("%s %v: expected error", c.q, c.args)
		}
	}
}
