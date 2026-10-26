package bangdb

import (
	"errors"
	"testing"
)

func TestCollations(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE u (id INTEGER PRIMARY KEY, email TEXT UNIQUE COLLATE NOCASE, name TEXT, tag TEXT COLLATE RTRIM);
		CREATE INDEX u_name ON u (name);
		INSERT INTO u (email, name, tag) VALUES ('Kim@X.io', 'Kim', 'a  '), ('lee@x.io', 'lee', 'b'), ('Park@x.io', 'park', 'c')`)
	// UNIQUE respects the column collation.
	if _, err := db.Exec(`INSERT INTO u (email, name) VALUES ('KIM@x.IO', 'dup')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("NOCASE UNIQUE: %v", err)
	}
	// Comparisons use the column collation, also through the index.
	expect(t, db, `SELECT id FROM u WHERE email = 'kim@x.io'`, "1")
	expect(t, db, `SELECT id FROM u WHERE email IN ('LEE@X.IO', 'nobody')`, "2")
	expect(t, db, `SELECT id FROM u WHERE email > 'L' ORDER BY id`, "2;3")
	expect(t, db, `SELECT id FROM u WHERE tag = 'a'`, "1")
	// Explicit COLLATE on a binary column, and BINARY overriding NOCASE.
	expect(t, db, `SELECT id FROM u WHERE name = 'KIM' COLLATE NOCASE`, "1")
	expect(t, db, `SELECT COUNT(*) FROM u WHERE name = 'KIM'`, "0")
	expect(t, db, `SELECT COUNT(*) FROM u WHERE email = 'kim@x.io' COLLATE BINARY`, "0")
	// ORDER BY / GROUP BY
	expect(t, db, `SELECT name FROM u ORDER BY name`, "Kim;lee;park")
	expect(t, db, `SELECT name FROM u ORDER BY name COLLATE NOCASE DESC`, "park;lee;Kim")
	exec(t, db, `CREATE TABLE g (w TEXT COLLATE NOCASE); INSERT INTO g VALUES ('a'), ('A'), ('b')`)
	expect(t, db, `SELECT COUNT(*) FROM g GROUP BY w ORDER BY 1`, "1;2")
	// LIKE is case-insensitive beyond ASCII.
	expect(t, db, `SELECT 'ÄRGER' LIKE 'är%', 'Straße' LIKE 'STRASSE'`, "1|0")
	if _, err := db.Exec(`CREATE TABLE bad (x TEXT COLLATE KOREAN)`); err == nil {
		t.Fatal("unknown collation must fail")
	}
}

func TestDateFunctions(t *testing.T) {
	db := open(t)
	for q, want := range map[string]string{
		`SELECT date('2026-10-25 13:45:10')`:                                 "2026-10-25",
		`SELECT time('2026-10-25T13:45:10Z')`:                                "13:45:10",
		`SELECT datetime('2026-01-31', '+1 months')`:                         "2026-03-03 00:00:00",
		`SELECT datetime('2026-10-25 13:45', '+90 minutes', '-1 day')`:       "2026-10-24 15:15:00",
		`SELECT date('2026-10-25', 'start of month', '+1 month', '-1 day')`:  "2026-10-31",
		`SELECT date('2026-10-25', 'start of year')`:                         "2026-01-01",
		`SELECT date('2026-10-25', 'weekday 1')`:                             "2026-10-26",
		`SELECT strftime('%Y/%m/%d %H:%M %j %w', '2026-10-25 07:05:00')`:     "2026/10/25 07:05 298 0",
		`SELECT unixepoch('2026-10-25 00:00:00')`:                            "1792886400",
		`SELECT datetime(1792886400, 'unixepoch')`:                           "2026-10-25 00:00:00",
		`SELECT julianday('2000-01-01 12:00:00')`:                            "2451545",
		`SELECT date('not a date'), date(NULL), date('2026-10-25', 'bogus')`: "NULL|NULL|NULL",
	} {
		expect(t, db, q, want)
	}
	exec(t, db, `CREATE TABLE ev (at TEXT); INSERT INTO ev VALUES ('2026-10-01 09:00:00'), ('2026-10-20 18:30:00'), ('2026-11-02 08:00:00')`)
	expect(t, db, `SELECT strftime('%Y-%m', at) m, COUNT(*) FROM ev GROUP BY m ORDER BY m`, "2026-10|2;2026-11|1")
	expect(t, db, `SELECT COUNT(*) FROM ev WHERE at >= datetime('2026-11-02', '-14 days')`, "2")
}
