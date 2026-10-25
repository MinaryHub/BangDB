package value

import (
	"bytes"
	"math"
	"testing"
)

func FuzzDecodeRow(f *testing.F) {
	f.Add(EncodeRow([]any{nil, int64(-5), 2.5, "text", []byte{0, 1}}))
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		row, err := DecodeRow(b)
		if err != nil {
			return
		}
		// Whatever decodes must re-encode to something that decodes equally.
		again, err := DecodeRow(EncodeRow(row))
		if err != nil || len(again) != len(row) {
			t.Fatalf("round trip failed: %v", err)
		}
	})
}

// FuzzKeyOrder checks that key encoding preserves Compare's order.
func FuzzKeyOrder(f *testing.F) {
	f.Add(int64(1), int64(-1), 1.5, -0.0, "a", "a\x00b")
	f.Fuzz(func(t *testing.T, i1, i2 int64, f1, f2 float64, s1, s2 string) {
		pairs := [][2]any{{i1, i2}, {s1, s2}, {[]byte(s1), []byte(s2)}}
		if !math.IsNaN(f1) && !math.IsNaN(f2) {
			pairs = append(pairs, [2]any{f1, f2})
		}
		for _, p := range pairs {
			want := Compare(p[0], p[1])
			got := bytes.Compare(AppendKey(nil, p[0]), AppendKey(nil, p[1]))
			if want != got {
				t.Fatalf("order mismatch for %#v vs %#v: Compare=%d keys=%d", p[0], p[1], want, got)
			}
			// Prefix-freeness: a key followed by more data still sorts the same.
			a := append(AppendKey(nil, p[0]), 0xff)
			b := append(AppendKey(nil, p[1]), 0x00)
			if want != 0 && bytes.Compare(a, b) != want {
				t.Fatalf("encoding is not prefix-free for %#v vs %#v", p[0], p[1])
			}
		}
	})
}
