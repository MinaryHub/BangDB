package store

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/minaryhub/bangdb/internal/pager"
	"github.com/minaryhub/bangdb/internal/value"
)

func openTS(t *testing.T, v *pager.View, opts map[string]string) (*tsStore, uint32) {
	t.Helper()
	k := TimeSeriesKind{}
	o := map[string]string{"time_column": "ts", "block_rows": "16"}
	for key, val := range opts {
		o[key] = val
	}
	cfg, err := k.Configure(TableSpec{Columns: []ColumnSpec{{Name: "ts", Type: "INTEGER"}, {Name: "v", Type: "REAL"}, {Name: "tag", Type: "TEXT"}}, RowidColumn: -1, Options: o})
	if err != nil {
		t.Fatal(err)
	}
	root, err := k.Create(v)
	if err != nil {
		t.Fatal(err)
	}
	return k.OpenConfigured(v, root, cfg).(*tsStore), root
}

func scanAll(t *testing.T, s TableStore, lo, hi int64, desc bool) []int64 {
	t.Helper()
	var ids []int64
	err := s.Scan(lo, hi, desc, func(id int64, row []any) (bool, error) {
		ids = append(ids, id)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestTimeSeriesModel(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	v := pager.OpenMemory().NewView()
	s, _ := openTS(t, v, nil)
	ref := map[int64][]any{}
	keys := func() []int64 {
		var out []int64
		for k := range ref {
			out = append(out, k)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	tick := int64(1_000_000)
	for i := 0; i < 20000; i++ {
		switch r := rng.Intn(100); {
		case r < 70: // append, sometimes at the same time or a bit in the past
			ts := tick
			switch rng.Intn(10) {
			case 0:
				ts = tick - int64(rng.Intn(500))
			case 1, 2:
			default:
				tick += int64(1 + rng.Intn(3))
				ts = tick
			}
			row := []any{ts, rng.Float64(), "t" + string(rune('a'+rng.Intn(5)))}
			id, err := s.AssignRowid(row)
			if err != nil {
				t.Fatal(err)
			}
			if _, dup := ref[id]; dup || id>>s.cfg.shift != ts {
				t.Fatalf("AssignRowid gave %d (dup %v) for time %d", id, dup, ts)
			}
			if err := s.Put(id, row); err != nil {
				t.Fatal(err)
			}
			ref[id] = row
		case r < 80: // update an existing row
			ks := keys()
			if len(ks) == 0 {
				continue
			}
			id := ks[rng.Intn(len(ks))]
			row := []any{id >> s.cfg.shift, -1.0, nil}
			if err := s.Put(id, row); err != nil {
				t.Fatal(err)
			}
			ref[id] = row
		case r < 92: // delete
			ks := keys()
			if len(ks) == 0 {
				continue
			}
			id := ks[rng.Intn(len(ks))]
			if rng.Intn(4) == 0 {
				id += 1 // sometimes a missing one
			}
			ok, err := s.Delete(id)
			if _, want := ref[id]; err != nil || ok != want {
				t.Fatalf("Delete(%d) = %v %v, want %v", id, ok, err, want)
			}
			delete(ref, id)
		case r < 97: // get
			ks := keys()
			if len(ks) == 0 {
				continue
			}
			id := ks[rng.Intn(len(ks))]
			row, ok, err := s.Get(id)
			if err != nil || !ok || !reflect.DeepEqual(row, ref[id]) {
				t.Fatalf("Get(%d) = %v %v %v, want %v", id, row, ok, err, ref[id])
			}
		default: // range delete (rarely: it removes many rows)
			ks := keys()
			if len(ks) < 2 || rng.Intn(10) != 0 {
				continue
			}
			i := rng.Intn(len(ks))
			a, b := ks[i], ks[min(len(ks)-1, i+rng.Intn(60))]
			var want int64
			for _, k := range ks {
				if k >= a && k <= b {
					want++
					delete(ref, k)
				}
			}
			n, err := s.DeleteRange(a, b)
			if err != nil || n != want {
				t.Fatalf("DeleteRange = %d %v, want %d", n, err, want)
			}
		}
		if i%997 == 0 {
			ks := keys()
			if got := scanAll(t, s, MinRowid, MaxRowid, false); !reflect.DeepEqual(got, ks) && !(len(got) == 0 && len(ks) == 0) {
				t.Fatalf("op %d: scan has %d rows, model %d", i, len(got), len(ks))
			}
			if len(ks) > 0 {
				a, b := ks[rng.Intn(len(ks))], ks[rng.Intn(len(ks))]
				if a > b {
					a, b = b, a
				}
				var want []int64
				for j := len(ks) - 1; j >= 0; j-- {
					if ks[j] >= a && ks[j] <= b {
						want = append(want, ks[j])
					}
				}
				if got := scanAll(t, s, a, b, true); !reflect.DeepEqual(got, want) {
					t.Fatalf("op %d: reverse range scan differs (%d vs %d rows)", i, len(got), len(want))
				}
			}
			if n, _ := s.EstimateRows(); n != int64(len(ks)) {
				t.Fatalf("op %d: row count %d, model %d", i, n, len(ks))
			}
		}
	}
	m, _ := s.meta()
	blocks := 0
	for c := s.t.Seek([]byte{tsBlockKey}); c.Valid() && c.Key()[0] == tsBlockKey; c.Next() {
		blocks++
	}
	t.Logf("%d rows in %d blocks, %d tail singles", m.live, blocks, m.tail)
	// Compaction keeps the changes inside each block below the threshold.
	for c := s.t.Seek([]byte{tsBlockKey}); c.Valid() && c.Key()[0] == tsBlockKey; c.Next() {
		v, _ := c.Value()
		b, err := parseBlock(c.Key(), v)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for sc := s.t.Seek(key(tsSingleKey, b.first)); sc.Valid() && sc.Key()[0] == tsSingleKey && value.DecodeRowID(sc.Key()[1:]) <= b.last; sc.Next() {
			n++
		}
		if n >= s.cfg.blockRows/4 || b.count > s.cfg.blockRows {
			t.Fatalf("block %d..%d: %d rows, %d pending changes", b.first, b.last, b.count, n)
		}
	}
	if blocks < 10 {
		t.Fatal("too few blocks: sealing did not happen")
	}
	// Scans see every row with its latest value.
	err := s.Scan(MinRowid, MaxRowid, false, func(id int64, row []any) (bool, error) {
		if !reflect.DeepEqual(row, ref[id]) {
			t.Fatalf("row %d = %v, want %v", id, row, ref[id])
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTimeSeriesRetention(t *testing.T) {
	v := pager.OpenMemory().NewView()
	s, _ := openTS(t, v, map[string]string{"retention": "1 hour"})
	for ts := int64(0); ts < 3*3600; ts += 10 {
		row := []any{ts, 1.0, "x"}
		id, err := s.AssignRowid(row)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(id, row); err != nil {
			t.Fatal(err)
		}
	}
	ids := scanAll(t, s, MinRowid, MaxRowid, false)
	oldest := ids[0] >> s.cfg.shift
	newest := ids[len(ids)-1] >> s.cfg.shift
	// Retention works per block (16 rows = 160 s here).
	if newest-oldest > 3600+160 || newest-oldest < 3600-160 {
		t.Fatalf("kept %d..%d (span %d s), want about one hour", oldest, newest, newest-oldest)
	}
	if n, _ := s.EstimateRows(); n != int64(len(ids)) {
		t.Fatalf("count %d, scan %d", n, len(ids))
	}
}

func TestTimeRanges(t *testing.T) {
	v := pager.OpenMemory().NewView()
	s, _ := openTS(t, v, nil)
	from, to, ok := s.RowidRange(int64(10), true, int64(20), false)
	if !ok || from != 10<<20 || to != 19<<20|(1<<20-1) {
		t.Fatalf("[10, 20): %d..%d", from, to)
	}
	from, to, ok = s.RowidRange(9.5, false, nil, false)
	if !ok || from != 10<<20 || to != MaxRowid {
		t.Fatalf("> 9.5: %d..%d", from, to)
	}
	if _, _, ok = s.RowidRange("x", true, nil, false); ok {
		t.Fatal("text bound converted")
	}
	if from, to, _ = s.RowidRange(int64(5), false, int64(5), false); from <= to {
		t.Fatal("empty range expected")
	}
	for _, c := range []struct {
		s    string
		unit string
		want int64
	}{{"30 days", "s", 30 * 86400}, {"5 minutes", "ms", 300000}, {"250ms", "us", 250000}, {"2h", "s", 7200}, {"90", "s", 90}, {"1 week", "s", 604800}} {
		if got, err := ParseInterval(c.s, c.unit); err != nil || got != c.want {
			t.Fatalf("ParseInterval(%q, %s) = %d %v, want %d", c.s, c.unit, got, err, c.want)
		}
	}
	if _, err := ParseInterval("3 fortnights", "s"); err == nil {
		t.Fatal("bad interval accepted")
	}
}
