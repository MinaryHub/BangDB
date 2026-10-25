package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// FuzzExec runs arbitrary SQL against a small database: statements may fail,
// but must not panic, and the database must stay structurally sound.
func FuzzExec(f *testing.F) {
	for _, s := range []string{
		"INSERT INTO t (name, n) VALUES ('x', 1)",
		"SELECT name, SUM(n) FROM t GROUP BY name HAVING SUM(n) > 1 ORDER BY 2",
		"UPDATE t SET n = n * 2, name = UPPER(name) WHERE id IN (1, 2)",
		"DELETE FROM t WHERE n BETWEEN 1 AND 3",
		"SELECT * FROM t a LEFT JOIN u b ON b.tid = a.id WHERE b.v IS NULL",
		"CREATE INDEX i ON t (n); SELECT * FROM t WHERE n > 1.5",
		"ALTER TABLE t ADD COLUMN z REAL DEFAULT 1.5; SELECT z FROM t",
		"INSERT INTO u (tid, v) VALUES (1, x'00'), (2, NULL); SELECT LENGTH(v), TYPEOF(v) FROM u",
		"SELECT SUBSTR(name, -1, 2), ROUND(n / 3.0, 1), COALESCE(NULL, n), CAST(name AS INTEGER) FROM t",
		"DROP TABLE u; CREATE TABLE u (k TEXT PRIMARY KEY)",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		db, err := Open(":memory:", &Options{QueryTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, n INT);
			CREATE TABLE u (id INTEGER PRIMARY KEY AUTOINCREMENT, tid INT, v BLOB);
			CREATE INDEX u_tid ON u (tid);
			INSERT INTO t (name, n) VALUES ('a', 1), ('b', 2), ('c', NULL);
			INSERT INTO u (tid, v) VALUES (1, x'01'), (3, NULL)`); err != nil {
			t.Fatal(err)
		}
		db.Exec(src)
		if rows, err := db.QueryRows(context.Background(), src); err == nil {
			for rows.Next() {
			}
			rows.Close()
		}
		problems, err := db.IntegrityCheck(context.Background())
		if err != nil || len(problems) > 0 {
			t.Fatalf("after %q: %v\n%s", src, err, strings.Join(problems, "\n"))
		}
	})
}
