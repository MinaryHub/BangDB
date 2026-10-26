package store

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/knoxxr/bangdb/internal/value"
)

func roundTrip(t *testing.T, rowids []int64, rows [][]any) []byte {
	t.Helper()
	b := encodeBlock(rowids, rows)
	gotIDs, gotRows, err := decodeBlock(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotIDs, rowids) {
		t.Fatalf("rowids differ")
	}
	for i := range rows {
		if len(rows[i]) != len(gotRows[i]) {
			t.Fatalf("row %d: length %d, want %d", i, len(gotRows[i]), len(rows[i]))
		}
		for j := range rows[i] {
			a, b := rows[i][j], gotRows[i][j]
			if fa, ok := a.(float64); ok && math.IsNaN(fa) {
				if fb, ok := b.(float64); !ok || !math.IsNaN(fb) {
					t.Fatalf("row %d col %d: %v, want NaN", i, j, b)
				}
				continue
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("row %d col %d: %#v, want %#v", i, j, b, a)
			}
		}
	}
	return b
}

func TestBlockRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	values := []func() any{
		func() any { return nil },
		func() any { return rng.Int63() - rng.Int63() },
		func() any { return int64(rng.Intn(10)) },
		func() any { return rng.NormFloat64() * 1e6 },
		func() any { return math.Inf(-1) },
		func() any { return math.NaN() },
		func() any { return -0.0 },
		func() any { return fmt.Sprintf("s%d", rng.Intn(20)) },
		func() any { return "" },
		func() any { return []byte{1, 0, 2} },
	}
	for trial := 0; trial < 300; trial++ {
		n := 1 + rng.Intn(300)
		ncols := rng.Intn(6)
		kinds := make([][]int, ncols) // per column, the allowed value kinds
		for c := range kinds {
			for k := 0; k < 1+rng.Intn(3); k++ {
				kinds[c] = append(kinds[c], rng.Intn(len(values)))
			}
		}
		rowids := make([]int64, n)
		rows := make([][]any, n)
		id := rng.Int63n(1<<40) - 1<<39
		for i := range rows {
			id += 1 + rng.Int63n(1000)
			rowids[i] = id
			l := ncols
			if trial%7 == 0 {
				l = rng.Intn(ncols + 1) // rows of different lengths (ALTER TABLE ADD COLUMN)
			}
			rows[i] = make([]any, l)
			for c := range rows[i] {
				ks := kinds[c]
				rows[i][c] = values[ks[rng.Intn(len(ks))]]()
			}
		}
		roundTrip(t, rowids, rows)
	}
	roundTrip(t, []int64{math.MinInt64, 0, math.MaxInt64}, [][]any{{int64(math.MinInt64)}, {int64(0)}, {int64(math.MaxInt64)}})
}

// TestBlockCompression checks the size of typical metrics: a timestamp
// every 10 s, a host tag, a counter and a gauge.
func TestBlockCompression(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	const n = 1024
	rowids := make([]int64, n)
	rows := make([][]any, n)
	ts, counter, gauge := int64(1_760_000_000), int64(0), 50.0
	for i := range rows {
		ts += 10
		counter += int64(rng.Intn(100))
		gauge += math.Round(rng.NormFloat64()*10) / 10
		rowids[i] = ts << 20
		rows[i] = []any{ts, fmt.Sprintf("web-%d", i%8), counter, gauge}
	}
	b := roundTrip(t, rowids, rows)
	var plain int
	for _, r := range rows {
		plain += len(value.EncodeRow(r)) + 8
	}
	t.Logf("block %d bytes for %d rows (%.1f bytes/row); row records %d bytes (%.1fx)", len(b), n, float64(len(b))/n, plain, float64(plain)/float64(len(b)))
	if len(b)*3 > plain {
		t.Fatalf("poor compression: %d vs %d bytes", len(b), plain)
	}
}

func FuzzDecodeBlock(f *testing.F) {
	f.Add(encodeBlock([]int64{1, 2, 3}, [][]any{{int64(1), 1.5, "a"}, {nil, 2.5, "b"}, {int64(3), nil, "a"}}))
	f.Fuzz(func(t *testing.T, data []byte) {
		decodeBlock(data) // must not panic
	})
}
