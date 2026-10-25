package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/value"
)

func checkDB(t *testing.T) *DB {
	db, err := Open(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, body TEXT);
		CREATE INDEX t_body ON t (body);
		INSERT INTO t (name, body) VALUES ('a', 'x'), ('b', 'y'), ('c', 'z')`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func problems(t *testing.T, db *DB) string {
	p, err := db.IntegrityCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(p, "\n")
}

// damage runs fn in a raw write transaction, bypassing all SQL checks.
func damage(t *testing.T, db *DB, fn func(tx *Tx)) {
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityCheckFindsDamage(t *testing.T) {
	db := checkDB(t)
	if p := problems(t, db); p != "" {
		t.Fatalf("healthy database reported: %s", p)
	}

	cases := []struct {
		name string
		fn   func(tx *Tx)
		want string
	}{
		{"missing index entry", func(tx *Tx) {
			ix := tx.cat.index("t_body")
			k, _ := indexEntry("y", 2)
			btree.Open(tx.v, ix.Root).Delete(k)
		}, "has 2 entries but table t has 3 rows"},
		{"stale index value", func(tx *Tx) {
			tbl := tx.cat.table("t")
			btree.Open(tx.v, tbl.Root).Put(value.EncodeRowID(3), value.EncodeRow([]any{int64(3), "c", "changed"}))
		}, "does not match the row's body"},
		{"duplicate in unique index", func(tx *Tx) {
			tbl := tx.cat.table("t")
			btree.Open(tx.v, tbl.Root).Put(value.EncodeRowID(4), value.EncodeRow([]any{int64(4), "a", "w"}))
			for _, ix := range tbl.indexes {
				k, _ := indexEntry([]any{int64(4), "a", "w"}[ix.col], 4)
				btree.Open(tx.v, ix.Root).Put(k, nil)
			}
		}, "has duplicate value a"},
		{"lost page", func(tx *Tx) { tx.v.Alloc() }, "is not used by anything"},
		{"page used twice", func(tx *Tx) {
			tbl := tx.cat.table("t")
			tx.v.Free(tbl.Root)
		}, "is used by both"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := checkDB(t)
			damage(t, db, c.fn)
			if p := problems(t, db); !strings.Contains(p, c.want) {
				t.Fatalf("want a problem containing %q, got:\n%s", c.want, p)
			}
		})
	}
}
