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
		"INSERT INTO t (name, n) VALUES ('a', 5) ON CONFLICT (name) DO UPDATE SET n = excluded.n + t.n WHERE excluded.n > 0 RETURNING *",
		"INSERT OR REPLACE INTO t (id, name) SELECT id + 1, name || 'x' FROM t RETURNING id",
		"CREATE TABLE p (a INT, b TEXT, c REAL, PRIMARY KEY (a, b), UNIQUE (b, c)); INSERT INTO p VALUES (1, 'x', 1.5) ON CONFLICT DO NOTHING",
		"UPDATE u SET tid = tid + 1 WHERE tid IN (1, 3) RETURNING rowid, tid; DELETE FROM u RETURNING v",
		"SELECT name, (SELECT COUNT(*) FROM u WHERE u.tid = t.id) FROM t WHERE EXISTS (SELECT 1 FROM u WHERE tid = t.id)",
		"SELECT * FROM (SELECT n, COUNT(*) c FROM t GROUP BY n) x JOIN t ON t.n = x.n WHERE t.id NOT IN (SELECT tid FROM u)",
		"SELECT n FROM t UNION SELECT tid FROM u INTERSECT SELECT 1 EXCEPT SELECT NULL ORDER BY 1 DESC LIMIT 2",
		"UPDATE t SET n = (SELECT MAX(tid) FROM u WHERE tid < t.id) WHERE id IN (SELECT tid FROM u UNION ALL SELECT 2)",
		"SELECT * FROM t WHERE n = 1 OR name = 'b' OR id IN (2, 3) ORDER BY id DESC LIMIT 2",
		"SELECT t.id, u.v FROM t JOIN u ON u.v = t.name LEFT JOIN t t2 ON t2.n = u.tid ORDER BY t.id",
		"SELECT * FROM t WHERE name > 'a' ORDER BY name DESC, id DESC LIMIT 1 OFFSET 1",
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
