package bangdb

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestOrderByIndexMatchesSort compares queries answered in index order with
// the same queries sorted explicitly (on an identical table without indexes).
func TestOrderByIndexMatchesSort(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, x INT, y TEXT, z REAL);
		CREATE TABLE b (id INTEGER PRIMARY KEY, x INT, y TEXT, z REAL);
		CREATE INDEX a_x ON a (x); CREATE INDEX a_yz ON a (y, z)`)
	rng := rand.New(rand.NewSource(21))
	val := func(col string) any {
		if rng.Intn(10) == 0 {
			return nil
		}
		switch col {
		case "x":
			return int64(rng.Intn(40))
		case "y":
			return fmt.Sprintf("s%d", rng.Intn(6))
		}
		return float64(rng.Intn(20)) / 4
	}
	db.Update(func(tx *Tx) error {
		for i := 0; i < 2000; i++ {
			x, y, z := val("x"), val("y"), val("z")
			for _, tb := range []string{"a", "b"} {
				tx.Exec(`INSERT INTO `+tb+` (x, y, z) VALUES (?, ?, ?)`, x, y, z)
			}
		}
		return nil
	})
	type q struct {
		where string
		args  []any
		order string
	}
	var qs []q
	for k := 0; k < 300; k++ {
		dir := []string{"", " DESC"}[rng.Intn(2)]
		switch rng.Intn(5) {
		case 0:
			qs = append(qs, q{"1", nil, "id" + dir})
		case 1:
			qs = append(qs, q{"id > ? AND id <= ?", []any{rng.Intn(1000), 1000 + rng.Intn(1000)}, "id" + dir})
		case 2:
			qs = append(qs, q{"1", nil, "x" + dir + ", id" + dir})
		case 3:
			qs = append(qs, q{"x BETWEEN ? AND ?", []any{rng.Intn(20), 20 + rng.Intn(20)}, "x" + dir + ", id" + dir})
		default:
			qs = append(qs, q{"y = ? AND z > ?", []any{val("y"), val("z")}, "z" + dir + ", id" + dir})
		}
	}
	for _, c := range qs {
		limit := ""
		if rng.Intn(2) == 0 {
			limit = fmt.Sprintf(" LIMIT %d OFFSET %d", 1+rng.Intn(30), rng.Intn(5))
		}
		qa := `SELECT id, x, y, z FROM a WHERE ` + c.where + ` ORDER BY ` + c.order + limit
		qb := strings.Replace(qa, "FROM a", "FROM b", 1)
		if ga, gb := query(t, db, qa, c.args...), query(t, db, qb, c.args...); ga != gb {
			t.Fatalf("%s %v\nindex: %.200s\nsort:  %.200s", qa, c.args, ga, gb)
		}
	}
	// The plans really use the index order.
	for qry, want := range map[string]string{
		`SELECT * FROM a ORDER BY id DESC LIMIT 5`:                  "ordered by rowid",
		`SELECT * FROM a WHERE x = 3 ORDER BY id DESC`:              "a_x (x=?) (ordered by index)",
		`SELECT * FROM a ORDER BY x LIMIT 5`:                        "SCAN a USING INDEX a_x (ordered by index)",
		`SELECT * FROM a WHERE y = 's1' ORDER BY y, z DESC LIMIT 5`: "a_yz (y=?) (ordered by index)",
	} {
		rows, _ := db.Query("EXPLAIN " + qry)
		all, _ := rows.All()
		if p := fmt.Sprint(all); !strings.Contains(p, want) || strings.Contains(p, "SORT") {
			t.Errorf("%s: plan %s, want %q without SORT", qry, p, want)
		}
	}
}
