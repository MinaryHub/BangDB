package pager

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"testing"
)

// IEEE 1619 XTS-AES-128 test vectors 1 and 2.
func TestXTSVectors(t *testing.T) {
	for _, v := range []struct{ k1, k2, pt, ct string }{
		{"00000000000000000000000000000000", "00000000000000000000000000000000",
			"0000000000000000000000000000000000000000000000000000000000000000",
			"917cf69ebd68b2ec9b9fe9a3eadda692cd43d2f59598ed858c02c2652fbf922e"},
		{"11111111111111111111111111111111", "22222222222222222222222222222222",
			"4444444444444444444444444444444444444444444444444444444444444444",
			"c454185e6a16936e39334038acef838bfb186fff7480adc4289382ecd6d394f0"},
	} {
		k1, _ := hex.DecodeString(v.k1)
		k2, _ := hex.DecodeString(v.k2)
		pt, _ := hex.DecodeString(v.pt)
		b1, _ := aes.NewCipher(k1)
		b2, _ := aes.NewCipher(k2)
		sector := uint64(0)
		if v.k1[0] == '1' {
			sector = 0x3333333333
		}
		ct := make([]byte, len(pt))
		xts(b1, b2, ct, pt, sector, true)
		if hex.EncodeToString(ct) != v.ct {
			t.Errorf("encrypt: %x, want %s", ct, v.ct)
		}
		xts(b1, b2, ct, ct, sector, false)
		if !bytes.Equal(ct, pt) {
			t.Errorf("decrypt: %x", ct)
		}
	}
}
