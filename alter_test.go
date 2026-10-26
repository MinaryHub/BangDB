package bangdb

import (
	"errors"
	"strings"
	"testing"
)

func TestAlterTable(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE people (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE, nick TEXT, age INT, note TEXT);
		CREATE INDEX people_age ON people (age);
		INSERT INTO people (name, nick, age, note) VALUES ('kim', 'k', 30, 'a'), ('lee', 'l', 40, 'b')`)

	// DROP COLUMN rewrites rows; indexes keep working.
	exec(t, db, `ALTER TABLE people DROP COLUMN note`)
	expect(t, db, `SELECT * FROM people ORDER BY id`, "1|kim|k|30;2|lee|l|40")
	expect(t, db, `SELECT name FROM people WHERE age = 40`, "lee")
	for _, q := range []string{
		`ALTER TABLE people DROP COLUMN id`,   // primary key
		`ALTER TABLE people DROP COLUMN name`, // UNIQUE
		`ALTER TABLE people DROP COLUMN age`,  // used by an index
		`ALTER TABLE people DROP COLUMN nope`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}

	// RENAME COLUMN keeps indexes and constraints.
	exec(t, db, `ALTER TABLE people RENAME COLUMN age TO years; ALTER TABLE people RENAME nick TO alias`)
	expect(t, db, `SELECT name, years, alias FROM people WHERE years > 35`, "lee|40|l")
	if _, err := db.Exec(`INSERT INTO people (name) VALUES ('kim')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("UNIQUE lost after rename: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE people RENAME COLUMN years TO name`); err == nil {
		t.Fatal("rename onto an existing column must fail")
	}

	// RENAME TO moves everything, including automatic indexes and the
	// AUTOINCREMENT high-water mark.
	exec(t, db, `DELETE FROM people WHERE id = 2`)
	exec(t, db, `ALTER TABLE people RENAME TO persons`)
	if _, err := db.Query(`SELECT * FROM people`); err == nil {
		t.Fatal("old name still resolves")
	}
	if r := exec(t, db, `INSERT INTO persons (name, years) VALUES ('park', 20)`); r.LastInsertID != 3 {
		t.Fatalf("AUTOINCREMENT after rename: %d", r.LastInsertID)
	}
	expect(t, db, `SELECT name FROM persons WHERE name = 'park'`, "park")
	// The old name can be reused with its own constraints.
	exec(t, db, `CREATE TABLE people (name TEXT UNIQUE)`)
	exec(t, db, `INSERT INTO people VALUES ('kim')`)
	if _, err := db.Exec(`INSERT INTO people VALUES ('kim')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("UNIQUE on the reused table name: %v", err)
	}
	schema := db.Engine().Catalog().Describe("persons")
	if !strings.Contains(schema, "years INT") || strings.Contains(schema, "note") {
		t.Fatalf("schema:\n%s", schema)
	}
}
