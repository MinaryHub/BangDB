package bangdb

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

func setupJoinTables(t *testing.T, db *DB) {
	exec(t, db, `
		CREATE TABLE big (id INTEGER PRIMARY KEY, sid INT, status TEXT, pad TEXT);
		CREATE INDEX big_sid ON big (sid);
		CREATE INDEX big_status ON big (status);
		CREATE TABLE small (id INTEGER PRIMARY KEY, name TEXT, kind INT);`)
	tx, _ := db.Begin()
	for i := 0; i < 5000; i++ {
		status := "done"
		if i%500 == 0 {
			status = "open"
		}
		tx.Exec("INSERT INTO big (sid, status, pad) VALUES (?, ?, 'x')", i%50, status)
	}
	for i := 0; i < 50; i++ {
		tx.Exec("INSERT INTO small (id, name, kind) VALUES (?, ?, ?)", i, fmt.Sprintf("s%d", i), i%5)
	}
	tx.Commit()
}

func TestJoinReordering(t *testing.T) {
	db := open(t)
	setupJoinTables(t, db)
	// The selective table goes first even though it is written last.
	plan := query(t, db, "EXPLAIN SELECT big.id FROM big JOIN small ON big.sid = small.id WHERE small.name = 's7'")
	if !strings.HasPrefix(plan, "SCAN small;  SEARCH big USING INDEX big_sid") {
		t.Errorf("plan: %s", plan)
	}
	expect(t, db, "SELECT count(*) FROM big JOIN small ON big.sid = small.id WHERE small.name = 's7'", "100")
	// SELECT * keeps the FROM order of columns.
	cols := query(t, db, "SELECT * FROM big JOIN small ON big.sid = small.id WHERE small.name = 's7' AND big.id = 8")
	if cols != "8|7|done|x|7|s7|2" {
		t.Errorf("SELECT *: %s", cols)
	}
	// LEFT JOIN, and ORDER BY with LIMIT, keep the written order.
	plan = query(t, db, "EXPLAIN SELECT big.id FROM big LEFT JOIN small ON big.sid = small.id WHERE small.name = 's7'")
	if !strings.HasPrefix(plan, "SCAN big") {
		t.Errorf("LEFT JOIN plan: %s", plan)
	}
	plan = query(t, db, "EXPLAIN SELECT big.id FROM big JOIN small ON big.sid = small.id WHERE small.name = 's7' ORDER BY big.id LIMIT 5")
	if !strings.HasPrefix(plan, "SCAN big") {
		t.Errorf("ORDER BY/LIMIT plan: %s", plan)
	}
	// A good written order is kept.
	plan = query(t, db, "EXPLAIN SELECT big.id FROM small JOIN big ON big.sid = small.id WHERE small.kind = 1")
	if !strings.HasPrefix(plan, "SCAN small;  SEARCH big USING INDEX big_sid") {
		t.Errorf("kept plan: %s", plan)
	}
}

func TestAnalyze(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	setupJoinTables(t, db)
	exec(t, db, "ANALYZE")
	ix := db.Engine().Catalog().Index("big_status")
	if ix == nil || len(ix.Stats) != 1 || ix.Stats[0] != 2500 {
		t.Fatalf("big_status stats: %+v", ix)
	}
	if s := db.Engine().Catalog().Index("big_sid").Stats; len(s) != 1 || s[0] != 100 {
		t.Fatalf("big_sid stats: %v", s)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if s := db.Engine().Catalog().Index("big_status").Stats; len(s) != 1 {
		t.Fatal("statistics were not persisted")
	}
	exec(t, db, "ANALYZE small")
	if _, err := db.Exec("ANALYZE nope"); err == nil {
		t.Error("ANALYZE of a missing table succeeded")
	}
	// The statistics are averages: status has two values, so an equality
	// on it is estimated at 2500 rows, and the join by sid stays cheaper.
	plan := query(t, db, "EXPLAIN SELECT count(*) FROM small JOIN big ON big.sid = small.id WHERE big.status = 'open'")
	if !strings.HasPrefix(plan, "SCAN small;  SEARCH big USING INDEX big_sid") {
		t.Errorf("plan: %s", plan)
	}
	expect(t, db, "SELECT count(*) FROM small JOIN big ON big.sid = small.id WHERE big.status = 'open'", "10")
	checkIntegrity(t, db)
}

// Reordered joins return the same rows as the written order (forced with
// ORDER BY ... LIMIT, which keeps it).
func TestJoinReorderingResults(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, x INT, y INT);
		CREATE TABLE b (id INTEGER PRIMARY KEY, x INT, z INT); CREATE INDEX b_x ON b (x);
		CREATE TABLE c (id INTEGER PRIMARY KEY, y INT, z INT); CREATE INDEX c_z ON c (z)`)
	rng := rand.New(rand.NewSource(1))
	tx, _ := db.Begin()
	for i := 0; i < 400; i++ {
		tx.Exec("INSERT INTO a (x, y) VALUES (?, ?)", rng.Intn(40), rng.Intn(30))
	}
	for i := 0; i < 60; i++ {
		tx.Exec("INSERT INTO b (x, z) VALUES (?, ?)", rng.Intn(40), rng.Intn(20))
	}
	for i := 0; i < 2000; i++ {
		tx.Exec("INSERT INTO c (y, z) VALUES (?, ?)", rng.Intn(30), rng.Intn(20))
	}
	tx.Commit()
	exec(t, db, "ANALYZE")
	queries := []string{
		"SELECT a.id, b.id, c.id FROM a JOIN b ON a.x = b.x JOIN c ON c.z = b.z WHERE a.y = c.y",
		"SELECT a.id, b.id FROM c, b, a WHERE a.x = b.x AND b.z = c.z AND c.id < 50",
		"SELECT a.id, c.id FROM a JOIN c ON a.y = c.y WHERE a.x = 3",
		"SELECT b.id, count(*) FROM c JOIN b ON b.z = c.z JOIN a ON a.x = b.x GROUP BY b.id",
	}
	for _, q := range queries {
		got := query(t, db, q+" ORDER BY 1, 2")
		want := query(t, db, q+" ORDER BY 1, 2 LIMIT 100000000")
		if got != want {
			t.Errorf("%s: reordered result differs", q)
		}
		if got == "" {
			t.Errorf("%s: empty result", q)
		}
	}
}
