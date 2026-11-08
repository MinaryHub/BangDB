package bangdb

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/minaryhub/bangdb/internal/engine"
	"github.com/minaryhub/bangdb/internal/pager"
	"github.com/minaryhub/bangdb/internal/store"
	"github.com/minaryhub/bangdb/internal/store/storetest"
)

// reversedKind stores each row with its values in reverse order inside the
// default B+tree store. Code that bypassed the store interface and decoded
// the B+tree itself would see scrambled rows, so running the suite with it
//
//	BANGDB_TEST_STORE=reversed go test .
//
// checks that the executor reaches rows only through package store.
type reversedKind struct{ store.RowKind }

func (reversedKind) Name() string { return "reversed" }

func (k reversedKind) Open(v *pager.View, root uint32) store.TableStore {
	return reversedStore{k.RowKind.Open(v, root)}
}

type reversedStore struct{ store.TableStore }

func reversed(row []any) []any {
	out := slices.Clone(row)
	slices.Reverse(out)
	return out
}

func (s reversedStore) Get(rowid int64) ([]any, bool, error) {
	row, ok, err := s.TableStore.Get(rowid)
	return reversed(row), ok, err
}

func (s reversedStore) Put(rowid int64, row []any) error {
	return s.TableStore.Put(rowid, reversed(row))
}

func (s reversedStore) Scan(lo, hi int64, desc bool, fn func(int64, []any) (bool, error)) error {
	return s.TableStore.Scan(lo, hi, desc, func(id int64, row []any) (bool, error) { return fn(id, reversed(row)) })
}

func init() { store.Register(reversedKind{}) }

// durableOnly skips a test that reopens a database and expects its rows,
// when the suite runs with a volatile default kind (BANGDB_TEST_STORE).
func durableOnly(t *testing.T) {
	t.Helper()
	if k, err := store.Lookup(os.Getenv("BANGDB_TEST_STORE")); err == nil && k.Volatile() {
		t.Skip("needs durable tables")
	}
}

func TestMain(m *testing.M) {
	if k := os.Getenv("BANGDB_TEST_STORE"); k != "" {
		engine.SetDefaultStore(k)
	}
	os.Exit(m.Run())
}

func TestReversedKindConformance(t *testing.T) {
	storetest.TestTableStore(t, reversedKind{})
}

func TestCreateTableUsing(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE r (id INTEGER PRIMARY KEY, name TEXT UNIQUE, n INT) USING reversed;
		CREATE TABLE b (id INTEGER PRIMARY KEY, r_id INT REFERENCES r ON DELETE CASCADE, v TEXT) USING btree;
		CREATE INDEX r_n ON r (n);
		INSERT INTO r (name, n) VALUES ('a', 3), ('b', 1), ('c', 2);
		INSERT INTO b (r_id, v) VALUES (1, 'x'), (2, 'y'), (3, 'z')`)
	expect(t, db, `SELECT name FROM r ORDER BY n`, "b;c;a")
	expect(t, db, `SELECT r.name, b.v FROM r JOIN b ON b.r_id = r.id WHERE r.n >= 2 ORDER BY r.id DESC`, "c|z;a|x")
	exec(t, db, `UPDATE r SET n = n * 10 WHERE name <> 'a'; DELETE FROM r WHERE name = 'b'`)
	expect(t, db, `SELECT name, n FROM r ORDER BY id`, "a|3;c|20")
	expect(t, db, `SELECT count(*) FROM b`, "2")
	if _, err := db.Exec(`INSERT INTO r (name) VALUES ('a')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("unique on reversed kind: %v", err)
	}
	exec(t, db, `ALTER TABLE r ADD COLUMN extra TEXT DEFAULT 'd'; DROP INDEX r_n; ALTER TABLE r DROP COLUMN n`)
	expect(t, db, `SELECT * FROM r ORDER BY id`, "1|a|d;3|c|d")
	if d := db.Engine().Catalog().Describe("r"); !strings.Contains(d, ") USING reversed;") {
		t.Fatalf("describe: %s", d)
	}
	if d := db.Engine().Catalog().Describe("b"); strings.Contains(d, "USING") {
		t.Fatalf("default kind should not be shown: %s", d)
	}
	mustFail(t, db, `CREATE TABLE z (a INT) USING nosuchkind`, "unknown storage kind")
}

// TestDefaultStoreApplies guards the BANGDB_TEST_STORE run itself: tables
// created without USING must use the configured kind.
func TestDefaultStoreApplies(t *testing.T) {
	k := os.Getenv("BANGDB_TEST_STORE")
	if k == "" || k == "btree" {
		t.Skip("BANGDB_TEST_STORE not set")
	}
	db := open(t)
	exec(t, db, `CREATE TABLE d (a INT)`)
	if d := db.Engine().Catalog().Describe("d"); !strings.Contains(d, "USING "+k) {
		t.Fatalf("default kind not applied: %s", d)
	}
}
