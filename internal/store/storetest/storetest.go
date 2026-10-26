// Package storetest checks that a store implementation behaves like the
// engine expects. Every store kind should run TestTableStore in its tests.
package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/store"
)

// TestTableStore runs the conformance checks against kind.
func TestTableStore(t *testing.T, kind store.Kind) {
	t.Run("RandomOps", func(t *testing.T) { randomOps(t, kind) })
	t.Run("Scan", func(t *testing.T) { scans(t, kind) })
	t.Run("Transactions", func(t *testing.T) { transactions(t, kind) })
	t.Run("Drop", func(t *testing.T) { drop(t, kind) })
}

// Spec is the table definition used for configurable kinds: an INTEGER
// time column first, small blocks so that sealing happens often.
var Spec = store.TableSpec{
	Columns:     []store.ColumnSpec{{Name: "ts", Type: "INTEGER"}, {Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}},
	RowidColumn: -1,
	Options:     map[string]string{"time_column": "ts", "block_rows": "16"},
}

// Open opens a store of kind, configuring it with Spec if needed.
func Open(t *testing.T, kind store.Kind, v *pager.View, root uint32) store.TableStore {
	t.Helper()
	if ck, ok := kind.(store.ConfigurableKind); ok {
		cfg, err := ck.Configure(Spec)
		if err != nil {
			t.Fatal(err)
		}
		return ck.OpenConfigured(v, root, cfg)
	}
	return kind.Open(v, root)
}

func newStore(t *testing.T, kind store.Kind) (*pager.Pager, *pager.View, store.TableStore) {
	p := pager.OpenMemory()
	v := p.NewView()
	root, err := kind.Create(v)
	if err != nil {
		t.Fatal(err)
	}
	return p, v, Open(t, kind, v, root)
}

func randomRow(rng *rand.Rand, id int64) []any {
	row := []any{id, nil, rng.Float64(), fmt.Sprintf("s%d", rng.Intn(1000))}
	if rng.Intn(10) == 0 {
		row = append(row, bytes.Repeat([]byte{byte(id)}, 2000+rng.Intn(5000))) // overflow
	}
	return row
}

// collect returns all rows of s in the given order.
func collect(t *testing.T, s store.TableStore, lo, hi int64, desc bool) ([]int64, [][]any) {
	t.Helper()
	var ids []int64
	var rows [][]any
	err := s.Scan(lo, hi, desc, func(id int64, row []any) (bool, error) {
		ids = append(ids, id)
		rows = append(rows, row)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids, rows
}

func randomOps(t *testing.T, kind store.Kind) {
	_, _, s := newStore(t, kind)
	rng := rand.New(rand.NewSource(1))
	ref := map[int64][]any{}
	for i := 0; i < 20000; i++ {
		id := int64(rng.Intn(4000)) - 2000
		switch rng.Intn(5) {
		case 0:
			ok, err := s.Delete(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, want := ref[id]; ok != want {
				t.Fatalf("Delete(%d) = %v, want %v", id, ok, want)
			}
			delete(ref, id)
		case 1:
			row, ok, err := s.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if want, has := ref[id]; ok != has || has && !reflect.DeepEqual(row, want) {
				t.Fatalf("Get(%d) = %v %v, want %v", id, row, ok, want)
			}
		default:
			row := randomRow(rng, id)
			if err := s.Put(id, row); err != nil {
				t.Fatal(err)
			}
			ref[id] = row
		}
	}
	var want []int64
	for id := range ref {
		want = append(want, id)
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	ids, rows := collect(t, s, store.MinRowid, store.MaxRowid, false)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("scan returned %d ids, want %d", len(ids), len(want))
	}
	for i, id := range ids {
		if !reflect.DeepEqual(rows[i], ref[id]) {
			t.Fatalf("row %d differs", id)
		}
	}
	last, ok, err := s.Last()
	if err != nil || !ok || last != want[len(want)-1] {
		t.Fatalf("Last = %d %v %v", last, ok, err)
	}
	est, err := s.EstimateRows()
	if err != nil || est < int64(len(want))/3 || est > int64(len(want))*3 {
		t.Fatalf("EstimateRows = %d (%v), actual %d", est, err, len(want))
	}
}

func scans(t *testing.T, kind store.Kind) {
	_, _, s := newStore(t, kind)
	if _, ok, err := s.Last(); ok || err != nil {
		t.Fatalf("Last on empty store: %v %v", ok, err)
	}
	if n, err := s.EstimateRows(); n != 0 || err != nil {
		t.Fatalf("EstimateRows on empty store: %d %v", n, err)
	}
	for _, id := range []int64{store.MinRowid, -5, -1, 0, 1, 2, 3, 10, 100, store.MaxRowid} {
		if err := s.Put(id, []any{id}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		lo, hi int64
		desc   bool
		want   []int64
	}{
		{-1, 3, false, []int64{-1, 0, 1, 2, 3}},
		{-1, 3, true, []int64{3, 2, 1, 0, -1}},
		{4, 9, false, nil},
		{5, 4, false, nil},
		{100, store.MaxRowid, false, []int64{100, store.MaxRowid}},
		{store.MinRowid, -5, true, []int64{-5, store.MinRowid}},
	}
	for _, c := range cases {
		ids, rows := collect(t, s, c.lo, c.hi, c.desc)
		if !reflect.DeepEqual(ids, c.want) {
			t.Fatalf("Scan(%d, %d, %v) = %v, want %v", c.lo, c.hi, c.desc, ids, c.want)
		}
		for i, r := range rows {
			if r[0] != ids[i] {
				t.Fatalf("row of %d is %v", ids[i], r)
			}
		}
	}
	// Early stop and errors from fn.
	n := 0
	err := s.Scan(store.MinRowid, store.MaxRowid, false, func(int64, []any) (bool, error) { n++; return n < 3, nil })
	if err != nil || n != 3 {
		t.Fatalf("early stop: %d %v", n, err)
	}
	boom := errors.New("boom")
	if err := s.Scan(store.MinRowid, store.MaxRowid, true, func(int64, []any) (bool, error) { return true, boom }); err != boom {
		t.Fatalf("fn error not returned: %v", err)
	}
}

// transactions checks that stores live in pager pages: a snapshot keeps the
// committed state while a later view changes it.
func transactions(t *testing.T, kind store.Kind) {
	p := pager.OpenMemory()
	v := p.NewView()
	root, err := kind.Create(v)
	if err != nil {
		t.Fatal(err)
	}
	s := Open(t, kind, v, root)
	for i := int64(1); i <= 500; i++ {
		s.Put(i, []any{i, "committed"})
	}
	if err := p.Commit(v); err != nil {
		t.Fatal(err)
	}
	snap, err := p.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()

	v2 := p.NewView()
	s2 := Open(t, kind, v2, root)
	for i := int64(1); i <= 500; i += 2 {
		s2.Delete(i)
	}
	s2.Put(1000, []any{int64(1000), "new"})
	if err := p.Commit(v2); err != nil {
		t.Fatal(err)
	}
	old, _ := collect(t, Open(t, kind, snap.View(), root), store.MinRowid, store.MaxRowid, false)
	if len(old) != 500 {
		t.Fatalf("snapshot sees %d rows, want 500", len(old))
	}
	cur, _ := collect(t, Open(t, kind, p.NewView(), root), store.MinRowid, store.MaxRowid, false)
	if len(cur) != 251 || cur[len(cur)-1] != 1000 {
		t.Fatalf("latest state has %d rows", len(cur))
	}
}

func drop(t *testing.T, kind store.Kind) {
	p := pager.OpenMemory()
	v := p.NewView()
	before := v.PageCount()
	root, err := kind.Create(v)
	if err != nil {
		t.Fatal(err)
	}
	s := Open(t, kind, v, root)
	rng := rand.New(rand.NewSource(2))
	for i := int64(0); i < 3000; i++ {
		s.Put(i, randomRow(rng, i))
	}
	if err := kind.Drop(v, root); err != nil {
		t.Fatal(err)
	}
	// Every page went to the free list: allocating them all again must not
	// grow the file.
	grown := v.PageCount()
	for i := before; i < grown; i++ {
		if _, err := v.Alloc(); err != nil {
			t.Fatal(err)
		}
	}
	if v.PageCount() != grown {
		t.Fatalf("pages leaked by Drop: %d allocations grew the file from %d to %d", grown-before, grown, v.PageCount())
	}
}

// TestIndexStore runs the index conformance checks against kind.
func TestIndexStore(t *testing.T, kind store.Kind) {
	p := pager.OpenMemory()
	v := p.NewView()
	root, err := kind.CreateIndex(v)
	if err != nil {
		t.Fatal(err)
	}
	ix := kind.OpenIndex(v, root)
	rng := rand.New(rand.NewSource(3))
	ref := map[string]bool{}
	for i := 0; i < 20000; i++ {
		k := []byte(fmt.Sprintf("k%04d", rng.Intn(3000)))
		if rng.Intn(3) == 0 {
			ok, err := ix.Delete(k)
			if err != nil || ok != ref[string(k)] {
				t.Fatalf("Delete(%s) = %v %v", k, ok, err)
			}
			delete(ref, string(k))
		} else {
			if err := ix.Put(k); err != nil {
				t.Fatal(err)
			}
			ref[string(k)] = true
		}
	}
	var want []string
	for k := range ref {
		want = append(want, k)
	}
	sort.Strings(want)
	read := func(it store.Iter) (out []string) {
		for ; it.Valid(); it.Next() {
			out = append(out, string(it.Key()))
		}
		if it.Err() != nil {
			t.Fatal(it.Err())
		}
		return
	}
	if got := read(ix.Seek(nil)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Seek(nil): %d keys, want %d", len(got), len(want))
	}
	rev := read(ix.SeekReverse(nil))
	if len(rev) != len(want) || len(want) > 0 && rev[0] != want[len(want)-1] {
		t.Fatal("SeekReverse(nil)")
	}
	for _, probe := range []string{"k0000", "k1500", "k15005", "k2999", "k9", "a"} {
		i := sort.SearchStrings(want, probe)
		if got := read(ix.Seek([]byte(probe))); !reflect.DeepEqual(got, want[i:]) && !(len(got) == 0 && i == len(want)) {
			t.Fatalf("Seek(%s)", probe)
		}
		j := sort.Search(len(want), func(i int) bool { return want[i] > probe })
		got := read(ix.SeekReverse([]byte(probe)))
		if len(got) != j || j > 0 && got[0] != want[j-1] {
			t.Fatalf("SeekReverse(%s): %d keys, want %d", probe, len(got), j)
		}
	}
	if n, err := ix.EstimateRows(); err != nil || n < int64(len(want))/3 || n > int64(len(want))*3 {
		t.Fatalf("EstimateRows = %d, actual %d", n, len(want))
	}
	// The iterator's keys are the index's: a caller's buffer is not kept.
	buf := []byte("zzz1")
	ix.Put(buf)
	buf[3] = '2'
	if got := read(ix.Seek([]byte("zzz"))); len(got) != 1 || got[0] != "zzz1" {
		t.Fatalf("index kept the caller's key buffer: %v", got)
	}
	if err := kind.DropIndex(v, root); err != nil {
		t.Fatal(err)
	}
}
