package bangdb

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestCompositeKeys(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE enroll (
			student TEXT, course TEXT, term INT, grade REAL,
			PRIMARY KEY (student, course, term)
		);
		CREATE TABLE seats (room TEXT, seat INT, holder TEXT, CONSTRAINT u UNIQUE (room, seat));
		CREATE INDEX enroll_course_term ON enroll (course, term DESC)`)
	exec(t, db, `INSERT INTO enroll VALUES ('kim','db',2025,4.0), ('kim','db',2026,3.5), ('lee','db',2026,3.0), ('kim','os',2026,NULL)`)
	if _, err := db.Exec(`INSERT INTO enroll VALUES ('kim','db',2026,1.0)`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("composite PK duplicate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO enroll VALUES ('kim',NULL,2026,1.0)`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("composite PK column must be NOT NULL: %v", err)
	}
	exec(t, db, `INSERT INTO seats VALUES ('A', 1, 'x'), ('A', 2, 'y'), ('B', 1, 'z'), ('A', NULL, 'p'), ('A', NULL, 'q')`)
	if _, err := db.Exec(`UPDATE seats SET seat = 1 WHERE holder = 'y'`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("composite UNIQUE on update: %v", err)
	}

	expect(t, db, `SELECT grade FROM enroll WHERE student = 'kim' AND course = 'db' AND term = 2026`, "3.5")
	expect(t, db, `SELECT student FROM enroll WHERE course = 'db' AND term >= 2026 ORDER BY student`, "kim;lee")
	expect(t, db, `SELECT term FROM enroll WHERE student = 'kim' AND course IN ('os', 'db') ORDER BY term, course`, "2025;2026;2026")

	plan := func(q string) string {
		rows, _ := db.Query("EXPLAIN " + q)
		all, _ := rows.All()
		return fmt.Sprint(all)
	}
	for q, want := range map[string]string{
		`SELECT * FROM enroll WHERE student = 'kim' AND course = 'db' AND term = 1`: "bangdb_autoindex_enroll_pk (student=? AND course=? AND term=?)",
		`SELECT * FROM enroll WHERE student = 'kim'`:                               "bangdb_autoindex_enroll_pk (student=?)",
		`SELECT * FROM enroll WHERE course = 'db' AND term > 2025`:                 "enroll_course_term RANGE (course=? AND term>?)",
		`SELECT * FROM enroll WHERE term = 2026`:                                   "SCAN enroll",
		`SELECT * FROM seats WHERE room = 'A' AND seat IN (1, 2)`:                  "u", // composite UNIQUE index
	} {
		if p := plan(q); !strings.Contains(p, want) {
			t.Errorf("%s\n plan %s\n want %s", q, p, want)
		}
	}
	schema := db.Engine().Catalog().Describe("enroll")
	if !strings.Contains(schema, "PRIMARY KEY (student, course, term)") || !strings.Contains(schema, "ON enroll (course, term)") {
		t.Errorf("describe:\n%s", schema)
	}
}

// TestCompositeIndexMatchesScan compares composite-index lookups with full
// scans on random data.
func TestCompositeIndexMatchesScan(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, x INT, y TEXT, z REAL);
		CREATE TABLE b (id INTEGER PRIMARY KEY, x INT, y TEXT, z REAL);
		CREATE INDEX a_xyz ON a (x, y, z)`)
	rng := rand.New(rand.NewSource(11))
	val := func(col string) any {
		if rng.Intn(8) == 0 {
			return nil
		}
		switch col {
		case "x":
			return int64(rng.Intn(6))
		case "y":
			return fmt.Sprintf("s%d", rng.Intn(5))
		}
		return float64(rng.Intn(10)) / 2
	}
	db.Update(func(tx *Tx) error {
		for i := 0; i < 1500; i++ {
			x, y, z := val("x"), val("y"), val("z")
			for _, tb := range []string{"a", "b"} {
				tx.Exec(`INSERT INTO `+tb+` (x, y, z) VALUES (?, ?, ?)`, x, y, z)
			}
		}
		return nil
	})
	ops := []string{"=", "<", "<=", ">", ">="}
	for k := 0; k < 400; k++ {
		var conds []string
		var args []any
		conds = append(conds, "x = ?")
		args = append(args, val("x"))
		if rng.Intn(3) > 0 {
			conds = append(conds, "y = ?")
			args = append(args, val("y"))
			switch rng.Intn(3) {
			case 0:
				conds = append(conds, "z "+ops[rng.Intn(len(ops))]+" ?")
				args = append(args, val("z"))
			case 1:
				conds = append(conds, "z IN (?, ?)")
				args = append(args, val("z"), int64(rng.Intn(5)))
			}
		} else if rng.Intn(2) == 0 {
			conds = append(conds, "y "+ops[rng.Intn(len(ops))]+" ?")
			args = append(args, val("y"))
		}
		where := strings.Join(conds, " AND ")
		qa := query(t, db, `SELECT id, x, y, z FROM a WHERE `+where+` ORDER BY id`, args...)
		qb := query(t, db, `SELECT id, x, y, z FROM b WHERE `+where+` ORDER BY id`, args...)
		if qa != qb {
			t.Fatalf("mismatch for %s %v\nindex: %s\nscan:  %s", where, args, qa, qb)
		}
	}
}
