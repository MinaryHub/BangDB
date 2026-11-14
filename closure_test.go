package bangdb

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/knoxxr/bangdb/internal/value"
)

// expectedClosure computes the closure of a table's child -> parent edges
// the slow way: a breadth-first search up from every node that is a child.
// Lines are "anc|des|depth", sorted.
func expectedClosure(t *testing.T, db *DB, source, child, parent string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("SELECT %s, %s FROM %s", child, parent, source))
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string][]string{}
	nodes := map[string]bool{}
	for rows.Next() {
		v := rows.Values()
		if v[0] == nil {
			continue
		}
		c := value.Format(v[0])
		nodes[c] = true
		if v[1] != nil {
			parents[c] = append(parents[c], value.Format(v[1]))
		}
	}
	var out []string
	for n := range nodes {
		dist := map[string]int{n: 0}
		queue := []string{n}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, p := range parents[cur] {
				if _, seen := dist[p]; !seen {
					dist[p] = dist[cur] + 1
					queue = append(queue, p)
				}
			}
		}
		for a, d := range dist {
			out = append(out, fmt.Sprintf("%s|%s|%d", a, n, d))
		}
	}
	sort.Strings(out)
	return out
}

func actualClosure(t *testing.T, db *DB, name string) []string {
	t.Helper()
	got := query(t, db, "SELECT anc, des, depth FROM "+name)
	if got == "" {
		return nil
	}
	lines := strings.Split(got, ";")
	sort.Strings(lines)
	return lines
}

func checkClosure(t *testing.T, db *DB, name, source, child, parent, when string) {
	t.Helper()
	want := expectedClosure(t, db, source, child, parent)
	got := actualClosure(t, db, name)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s: closure %s has %d rows, want %d\n got %v\nwant %v", when, name, len(got), len(want), got, want)
	}
}

func TestClosureOfATreeTable(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE class (id INTEGER PRIMARY KEY, parent INT REFERENCES class (id) ON DELETE CASCADE, label TEXT);
		INSERT INTO class VALUES (1, NULL, 'Thing'), (2, 1, 'Part'), (3, 2, 'Fastener'), (4, 3, 'Bolt'), (5, 3, 'Nut'), (6, 1, 'Material');
		CREATE CLOSURE class_closure ON class (id, parent)`)
	checkClosure(t, db, "class_closure", "class", "id", "parent", "create")
	expect(t, db, "SELECT des FROM class_closure WHERE anc = 2 ORDER BY des", "2;3;4;5")
	expect(t, db, "SELECT anc, depth FROM class_closure WHERE des = 4 ORDER BY depth", "4|0;3|1;2|2;1|3")
	expect(t, db, "EXPLAIN SELECT des FROM class_closure WHERE anc = 2", "SEARCH class_closure USING COVERING INDEX bangdb_autoindex_class_closure_pk (anc=?)")
	expect(t, db, "EXPLAIN SELECT anc FROM class_closure WHERE des = 4", "SEARCH class_closure USING COVERING INDEX class_closure_des (des=?)")

	exec(t, db, `INSERT INTO class VALUES (7, 4, 'HexBolt')`)
	checkClosure(t, db, "class_closure", "class", "id", "parent", "insert leaf")
	exec(t, db, `UPDATE class SET parent = 6 WHERE id = 3`) // move a subtree
	checkClosure(t, db, "class_closure", "class", "id", "parent", "move subtree")
	exec(t, db, `UPDATE class SET label = 'Fasteners' WHERE id = 3`) // no structural change
	checkClosure(t, db, "class_closure", "class", "id", "parent", "relabel")
	exec(t, db, `DELETE FROM class WHERE id = 3`) // cascades to 4, 5, 7
	expect(t, db, "SELECT COUNT(*) FROM class", "3")
	checkClosure(t, db, "class_closure", "class", "id", "parent", "cascade delete")

	// the closure table is maintained, not written
	for _, q := range []string{
		"INSERT INTO class_closure VALUES (1, 1, 0)",
		"DELETE FROM class_closure",
		"UPDATE class_closure SET depth = 0",
		"DROP TABLE class_closure",
		"DROP TABLE class",
		"ALTER TABLE class DROP COLUMN parent",
		"DROP INDEX class_closure_des",
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
	// a cycle fails the statement and changes nothing
	if _, err := db.Exec(`UPDATE class SET parent = 6 WHERE id = 1`); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := db.Exec(`UPDATE class SET parent = 2 WHERE id = 2`); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("self loop: %v", err)
	}
	expect(t, db, "SELECT parent FROM class WHERE id = 1", "NULL")
	checkClosure(t, db, "class_closure", "class", "id", "parent", "after refused cycle")

	// a rolled back transaction leaves the closure as it was
	err := db.Update(func(tx *Tx) error {
		if _, err := tx.Exec(`INSERT INTO class VALUES (8, 6, 'Steel'), (9, 8, 'S235')`); err != nil {
			return err
		}
		r := tx.QueryRow(`SELECT COUNT(*) FROM class_closure WHERE anc = 6`)
		var n int64
		if err := r.Scan(&n); err != nil || n != 3 {
			return fmt.Errorf("inside the transaction: %d rows, %v", n, err)
		}
		return fmt.Errorf("roll back")
	})
	if err == nil || err.Error() != "roll back" {
		t.Fatal(err)
	}
	checkClosure(t, db, "class_closure", "class", "id", "parent", "rollback")

	// the integrity check compares the closure with its source
	tx, err := db.Engine().Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.ForceClosureWrite(true)
	if _, err := tx.Exec(`UPDATE class_closure SET depth = 9 WHERE anc = 1 AND des = 2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.IntegrityCheck(); len(p) != 1 || !strings.Contains(p[0], "depth of (1, 2) is 9, should be 1") {
		t.Fatalf("integrity check of a damaged closure: %v", p)
	}
	tx, _ = db.Engine().Begin()
	tx.ForceClosureWrite(true)
	tx.Exec(`UPDATE class_closure SET depth = 1 WHERE anc = 1 AND des = 2`)
	tx.Commit()

	exec(t, db, `DROP CLOSURE class_closure`)
	expect(t, db, "SELECT COUNT(*) FROM class", "3")
	if _, err := db.Exec(`SELECT * FROM class_closure`); err == nil {
		t.Error("closure table still there")
	}
	exec(t, db, `DROP TABLE class`) // no longer a source
}

func TestClosureOfAnEdgeTableWithMultipleInheritance(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE subclass (child TEXT NOT NULL, parent TEXT, UNIQUE (child, parent));
		CREATE CLOSURE sc ON subclass (child, parent);
		INSERT INTO subclass VALUES ('ex:Bolt', 'ex:Fastener'), ('ex:Bolt', 'ex:ThreadedPart'),
			('ex:Fastener', 'ex:Part'), ('ex:ThreadedPart', 'ex:Part'), ('ex:Part', NULL)`)
	checkClosure(t, db, "sc", "subclass", "child", "parent", "diamond")
	// two paths Bolt -> Part, the shortest counts
	expect(t, db, "SELECT depth FROM sc WHERE anc = 'ex:Part' AND des = 'ex:Bolt'", "2")
	exec(t, db, `DELETE FROM subclass WHERE child = 'ex:Bolt' AND parent = 'ex:Fastener'`)
	checkClosure(t, db, "sc", "subclass", "child", "parent", "remove one of two paths")
	expect(t, db, "SELECT COUNT(*) FROM sc WHERE anc = 'ex:Part' AND des = 'ex:Bolt'", "1")
	if _, err := db.Exec(`INSERT INTO subclass VALUES ('ex:Part', 'ex:Bolt')`); err == nil {
		t.Fatal("cycle accepted")
	}
	checkClosure(t, db, "sc", "subclass", "child", "parent", "after refused cycle")

	// rename the source and its columns: the closure follows
	exec(t, db, `ALTER TABLE subclass RENAME TO sub_of; ALTER TABLE sub_of RENAME COLUMN parent TO super`)
	exec(t, db, `INSERT INTO sub_of VALUES ('ex:Washer', 'ex:Fastener')`)
	checkClosure(t, db, "sc", "sub_of", "child", "super", "after renames")
	if !strings.Contains(db.Engine().Catalog().DescribeClosure("sc"), `ON sub_of (child, super)`) {
		t.Errorf("describe: %s", db.Engine().Catalog().DescribeClosure("sc"))
	}
	expect(t, db, "PRAGMA schema('sc')", "closure|sc|sub_of|CREATE CLOSURE sc ON sub_of (child, super);")
}

func TestClosureRandomEdits(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE e (child INT, parent INT); CREATE CLOSURE ec ON e (child, parent)`)
	rng := rand.New(rand.NewSource(3))
	const n = 40
	for step := 0; step < 400; step++ {
		var q string
		var args []any
		switch r := rng.Intn(10); {
		case r < 4: // a new edge pointing to a smaller node (keeps it acyclic)
			c := 1 + rng.Intn(n)
			p := rng.Intn(c) // 0 means a root row
			q, args = `INSERT INTO e VALUES (?, ?)`, []any{c, p}
			if p == 0 {
				args[1] = nil
			}
		case r < 6:
			var cnt int64
			db.QueryRow(`SELECT COUNT(*) FROM e`).Scan(&cnt)
			q, args = `DELETE FROM e WHERE rowid IN (SELECT rowid FROM e LIMIT 1 OFFSET ?)`, []any{rng.Int63n(cnt + 1)}
		case r < 7:
			q, args = `DELETE FROM e WHERE child = ?`, []any{1 + rng.Intn(n)}
		case r < 8: // re-parent all edges of a node, maybe making a cycle
			q, args = `UPDATE e SET parent = ? WHERE child = ?`, []any{1 + rng.Intn(n), 1 + rng.Intn(n)}
		case r < 9:
			q, args = `UPDATE e SET child = ? WHERE child = ?`, []any{1 + rng.Intn(n), 1 + rng.Intn(n)}
		default: // several changes in one statement
			q, args = `INSERT INTO e SELECT child + 1, parent FROM e WHERE child < 39 AND parent IS NOT NULL LIMIT 3 OFFSET ?`, []any{rng.Intn(20)}
		}
		if _, err := db.Exec(q, args...); err != nil && !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("step %d %s %v: %v", step, q, args, err)
		}
		checkClosure(t, db, "ec", "e", "child", "parent", fmt.Sprintf("step %d: %s %v", step, q, args))
	}
}

func TestClosureSurvivesReopenDumpAndVacuum(t *testing.T) {
	durableOnly(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "o.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `CREATE TABLE class (id INTEGER PRIMARY KEY, parent INT);
		INSERT INTO class VALUES (1, NULL), (2, 1), (3, 2), (4, 1);
		CREATE CLOSURE cc ON class (id, parent);
		CREATE INDEX cc_depth ON cc (depth)`)
	db.Close()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO class VALUES (5, 3)`)
	checkClosure(t, db, "cc", "class", "id", "parent", "after reopen")
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO class VALUES (6, 5)`)
	checkClosure(t, db, "cc", "class", "id", "parent", "after vacuum")
	checkIntegrity(t, db)

	var dump bytes.Buffer
	if err := db.Dump(context.Background(), &dump); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if !strings.Contains(dump.String(), "CREATE CLOSURE cc ON class (id, parent);") ||
		strings.Contains(dump.String(), "INSERT INTO cc ") || !strings.Contains(dump.String(), "CREATE INDEX cc_depth") {
		t.Fatalf("dump:\n%s", dump.String())
	}
	db2 := open(t)
	if _, err := db2.ImportSQL(context.Background(), &dump); err != nil {
		t.Fatal(err)
	}
	checkClosure(t, db2, "cc", "class", "id", "parent", "after import")
	exec(t, db2, `INSERT INTO class VALUES (7, 6)`)
	checkClosure(t, db2, "cc", "class", "id", "parent", "insert after import")
	os.Remove(path)
}

func TestClosureLargeHierarchy(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}
	db := open(t)
	exec(t, db, `CREATE TABLE class (id INTEGER PRIMARY KEY, parent INT, label TEXT)`)
	rng := rand.New(rand.NewSource(1))
	const n = 50000
	err := db.Update(func(tx *Tx) error {
		for i := 1; i <= n; i++ {
			var p any
			if i > 1 {
				p = 1 + rng.Intn(i-1)
			}
			if _, err := tx.Exec(`INSERT INTO class VALUES (?, ?, ?)`, i, p, fmt.Sprint("c", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	exec(t, db, `CREATE CLOSURE cc ON class (id, parent)`)
	build := time.Since(t0)
	var rows int64
	db.QueryRow(`SELECT COUNT(*) FROM cc`).Scan(&rows)
	t0 = time.Now()
	got := query(t, db, `SELECT COUNT(*) FROM cc WHERE anc = 7`)
	lookup := time.Since(t0)
	t0 = time.Now()
	exec(t, db, `INSERT INTO class VALUES (?, 7, 'new leaf')`, n+1)
	leaf := time.Since(t0)
	t0 = time.Now()
	exec(t, db, `UPDATE class SET label = 'renamed' WHERE id = 1`)
	relabel := time.Since(t0)
	t0 = time.Now()
	exec(t, db, `UPDATE class SET parent = 3 WHERE id = 7`) // move a subtree
	move := time.Since(t0)
	t.Logf("%d classes: build %v (%d closure rows), descendants of 7 = %s in %v, add leaf %v, relabel root %v, move subtree of 7 %v",
		n, build, rows, got, lookup, leaf, relabel, move)
	if relabel > 50*time.Millisecond {
		t.Errorf("relabelling the root took %v: a non-structural change must not recompute the closure", relabel)
	}
	checkClosure(t, db, "cc", "class", "id", "parent", "large")
}
