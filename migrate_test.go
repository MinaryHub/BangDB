package bangdb

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPragmas(t *testing.T) {
	db := setupShop(t)
	expect(t, db, "PRAGMA user_version", "0")
	exec(t, db, "PRAGMA user_version = 7")
	expect(t, db, "PRAGMA user_version", "7")
	expect(t, db, "PRAGMA table_info(users)", "0|id|INTEGER|0|NULL|1;1|name|TEXT|1|NULL|0;2|email|TEXT|0|NULL|0;3|age|INT|0|30|0")
	expect(t, db, "PRAGMA index_list(orders)", "0|idx_orders_user|0|c|0")
	expect(t, db, "PRAGMA index_info('idx_orders_user')", "0|1|user_id")
	expect(t, db, "PRAGMA table_list", "orders|table|btree|4;users|table|btree|4")
	expect(t, db, "PRAGMA integrity_check", "ok")
	expect(t, db, "PRAGMA foreign_keys = ON", "1")
	expect(t, db, "PRAGMA journal_mode", "wal")
	for q, want := range map[string]string{
		"PRAGMA foreign_keys = OFF": "always enforced",
		"PRAGMA user_version = 'x'": "32-bit",
		"PRAGMA table_info(nope)":   "no such table",
		"PRAGMA no_such_thing":      "unknown pragma",
	} {
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
	// The version travels with dumps.
	var buf bytes.Buffer
	if err := db.Dump(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	db2 := open(t)
	if _, err := db2.ImportSQL(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	expect(t, db2, "PRAGMA user_version", "7")
}

func TestMigrate(t *testing.T) {
	durableOnly(t)
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	steps := []Migration{
		{Version: 2, Name: "email", SQL: "ALTER TABLE users ADD COLUMN email TEXT"},
		{Version: 1, Name: "users", SQL: "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
	}
	ctx := context.Background()
	applied, err := db.Migrate(ctx, steps)
	if err != nil || !reflect.DeepEqual(applied, []int{1, 2}) {
		t.Fatal(applied, err)
	}
	expect(t, db, "PRAGMA user_version", "2")
	if applied, err = db.Migrate(ctx, steps); err != nil || len(applied) != 0 {
		t.Fatalf("second run applied %v %v", applied, err)
	}
	// A failing step is rolled back and stops the run; earlier steps stay.
	steps = append(steps,
		Migration{Version: 3, Func: func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO users (name) VALUES ('seed')")
			return err
		}},
		Migration{Version: 4, Name: "broken", SQL: "CREATE TABLE ok (x); CREATE TABLE users (x)"},
		Migration{Version: 5, SQL: "CREATE TABLE never (x)"},
	)
	applied, err = db.Migrate(ctx, steps)
	if err == nil || !strings.Contains(err.Error(), "migration broken") || !reflect.DeepEqual(applied, []int{3}) {
		t.Fatalf("failing run: %v %v", applied, err)
	}
	expect(t, db, "PRAGMA user_version", "3")
	expect(t, db, "SELECT name FROM users", "seed")
	if db.Engine().Catalog().Describe("ok") != "" {
		t.Fatal("the failed step was not rolled back")
	}
	db.Close()
	db, _ = Open(path)
	defer db.Close()
	expect(t, db, "PRAGMA user_version", "3")
	if _, err := db.Migrate(ctx, []Migration{{Version: 1}, {Version: 1}}); err == nil {
		t.Fatal("duplicate versions accepted")
	}
	if _, err := db.Migrate(ctx, []Migration{{Version: 0}}); err == nil {
		t.Fatal("version 0 accepted")
	}
}
