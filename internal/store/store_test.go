package store_test

import (
	"testing"

	"github.com/minaryhub/bangdb/internal/store"
	"github.com/minaryhub/bangdb/internal/store/storetest"
)

func TestKinds(t *testing.T) {
	for _, name := range []string{"", "memory", "timeseries"} {
		k, err := store.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Run("table/"+name, func(t *testing.T) { storetest.TestTableStore(t, k) })
		if name != "timeseries" { // no indexes
			t.Run("index/"+name, func(t *testing.T) { storetest.TestIndexStore(t, k) })
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, err := store.Lookup("nope"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
