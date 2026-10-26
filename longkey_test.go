package bangdb

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestLongIndexKeys checks indexes on values longer than the B+tree key
// limit: results must match an unindexed copy of the same data.
func TestLongIndexKeys(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, k TEXT UNIQUE, g INT, n INT);
		CREATE INDEX t_k_n ON t (k, n);
		CREATE INDEX t_n_k ON t (n, k);
		CREATE TABLE u (id INTEGER PRIMARY KEY, k TEXT, g INT, n INT)`)
	rng := rand.New(rand.NewSource(3))
	common := strings.Repeat("p", 1000) // values share more than the key limit
	var keys []string
	for i := 0; i < 400; i++ {
		var k string
		switch i % 4 {
		case 0:
			k = common + fmt.Sprintf("%05d", rng.Intn(100000)) + strings.Repeat("z", rng.Intn(50))
		case 1:
			k = common[:rng.Intn(600)] + fmt.Sprint(i)
		case 2:
			k = fmt.Sprintf("short-%d", i)
		default:
			k = strings.Repeat("q", 490+rng.Intn(30)) + fmt.Sprint(i) // around the limit
		}
		keys = append(keys, k)
	}
	seen := map[string]bool{}
	for i, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		n := rng.Intn(5)
		exec(t, db, `INSERT INTO t (k, g, n) VALUES (?, ?, ?)`, k, i%7, n)
		exec(t, db, `INSERT INTO u (k, g, n) VALUES (?, ?, ?)`, k, i%7, n)
	}
	same := func(q string, args ...any) {
		t.Helper()
		got := query(t, db, q, args...)
		want := query(t, db, strings.ReplaceAll(q, " t ", " u "), args...)
		if got != want {
			t.Fatalf("%s: indexed and unindexed results differ", q)
		}
	}
	same(`SELECT id FROM t ORDER BY k`)
	same(`SELECT id FROM t ORDER BY k DESC`)
	same(`SELECT id FROM t ORDER BY k LIMIT 7`)
	same(`SELECT id FROM t WHERE n = 2 ORDER BY k DESC`)
	same(`SELECT id FROM t WHERE n = 3 ORDER BY k`)
	for _, k := range keys[:40] {
		same(`SELECT id FROM t WHERE k = ?`, k)
		same(`SELECT id FROM t WHERE k > ? ORDER BY k`, k)
		same(`SELECT id FROM t WHERE k >= ? ORDER BY k DESC`, k)
		same(`SELECT id FROM t WHERE k < ? ORDER BY k DESC`, k)
		same(`SELECT id FROM t WHERE k <= ? ORDER BY k`, k)
		same(`SELECT id FROM t WHERE k BETWEEN ? AND ? ORDER BY k`, k, k+"zzz")
		same(`SELECT id FROM t WHERE k IN (?, ?) ORDER BY id`, k, keys[50])
		same(`SELECT id FROM t WHERE n = 1 AND k > ? ORDER BY id`, k)
		same(`SELECT id FROM t WHERE k = ? AND n = 2`, k)
	}
	if plan := query(t, db, `EXPLAIN SELECT id FROM t WHERE k = ?`, keys[0]); !strings.Contains(plan, "INDEX") {
		t.Fatalf("plan: %s", plan)
	}

	// Uniqueness compares the values, not the truncated keys.
	if _, err := db.Exec(`INSERT INTO t (k) VALUES (?)`, keys[0]); !errors.Is(err, ErrConstraint) {
		t.Fatalf("duplicate long key: %v", err)
	}
	exec(t, db, `INSERT INTO t (k) VALUES (?)`, common+"-new")
	exec(t, db, `INSERT INTO t (k, g) VALUES (?, 1) ON CONFLICT (k) DO UPDATE SET g = 99`, common+"-new")
	expect(t, db, `SELECT g FROM t WHERE k = ?`, "99", common+"-new")
	exec(t, db, `UPDATE t SET k = k || 'x' WHERE k = ?`, keys[0])
	expect(t, db, `SELECT count(*) FROM t WHERE k = ?`, "1", keys[0]+"x")

	// Foreign keys to a long key.
	exec(t, db, `CREATE TABLE c (pk TEXT REFERENCES t (k) ON UPDATE CASCADE); CREATE INDEX c_pk ON c (pk)`)
	exec(t, db, `INSERT INTO c VALUES (?)`, keys[4])
	mustFail(t, db, `INSERT INTO c VALUES (?)`, "FOREIGN KEY", common+"-missing")
	exec(t, db, `UPDATE t SET k = ? WHERE k = ?`, common+"-moved", keys[4])
	expect(t, db, `SELECT count(*) FROM c WHERE pk = ?`, "1", common+"-moved")
}
