package sql

import "testing"

func TestParseValid(t *testing.T) {
	cases := []string{
		"CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name VARCHAR(50) NOT NULL, email TEXT UNIQUE, age INT DEFAULT 0)",
		"CREATE TABLE t (a TEXT, b REAL, PRIMARY KEY (a))",
		"CREATE UNIQUE INDEX idx_email ON users (email)",
		"INSERT INTO users (name, age) VALUES ('kim', 30), ('lee', ?)",
		"SELECT u.name, COUNT(*) AS n FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.age BETWEEN 10 AND 20 AND u.name NOT LIKE 'a%' GROUP BY u.name HAVING COUNT(*) > 1 ORDER BY n DESC, 1 LIMIT 10 OFFSET 5",
		"SELECT * FROM a, b WHERE a.id = b.id AND a.x IN (1, 2, 3) AND a.y IS NOT NULL",
		"SELECT CASE WHEN x > 0 THEN 'pos' ELSE 'neg' END, CAST(x AS TEXT), -9223372036854775808 FROM t",
		"UPDATE users SET age = age + 1, name = 'x' WHERE id = ?",
		"DELETE FROM users WHERE id = 1",
		"BEGIN; COMMIT; ROLLBACK",
		"ALTER TABLE users ADD COLUMN created_at TEXT DEFAULT 'now'",
		"EXPLAIN SELECT * FROM t WHERE id = 1",
		"DROP TABLE IF EXISTS users; DROP INDEX idx",
		"SELECT x'00ff', \"weird name\" FROM t -- comment",
	}
	for _, c := range cases {
		if _, _, err := Parse(c); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []string{
		"SELEC 1",
		"SELECT FROM",
		"INSERT INTO t VALUES (1",
		"CREATE TABLE t (a, b",
		"SELECT 'abc",
		"SELECT 9223372036854775808",
	}
	for _, c := range cases {
		if _, _, err := Parse(c); err == nil {
			t.Errorf("%s: expected error", c)
		}
	}
}

func TestParams(t *testing.T) {
	_, n, err := Parse("INSERT INTO t VALUES (?, ?); SELECT * FROM t WHERE a = ?")
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
