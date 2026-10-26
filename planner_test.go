package bangdb

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestOrUnionMatchesScan(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, x INT, y TEXT, z REAL);
		CREATE TABLE b (id INTEGER PRIMARY KEY, x INT, y TEXT, z REAL);
		CREATE INDEX a_x ON a (x); CREATE INDEX a_y ON a (y); CREATE INDEX a_z ON a (z)`)
	rng := rand.New(rand.NewSource(8))
	db.Update(func(tx *Tx) error {
		for i := 0; i < 1500; i++ {
			x, y, z := any(int64(rng.Intn(50))), any(fmt.Sprintf("s%d", rng.Intn(30))), any(float64(rng.Intn(40))/4)
			if rng.Intn(10) == 0 {
				x = nil
			}
			for _, tb := range []string{"a", "b"} {
				tx.Exec(`INSERT INTO `+tb+` (x, y, z) VALUES (?, ?, ?)`, x, y, z)
			}
		}
		return nil
	})
	conds := []string{"x = ?", "y = ?", "z > ?", "x IN (?, ?)", "id = ?", "z BETWEEN ? AND ?", "x = ? AND y = ?"}
	argFor := func(c string) []any {
		var out []any
		for i := 0; i < strings.Count(c, "?"); i++ {
			switch {
			case strings.HasPrefix(c, "y"):
				out = append(out, fmt.Sprintf("s%d", rng.Intn(30)))
			case strings.HasPrefix(c, "z"):
				out = append(out, float64(rng.Intn(40))/4)
			default:
				out = append(out, int64(rng.Intn(60)))
			}
		}
		return out
	}
	for k := 0; k < 300; k++ {
		var parts []string
		var args []any
		for i := 0; i < 2+rng.Intn(2); i++ {
			c := conds[rng.Intn(len(conds))]
			parts = append(parts, "("+c+")")
			args = append(args, argFor(c)...)
		}
		where := strings.Join(parts, " OR ")
		qa := `SELECT id FROM a WHERE ` + where + ` ORDER BY id`
		qb := strings.Replace(qa, "FROM a", "FROM b", 1)
		if ga, gb := query(t, db, qa, args...), query(t, db, qb, args...); ga != gb {
			t.Fatalf("%s %v\nunion: %.200s\nscan:  %.200s", where, args, ga, gb)
		}
	}
	rows, _ := db.Query(`EXPLAIN SELECT * FROM a WHERE x = 1 OR y = 's2'`)
	all, _ := rows.All()
	if p := fmt.Sprint(all); !strings.Contains(p, "USING UNION") {
		t.Errorf("plan: %s", p)
	}
}

func TestHashJoinMatchesNestedLoop(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, k); CREATE TABLE b (id INTEGER PRIMARY KEY, k)`)
	rng := rand.New(rand.NewSource(9))
	key := func() any {
		switch rng.Intn(6) {
		case 0:
			return nil
		case 1:
			return float64(rng.Intn(20)) // integral reals equal integers
		case 2:
			return float64(rng.Intn(20)) + 0.5
		case 3:
			return fmt.Sprint(rng.Intn(20)) // text never equals a number
		}
		return int64(rng.Intn(20))
	}
	db.Update(func(tx *Tx) error {
		for i := 0; i < 400; i++ {
			tx.Exec(`INSERT INTO a (k) VALUES (?)`, key())
			tx.Exec(`INSERT INTO b (k) VALUES (?)`, key())
		}
		return nil
	})
	pairs := [][2]string{
		{`SELECT a.id, b.id FROM a JOIN b ON b.k = a.k ORDER BY 1, 2`,
			`SELECT a.id, b.id FROM a JOIN b ON (b.k = a.k) = 1 ORDER BY 1, 2`},
		{`SELECT a.id, b.id FROM a LEFT JOIN b ON b.k = a.k WHERE a.id < 100 ORDER BY 1, 2`,
			`SELECT a.id, b.id FROM a LEFT JOIN b ON (b.k = a.k) = 1 WHERE a.id < 100 ORDER BY 1, 2`},
		{`SELECT a.id, s.n FROM a JOIN (SELECT k, COUNT(*) n FROM b GROUP BY k) s ON s.k = a.k ORDER BY 1`,
			`SELECT a.id, s.n FROM a JOIN (SELECT k, COUNT(*) n FROM b GROUP BY k) s ON (s.k = a.k) = 1 ORDER BY 1`},
	}
	for _, p := range pairs {
		if h, n := query(t, db, p[0]), query(t, db, p[1]); h != n {
			t.Fatalf("%s\nhash:   %.200s\nnested: %.200s", p[0], h, n)
		}
	}
	for q, want := range map[string]string{
		pairs[0][0]: "SCAN b USING HASH (k=?)",
		pairs[2][0]: "SCAN SUBQUERY s USING HASH (k=?)",
	} {
		rows, _ := db.Query("EXPLAIN " + q)
		all, _ := rows.All()
		if p := fmt.Sprint(all); !strings.Contains(p, want) {
			t.Errorf("plan %s, want %s", p, want)
		}
	}
}
