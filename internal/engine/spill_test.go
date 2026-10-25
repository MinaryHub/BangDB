package engine

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/knoxxr/bangdb/internal/value"
)

func TestExtSorter(t *testing.T) {
	for _, limit := range []int{0, 1 << 10, 1 << 30} {
		s := newSorter(&spillConfig{limit: limit, dir: t.TempDir()}, func(a, b []any) bool {
			return value.Compare(a[0], b[0]) < 0
		})
		rng := rand.New(rand.NewSource(1))
		var want [][]any
		for i := 0; i < 5000; i++ {
			rec := []any{int64(rng.Intn(100)), int64(i), "pad", nil, []byte{1, 2}, 1.5}
			want = append(want, rec)
			if err := s.Add(rec); err != nil {
				t.Fatal(err)
			}
		}
		if spilled := s.Spilled(); spilled != (limit == 1<<10) {
			t.Fatalf("limit %d: spilled = %v", limit, spilled)
		}
		sort.SliceStable(want, func(i, j int) bool { return want[i][0].(int64) < want[j][0].(int64) })
		it, err := s.Finish()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			rec, err := it.Next()
			if err != nil {
				t.Fatal(err)
			}
			if rec == nil {
				if i != len(want) {
					t.Fatalf("got %d records", i)
				}
				break
			}
			// Stable: equal keys keep insertion order (column 1).
			if rec[0] != want[i][0] || rec[1] != want[i][1] || rec[2] != "pad" || rec[3] != nil || rec[5] != 1.5 {
				t.Fatalf("record %d = %v, want %v", i, rec, want[i])
			}
		}
		s.Close()
	}
}
