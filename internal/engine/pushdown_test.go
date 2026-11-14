package engine

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func pushdownDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE class (id INTEGER PRIMARY KEY, parent INT, label TEXT COLLATE NOCASE, kind TEXT);
		CREATE INDEX class_parent ON class (parent);
		CREATE VIEW closure AS
			WITH RECURSIVE r (anc, des, depth) AS (
				SELECT id, id, 0 FROM class
				UNION ALL
				SELECT r.anc, c.id, r.depth + 1 FROM class c JOIN r ON c.parent = r.des)
			SELECT anc, des, depth FROM r;
		CREATE VIEW closure_u AS
			WITH RECURSIVE r (anc, des) AS (
				SELECT id, id FROM class
				UNION
				SELECT anc, c.id FROM class c JOIN r ON c.parent = r.des)
			SELECT anc, des FROM r;
		CREATE VIEW up AS
			WITH RECURSIVE u (des, anc) AS (
				SELECT id, parent FROM class WHERE parent IS NOT NULL
				UNION ALL
				SELECT u.des, c.parent FROM class c JOIN u ON c.id = u.anc WHERE c.parent IS NOT NULL)
			SELECT des, anc FROM u;
		CREATE VIEW closure2 AS SELECT anc AS a, des AS d FROM closure;
		CREATE VIEW labels AS
			SELECT id, label, kind FROM class WHERE id % 2 = 0
			UNION SELECT id, upper(label), kind FROM class WHERE id % 3 = 0;
		CREATE VIEW kinds AS SELECT kind, COUNT(*) AS n, MAX(id) AS top FROM class GROUP BY kind;
		CREATE VIEW ranked AS SELECT id, kind, ROW_NUMBER() OVER (PARTITION BY kind ORDER BY id) AS rn FROM class;
		CREATE VIEW firstfew AS SELECT id, kind FROM class ORDER BY id LIMIT 50;
		CREATE VIEW everything AS SELECT * FROM class;`)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 300; i++ {
		var parent any
		if i > 1 {
			parent = int64(1 + rng.Intn(i-1))
		}
		label := fmt.Sprintf("Cls%d", i)
		if rng.Intn(2) == 0 {
			label = strings.ToLower(label)
		}
		kind := []string{"a", "b", "c"}[rng.Intn(3)]
		if _, err := tx.Exec(`INSERT INTO class VALUES (?, ?, ?, ?)`, i, parent, label, kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

// sortedRows renders a result as sorted lines, for order-insensitive
// comparison.
func sortedRows(r Result) []string {
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		out[i] = fmt.Sprintf("%#v", row)
	}
	sort.Strings(out)
	return out
}

func TestPushdownGivesTheSameRows(t *testing.T) {
	db := pushdownDB(t)
	queries := []struct {
		sql  string
		args []any
	}{
		{"SELECT des FROM closure WHERE anc = 7", nil},
		{"SELECT des, depth FROM closure WHERE anc IN (3, 9, 12) AND depth > 1", nil},
		{"SELECT * FROM closure WHERE des = 40", nil},
		{"SELECT des FROM closure WHERE anc = ?", []any{11}},
		{"SELECT des FROM closure WHERE anc = 3 + 4", nil},
		{"SELECT des FROM closure WHERE anc = 7.0", nil},
		{"SELECT des FROM closure WHERE anc = '7'", nil},
		{"SELECT des FROM closure WHERE 5 = anc", nil},
		{"SELECT des FROM closure_u WHERE anc = 7", nil},
		{"SELECT anc FROM up WHERE des = 50", nil},
		{"SELECT d FROM closure2 WHERE a = 7", nil},
		{"SELECT c.label, x.des FROM class c JOIN closure x ON x.des = c.id WHERE x.anc = ?", []any{5}},
		{"SELECT c.id, x.des FROM class c LEFT JOIN closure x ON x.anc = c.id AND x.anc = 3", nil},
		{"SELECT c.id, x.des FROM class c LEFT JOIN closure x ON x.anc = c.id WHERE x.anc = 3", nil},
		{"SELECT * FROM labels WHERE label = 'cls6'", nil},
		{"SELECT * FROM labels WHERE label = 'CLS9'", nil},
		{"SELECT * FROM labels WHERE id IN (6, 9, 12, 13)", nil},
		{"SELECT * FROM labels WHERE id = '6'", nil},
		{"SELECT * FROM kinds WHERE kind = 'a'", nil},
		{"SELECT * FROM kinds WHERE n > 0 AND kind IN ('b', 'c')", nil},
		{"SELECT * FROM ranked WHERE kind = 'b' AND rn <= 3", nil},
		{"SELECT * FROM firstfew WHERE kind = 'a'", nil},
		{"SELECT * FROM everything WHERE id = 4", nil},
		{"WITH t AS (SELECT id, kind FROM class WHERE id > 10) SELECT * FROM t WHERE kind = 'b'", nil},
		{"WITH t AS (SELECT id, kind FROM class) SELECT a.id, b.id FROM t a JOIN t b ON a.id = b.id + 1 WHERE a.kind = 'a'", nil},
		{"WITH t AS (SELECT id, kind FROM class) SELECT a.id FROM t a WHERE a.kind = 'c' UNION ALL SELECT id FROM t", nil},
		{"SELECT * FROM (SELECT id, parent AS p FROM class) d WHERE d.p = 3", nil},
		{"SELECT * FROM (SELECT id, parent AS p FROM class) d WHERE p IN (3, 4)", nil},
		{"SELECT * FROM (SELECT id, parent AS p FROM class LIMIT 20) d WHERE p = 1", nil},
		{"WITH RECURSIVE r (anc, des) AS (SELECT id, id FROM class UNION ALL SELECT r.anc, c.id FROM class c JOIN r ON c.parent = r.des) SELECT COUNT(*) FROM r WHERE anc = 2", nil},
		{"WITH RECURSIVE r (n, root) AS (SELECT id, id FROM class WHERE parent = 1 UNION ALL SELECT c.id, r.root FROM class c, r WHERE c.parent = r.n) SELECT n FROM r WHERE root = 4", nil},
		{"WITH RECURSIVE r (anc, des) AS (SELECT id, id FROM class UNION ALL SELECT r.anc, c.id FROM class c JOIN r ON c.parent = r.des ORDER BY 2 DESC) SELECT des FROM r WHERE anc = 6", nil},
	}
	for _, q := range queries {
		pushdownOff = true
		want, err := db.Query(q.sql, q.args...)
		pushdownOff = false
		if err != nil {
			t.Fatalf("%s (without push-down): %v", q.sql, err)
		}
		got, err := db.Query(q.sql, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
		w, g := sortedRows(want), sortedRows(got)
		if strings.Join(w, "\n") != strings.Join(g, "\n") {
			t.Errorf("%s: %d rows, want %d\n got %v\nwant %v", q.sql, len(g), len(w), g, w)
		}
	}
}

func TestPushdownIsShownAndSkippedWhereUnsafe(t *testing.T) {
	db := pushdownDB(t)
	plan := func(q string) string {
		r, err := db.Query("EXPLAIN " + q)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, row := range r.Rows {
			lines = append(lines, fmt.Sprint(row[0]))
		}
		return strings.Join(lines, "\n")
	}
	for q, want := range map[string]string{
		"SELECT des FROM closure WHERE anc = 7":                         "SCAN SUBQUERY closure (pushed anc=?)",
		"SELECT des FROM closure WHERE anc IN (1, 2)":                   "(pushed anc IN (...))",
		"SELECT * FROM kinds WHERE kind = 'a'":                          "(pushed kind=?)",
		"SELECT * FROM (SELECT id, parent AS p FROM class) d WHERE p=3": "(pushed p=?)",
	} {
		if p := plan(q); !strings.Contains(p, want) {
			t.Errorf("EXPLAIN %s:\n%s\nwant %q", q, p, want)
		}
	}
	for _, q := range []string{
		"SELECT * FROM ranked WHERE kind = 'b'",                                   // window function
		"SELECT * FROM firstfew WHERE kind = 'a'",                                 // LIMIT
		"SELECT * FROM everything WHERE id = 4",                                   // * item
		"SELECT * FROM closure WHERE des = 40",                                    // not a pass-through column of the recursion
		"SELECT x.des FROM class c JOIN closure x ON x.anc = c.id WHERE c.id = 3", // value from another table
		"SELECT des FROM closure WHERE anc = abs(-7)",                             // function call
	} {
		if p := plan(q); strings.Contains(p, "pushed") && !strings.Contains(q, "des = 40") {
			t.Errorf("EXPLAIN %s pushed a condition it must not:\n%s", q, p)
		}
	}
	// des = 40 is pushed into the view (SELECT anc, des, depth FROM r) but not
	// further into the recursive table: the plan of the view names it, the
	// result is checked in TestPushdownGivesTheSameRows.
}
