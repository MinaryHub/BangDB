package store_test

import (
	"testing"

	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/store/storetest"
)

func TestKinds(t *testing.T) {
	for _, name := range []string{"", "memory"} {
		k, err := store.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Run("table/"+name, func(t *testing.T) { storetest.TestTableStore(t, k) })
		t.Run("index/"+name, func(t *testing.T) { storetest.TestIndexStore(t, k) })
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, err := store.Lookup("nope"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
