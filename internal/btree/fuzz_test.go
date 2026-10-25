package btree

import (
	"testing"

	"github.com/knoxxr/bangdb/internal/pager"
)

// FuzzDecode feeds arbitrary page images to the node decoder: it must report
// errors, never panic.
func FuzzDecode(f *testing.F) {
	leaf := &node{leaf: true, keys: [][]byte{[]byte("k1"), []byte("k2")},
		vals: []value{{inline: []byte("v")}, {overflow: true, total: 5000, first: 7}}}
	f.Add(leaf.encode())
	f.Add((&node{keys: [][]byte{[]byte("m")}, children: []uint32{3, 4}}).encode())
	f.Fuzz(func(t *testing.T, b []byte) {
		page := make([]byte, pager.PageSize)
		copy(page, b)
		if n, err := decode(page); err == nil {
			n.size()
			n.encode()
		}
	})
}
