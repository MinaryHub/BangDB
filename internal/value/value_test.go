package value

import "testing"

func TestDecodeKey(t *testing.T) {
	vals := []any{nil, int64(-5), int64(1 << 62), 3.25, -1e300, "", "a\x00b", []byte{}, []byte{0, 1, 0xff}}
	var k []byte
	for _, v := range vals {
		k = AppendKey(k, v)
	}
	k = append(k, 1, 2, 3) // a rowid suffix is ignored
	got, ok := DecodeKey(k, len(vals))
	if !ok {
		t.Fatal("decode failed")
	}
	for i := range vals {
		if Compare(got[i], vals[i]) != 0 || TypeOf(got[i]) != TypeOf(vals[i]) {
			t.Errorf("%d: got %#v, want %#v", i, got[i], vals[i])
		}
	}
	if _, ok := DecodeKey(k[:5], 2); ok {
		t.Error("truncated key decoded")
	}
}
