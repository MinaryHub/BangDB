package bangdb

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// TestSpillToDisk runs sorting, DISTINCT and GROUP BY queries with a tiny
// sort memory limit, so they go through temporary files, and compares them
// with the in-memory results.
func TestSpillToDisk(t *testing.T) {
	mem, err := OpenWithOptions(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	disk, err := OpenWithOptions(":memory:", &Options{SortMemory: 4 << 10, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	for _, db := range []*DB{mem, disk} {
		exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, g INT, h TEXT, v REAL, s TEXT)`)
	}
	for _, db := range []*DB{mem, disk} {
		rng := rand.New(rand.NewSource(5))
		err := db.Update(func(tx *Tx) error {
			for i := 0; i < 6000; i++ {
				var v any = rng.Float64() * 100
				if i%17 == 0 {
					v = nil
				}
				_, err := tx.Exec(`INSERT INTO t (g, h, v, s) VALUES (?, ?, ?, ?)`,
					rng.Intn(1500), fmt.Sprintf("h%d", rng.Intn(40)), v, strings.Repeat("s", rng.Intn(30)))
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ordered := []string{
		`SELECT id, v FROM t ORDER BY v, id`,
		`SELECT id FROM t ORDER BY v DESC, s, id DESC`,
		`SELECT id, s FROM t ORDER BY length(s) DESC, id LIMIT 5000 OFFSET 7`,
		`SELECT id FROM t ORDER BY h, g LIMIT -1 OFFSET 100`,
		`SELECT DISTINCT g FROM t ORDER BY g`,
		`SELECT DISTINCT g, h FROM t ORDER BY h DESC, g LIMIT 900`,
		`SELECT g, count(*), sum(v), min(s), count(DISTINCT h) FROM t GROUP BY g ORDER BY g`,
		`SELECT h, g, avg(v) FROM t GROUP BY h, g HAVING count(*) > 1 ORDER BY 3 DESC, 1, 2`,
		`SELECT count(*) FROM (SELECT DISTINCT g, h FROM t) d`,
		`SELECT g FROM t GROUP BY g ORDER BY count(*) DESC, g LIMIT 10`,
	}
	for _, q := range ordered {
		if a, b := query(t, mem, q), query(t, disk, q); a != b {
			t.Fatalf("%s: results differ with spilling", q)
		}
	}
	unordered := []string{
		`SELECT DISTINCT g, h FROM t`,
		`SELECT g, count(*), group_concat(id) FROM t GROUP BY g`,
		`SELECT DISTINCT h FROM t WHERE g > 100`,
	}
	sorted := func(s string) string {
		rows := strings.Split(s, ";")
		sort.Strings(rows)
		return strings.Join(rows, ";")
	}
	for _, q := range unordered {
		if a, b := sorted(query(t, mem, q)), sorted(query(t, disk, q)); a != b {
			t.Fatalf("%s: results differ with spilling", q)
		}
	}
	// Streaming cursor over a spilled sort.
	rows, err := disk.Query(`SELECT id FROM t ORDER BY s, id`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if rows.Err() != nil || n != 6000 {
		t.Fatalf("streamed %d rows: %v", n, rows.Err())
	}
	rows.Close()
}
