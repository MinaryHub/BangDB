package store_test

import (
	"testing"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/store/storetest"
)

func TestRowKind(t *testing.T) {
	k, err := store.Lookup("")
	if err != nil {
		t.Fatal(err)
	}
	storetest.TestTableStore(t, k)
}

func TestLookupUnknown(t *testing.T) {
	if _, err := store.Lookup("nope"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestIndexStore(t *testing.T) {
	v := pager.OpenMemory().NewView()
	root, err := store.CreateIndex(v)
	if err != nil {
		t.Fatal(err)
	}
	ix := store.OpenIndex(v, root)
	for _, k := range []string{"b1", "a2", "a1", "c1"} {
		if err := ix.Put([]byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	got := func(it store.Iter) (out []string) {
		for ; it.Valid(); it.Next() {
			out = append(out, string(it.Key()))
		}
		if it.Err() != nil {
			t.Fatal(it.Err())
		}
		return
	}
	if g := got(ix.Seek([]byte("a2"))); len(g) != 3 || g[0] != "a2" || g[2] != "c1" {
		t.Fatalf("Seek: %v", g)
	}
	if g := got(ix.SeekReverse([]byte("b"))); len(g) != 2 || g[0] != "a2" || g[1] != "a1" {
		t.Fatalf("SeekReverse: %v", g)
	}
	if ok, _ := ix.Delete([]byte("a2")); !ok {
		t.Fatal("Delete")
	}
	if g := got(ix.SeekReverse(nil)); len(g) != 3 || g[0] != "c1" {
		t.Fatalf("SeekReverse(nil): %v", g)
	}
}
