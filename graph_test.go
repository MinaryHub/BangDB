package bangdb

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestGraphWalkAndPath(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE part_of (part TEXT, whole TEXT);
		INSERT INTO part_of VALUES ('bolt', 'flange'), ('nut', 'flange'), ('flange', 'pump'),
			('impeller', 'pump'), ('pump', 'plant'), ('valve', 'plant'), ('plant', 'site'),
			('site', 'plant');  -- a cycle
		CREATE TABLE label (id TEXT PRIMARY KEY, name TEXT);
		INSERT INTO label VALUES ('bolt', '볼트'), ('impeller', '임펠러')`)
	expect(t, db, `SELECT node, depth, parent FROM graph_walk('part_of', 'part', 'whole', 'bolt')`,
		"bolt|0|NULL;flange|1|bolt;pump|2|flange;plant|3|pump;site|4|plant")
	expect(t, db, `SELECT node FROM graph_walk('part_of', 'part', 'whole', 'bolt', 2) WHERE depth > 0`, "flange;pump")
	expect(t, db, `SELECT node, depth FROM graph_walk('part_of', 'part', 'whole', 'pump', NULL, 'in') ORDER BY depth, node`,
		"pump|0;flange|1;impeller|1;bolt|2;nut|2")
	expect(t, db, `SELECT COUNT(*) FROM graph_walk('part_of', 'part', 'whole', 'nut', NULL, 'both')`, "8")
	expect(t, db, `SELECT node FROM graph_path('part_of', 'part', 'whole', 'nut', 'site') ORDER BY step`, "nut;flange;pump;plant;site")
	expect(t, db, `SELECT step, node FROM graph_path('part_of', 'part', 'whole', 'nut', 'bolt', NULL, 'both')`, "0|nut;1|flange;2|bolt")
	expect(t, db, `SELECT COUNT(*) FROM graph_path('part_of', 'part', 'whole', 'nut', 'bolt')`, "0")
	expect(t, db, `SELECT COUNT(*) FROM graph_path('part_of', 'part', 'whole', 'nut', 'site', 2)`, "0")
	expect(t, db, `SELECT node FROM graph_path('part_of', 'part', 'whole', 'site', 'site')`, "site")
	// arguments from the tables to the left, LIMIT stops the walk
	expect(t, db, `SELECT l.name, w.node FROM label l, graph_walk('part_of', 'part', 'whole', l.id, 1) w
		WHERE w.depth = 1 ORDER BY l.name`, "볼트|flange;임펠러|pump")
	expect(t, db, `SELECT node FROM graph_walk('part_of', 'part', 'whole', 'bolt') LIMIT 2`, "bolt;flange")
	// a view as the edge set
	exec(t, db, `CREATE VIEW into_pump AS SELECT part, whole FROM part_of WHERE whole = 'pump'`)
	expect(t, db, `SELECT node FROM graph_walk('into_pump', 'part', 'whole', 'pump', NULL, 'in') ORDER BY node`, "flange;impeller;pump")
	// errors
	for _, q := range []string{
		`SELECT * FROM graph_walk('nope', 'a', 'b', 1)`,
		`SELECT * FROM graph_walk('part_of', 'part', 'nope', 1)`,
		`SELECT * FROM graph_walk('part_of', 'part', 'whole', 1, -1)`,
		`SELECT * FROM graph_walk('part_of', 'part', 'whole', 1, 3, 'sideways')`,
		`SELECT * FROM graph_walk('part_of', 1, 'whole', 1)`,
	} {
		if _, err := db.Query(q); err == nil {
			rows, _ := db.Query(q)
			if rows != nil {
				for rows.Next() {
				}
				err = rows.Err()
			}
			if err == nil {
				t.Errorf("%s: accepted", q)
			}
		}
	}
}

// TestGraphWalkMatchesBreadthFirstSearch compares graph_walk with a search
// done one SQL lookup at a time, on a random graph with cycles, for every
// direction, several depth limits, and with and without indexes on the
// looked-up columns.
func TestGraphWalkMatchesBreadthFirstSearch(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE e (a INT, b INT); CREATE TABLE ei (a INT, b INT); CREATE INDEX ei_a ON ei (a); CREATE INDEX ei_b ON ei (b)`)
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 300; i++ {
		a, b := rng.Intn(80), rng.Intn(80)
		exec(t, db, `INSERT INTO e VALUES (?, ?)`, a, b)
		exec(t, db, `INSERT INTO ei VALUES (?, ?)`, a, b)
	}
	for _, dir := range []string{"out", "in", "both"} {
		for start := 0; start < 80; start += 7 {
			for _, depth := range []int{1, 3, -1} {
				var reach []string
				seen := map[int]int{start: 0}
				frontier := []int{start}
				for d := 0; depth < 0 || d < depth; d++ {
					var next []int
					for _, n := range frontier {
						q, args := map[string]string{
							"out":  `SELECT b FROM e WHERE a = ?`,
							"in":   `SELECT a FROM e WHERE b = ?`,
							"both": `SELECT b FROM e WHERE a = ? UNION ALL SELECT a FROM e WHERE b = ?`,
						}[dir], []any{n}
						if dir == "both" {
							args = append(args, n)
						}
						rows, err := db.Query(q, args...)
						if err != nil {
							t.Fatal(err)
						}
						for rows.Next() {
							var m int
							rows.Scan(&m)
							if _, ok := seen[m]; !ok {
								seen[m] = d + 1
								next = append(next, m)
							}
						}
						rows.Close()
					}
					if len(next) == 0 {
						break
					}
					frontier = next
				}
				for n, d := range seen {
					reach = append(reach, fmt.Sprintf("%d|%d", n, d))
				}
				sort.Strings(reach)
				for _, table := range []string{"e", "ei"} {
					var maxArg any
					if depth >= 0 {
						maxArg = depth
					}
					got := strings.Split(query(t, db, fmt.Sprintf(`SELECT node, depth FROM graph_walk('%s', 'a', 'b', ?, ?, ?)`, table), start, maxArg, dir), ";")
					sort.Strings(got)
					if strings.Join(got, ";") != strings.Join(reach, ";") {
						t.Fatalf("%s start %d depth %d dir %s:\n got %v\nwant %v", table, start, depth, dir, got, reach)
					}
				}
			}
		}
	}
}

func TestGraphWalkOnTriples(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}
	db := open(t)
	exec(t, db, `CREATE TABLE triple (s INT NOT NULL, p INT NOT NULL, o INT NOT NULL, PRIMARY KEY (s, p, o));
		CREATE INDEX t_pos ON triple (p, o, s); CREATE INDEX t_osp ON triple (o, s, p)`)
	rng := rand.New(rand.NewSource(1))
	db.Update(func(tx *Tx) error {
		for i := 0; i < 100000; i++ {
			tx.Exec(`INSERT OR IGNORE INTO triple VALUES (?, ?, ?)`, rng.Intn(20000), rng.Intn(20), rng.Intn(20000))
		}
		return nil
	})
	exec(t, db, `CREATE VIEW p3 AS SELECT s, o FROM triple WHERE p = 3`)
	var start int64
	db.QueryRow(`SELECT s FROM triple WHERE p = 3 GROUP BY s ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&start)
	t0 := time.Now()
	n := query(t, db, `SELECT COUNT(*) FROM graph_walk('p3', 's', 'o', ?, 6)`, start)
	view := time.Since(t0)
	t0 = time.Now()
	m := query(t, db, `SELECT COUNT(*) FROM graph_walk('triple', 's', 'o', ?, 3)`, start)
	table := time.Since(t0)
	t.Logf("100k triples: 6 hops over one predicate (view) %s nodes in %v; 3 hops over all predicates (indexed table) %s nodes in %v", n, view, m, table)
}
