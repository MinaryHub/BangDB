// Package btree implements a B+tree of byte-string keys and values stored in
// pager pages. Leaves are linked left-to-right for range scans. Values larger
// than InlineMax are spilled to overflow page chains.
//
// Deletion removes entries without rebalancing (pages may become sparse or
// empty); space is reclaimed when the whole tree is dropped. This keeps the
// code small and is a common trade-off in embedded engines.
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/knoxxr/bangdb/internal/pager"
)

const (
	// MaxKey is the largest key accepted by Put.
	MaxKey = 512
	// InlineMax is the largest value stored inside a leaf page.
	InlineMax = 700

	typeLeaf     = 1
	typeInternal = 2
	typeOverflow = 3

	nodeHeader = 7 // type(1) n(2) next-or-child0(4)
	ovHeader   = 7 // type(1) next(4) used(2)
	ovChunk    = pager.UsableSize - ovHeader
)

// ErrKeyTooLarge is returned when a key exceeds MaxKey bytes.
var ErrKeyTooLarge = fmt.Errorf("bangdb: key exceeds %d bytes", MaxKey)

var errBadPage = errors.New("bangdb: corrupt b-tree page")

type value struct {
	inline   []byte
	overflow bool
	total    uint32
	first    uint32
}

type node struct {
	leaf     bool
	keys     [][]byte
	vals     []value  // leaf only
	children []uint32 // internal only, len(keys)+1
	next     uint32   // leaf only
}

func (n *node) size() int {
	s := nodeHeader
	for i, k := range n.keys {
		s += 2 + len(k)
		if n.leaf {
			if n.vals[i].overflow {
				s += 1 + 8
			} else {
				s += 1 + 2 + len(n.vals[i].inline)
			}
		} else {
			s += 4
		}
	}
	return s
}

func (n *node) encode() []byte {
	b := make([]byte, pager.PageSize)
	binary.BigEndian.PutUint16(b[1:], uint16(len(n.keys)))
	off := nodeHeader
	if n.leaf {
		b[0] = typeLeaf
		binary.BigEndian.PutUint32(b[3:], n.next)
	} else {
		b[0] = typeInternal
		binary.BigEndian.PutUint32(b[3:], n.children[0])
	}
	for i, k := range n.keys {
		binary.BigEndian.PutUint16(b[off:], uint16(len(k)))
		off += 2
		off += copy(b[off:], k)
		if !n.leaf {
			binary.BigEndian.PutUint32(b[off:], n.children[i+1])
			off += 4
			continue
		}
		v := n.vals[i]
		if v.overflow {
			b[off] = 1
			binary.BigEndian.PutUint32(b[off+1:], v.total)
			binary.BigEndian.PutUint32(b[off+5:], v.first)
			off += 9
		} else {
			b[off] = 0
			binary.BigEndian.PutUint16(b[off+1:], uint16(len(v.inline)))
			off += 3
			off += copy(b[off:], v.inline)
		}
	}
	return b
}

func decode(b []byte) (*node, error) {
	if len(b) < pager.UsableSize {
		return nil, errBadPage
	}
	b = b[:pager.UsableSize] // the rest is the page checksum
	n := &node{}
	switch b[0] {
	case typeLeaf:
		n.leaf = true
		n.next = binary.BigEndian.Uint32(b[3:])
	case typeInternal:
	default:
		return nil, errBadPage
	}
	cnt := int(binary.BigEndian.Uint16(b[1:]))
	if cnt > pager.UsableSize/3 { // every cell needs at least 3 bytes
		return nil, errBadPage
	}
	n.keys = make([][]byte, 0, cnt+1)
	if n.leaf {
		n.vals = make([]value, 0, cnt+1)
	} else {
		n.children = make([]uint32, 1, cnt+2)
		n.children[0] = binary.BigEndian.Uint32(b[3:])
	}
	off := nodeHeader
	need := func(k int) bool { return off+k <= len(b) }
	for i := 0; i < cnt; i++ {
		if !need(2) {
			return nil, errBadPage
		}
		kl := int(binary.BigEndian.Uint16(b[off:]))
		off += 2
		if !need(kl) {
			return nil, errBadPage
		}
		n.keys = append(n.keys, b[off:off+kl:off+kl])
		off += kl
		if !n.leaf {
			if !need(4) {
				return nil, errBadPage
			}
			n.children = append(n.children, binary.BigEndian.Uint32(b[off:]))
			off += 4
			continue
		}
		if !need(1) {
			return nil, errBadPage
		}
		switch b[off] {
		case 1:
			if !need(9) {
				return nil, errBadPage
			}
			n.vals = append(n.vals, value{overflow: true,
				total: binary.BigEndian.Uint32(b[off+1:]), first: binary.BigEndian.Uint32(b[off+5:])})
			off += 9
		case 0:
			if !need(3) {
				return nil, errBadPage
			}
			vl := int(binary.BigEndian.Uint16(b[off+1:]))
			off += 3
			if !need(vl) {
				return nil, errBadPage
			}
			n.vals = append(n.vals, value{inline: b[off : off+vl : off+vl]})
			off += vl
		default:
			return nil, errBadPage
		}
	}
	return n, nil
}

// Tree is a handle to a B+tree rooted at a fixed page. The root page number
// never changes, so it can be stored in a catalog.
type Tree struct {
	v    *pager.View
	root uint32
}

// Create allocates an empty tree and returns its root page.
func Create(v *pager.View) (uint32, error) {
	pg, err := v.Alloc()
	if err != nil {
		return 0, err
	}
	v.Write(pg, (&node{leaf: true}).encode())
	return pg, nil
}

// Open returns a handle for the tree rooted at root.
func Open(v *pager.View, root uint32) *Tree { return &Tree{v: v, root: root} }

func (t *Tree) load(pg uint32) (*node, error) {
	b, err := t.v.Read(pg)
	if err != nil {
		return nil, err
	}
	n, err := decode(b)
	if err != nil {
		return nil, fmt.Errorf("%w (page %d)", err, pg)
	}
	return n, nil
}

func (t *Tree) store(pg uint32, n *node) { t.v.Write(pg, n.encode()) }

// childIndex: children[i] holds keys < keys[i]; children[i+1] holds keys >= keys[i].
func childIndex(n *node, key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(key, n.keys[i]) < 0 })
}

func leafIndex(n *node, key []byte) (int, bool) {
	i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) >= 0 })
	return i, i < len(n.keys) && bytes.Equal(n.keys[i], key)
}

func (t *Tree) findLeaf(key []byte) (uint32, *node, error) {
	pg := t.root
	for {
		n, err := t.load(pg)
		if err != nil {
			return 0, nil, err
		}
		if n.leaf {
			return pg, n, nil
		}
		pg = n.children[childIndex(n, key)]
	}
}

// Get returns the value stored under key.
func (t *Tree) Get(key []byte) ([]byte, bool, error) {
	_, n, err := t.findLeaf(key)
	if err != nil {
		return nil, false, err
	}
	i, ok := leafIndex(n, key)
	if !ok {
		return nil, false, nil
	}
	v, err := t.readValue(n.vals[i])
	return v, err == nil, err
}

// Has reports whether key exists.
func (t *Tree) Has(key []byte) (bool, error) {
	_, n, err := t.findLeaf(key)
	if err != nil {
		return false, err
	}
	_, ok := leafIndex(n, key)
	return ok, nil
}

func (t *Tree) readValue(v value) ([]byte, error) {
	if !v.overflow {
		return v.inline, nil
	}
	out := make([]byte, 0, v.total)
	pg := v.first
	for pg != 0 && uint32(len(out)) < v.total {
		b, err := t.v.Read(pg)
		if err != nil {
			return nil, err
		}
		if b[0] != typeOverflow {
			return nil, errBadPage
		}
		used := int(binary.BigEndian.Uint16(b[5:]))
		out = append(out, b[ovHeader:ovHeader+used]...)
		pg = binary.BigEndian.Uint32(b[1:])
	}
	if uint32(len(out)) != v.total {
		return nil, errBadPage
	}
	return out, nil
}

func (t *Tree) writeValue(data []byte) (value, error) {
	if len(data) <= InlineMax {
		return value{inline: append([]byte(nil), data...)}, nil
	}
	// Allocate the chain back to front so each page knows its successor.
	nChunks := (len(data) + ovChunk - 1) / ovChunk
	next := uint32(0)
	for c := nChunks - 1; c >= 0; c-- {
		pg, err := t.v.Alloc()
		if err != nil {
			return value{}, err
		}
		chunk := data[c*ovChunk : min(len(data), (c+1)*ovChunk)]
		b := make([]byte, pager.PageSize)
		b[0] = typeOverflow
		binary.BigEndian.PutUint32(b[1:], next)
		binary.BigEndian.PutUint16(b[5:], uint16(len(chunk)))
		copy(b[ovHeader:], chunk)
		t.v.Write(pg, b)
		next = pg
	}
	return value{overflow: true, total: uint32(len(data)), first: next}, nil
}

func (t *Tree) freeValue(v value) error {
	pg := v.first
	for v.overflow && pg != 0 {
		b, err := t.v.Read(pg)
		if err != nil {
			return err
		}
		next := binary.BigEndian.Uint32(b[1:])
		t.v.Free(pg)
		pg = next
	}
	return nil
}

type split struct {
	sep   []byte
	right uint32
}

// Put inserts or replaces the value for key.
func (t *Tree) Put(key, val []byte) error {
	if len(key) > MaxKey {
		return ErrKeyTooLarge
	}
	v, err := t.writeValue(val)
	if err != nil {
		return err
	}
	s, err := t.insert(t.root, append([]byte(nil), key...), v)
	if err != nil || s == nil {
		return err
	}
	// Root split: move the (already updated) left half to a new page so that
	// the root page number stays stable.
	left, err := t.v.Alloc()
	if err != nil {
		return err
	}
	b, err := t.v.Read(t.root)
	if err != nil {
		return err
	}
	t.v.Write(left, append([]byte(nil), b...))
	t.store(t.root, &node{keys: [][]byte{s.sep}, children: []uint32{left, s.right}})
	return nil
}

func (t *Tree) insert(pg uint32, key []byte, v value) (*split, error) {
	n, err := t.load(pg)
	if err != nil {
		return nil, err
	}
	if n.leaf {
		i, found := leafIndex(n, key)
		if found {
			if err := t.freeValue(n.vals[i]); err != nil {
				return nil, err
			}
			n.vals[i] = v
		} else {
			n.keys = append(n.keys, nil)
			copy(n.keys[i+1:], n.keys[i:])
			n.keys[i] = key
			n.vals = append(n.vals, value{})
			copy(n.vals[i+1:], n.vals[i:])
			n.vals[i] = v
		}
		if n.size() <= pager.UsableSize {
			t.store(pg, n)
			return nil, nil
		}
		return t.splitLeaf(pg, n)
	}
	ci := childIndex(n, key)
	s, err := t.insert(n.children[ci], key, v)
	if err != nil || s == nil {
		return nil, err
	}
	n.keys = append(n.keys, nil)
	copy(n.keys[ci+1:], n.keys[ci:])
	n.keys[ci] = s.sep
	n.children = append(n.children, 0)
	copy(n.children[ci+2:], n.children[ci+1:])
	n.children[ci+1] = s.right
	if n.size() <= pager.UsableSize {
		t.store(pg, n)
		return nil, nil
	}
	return t.splitInternal(pg, n)
}

func (t *Tree) splitLeaf(pg uint32, n *node) (*split, error) {
	// Split by bytes so both halves fit even with variable-size cells.
	half, acc, m := n.size()/2, nodeHeader, 0
	for m < len(n.keys)-1 {
		acc += (&node{leaf: true, keys: n.keys[m : m+1], vals: n.vals[m : m+1]}).size() - nodeHeader
		m++
		if acc >= half {
			break
		}
	}
	rpg, err := t.v.Alloc()
	if err != nil {
		return nil, err
	}
	right := &node{leaf: true, keys: append([][]byte(nil), n.keys[m:]...), vals: append([]value(nil), n.vals[m:]...), next: n.next}
	left := &node{leaf: true, keys: n.keys[:m], vals: n.vals[:m], next: rpg}
	t.store(pg, left)
	t.store(rpg, right)
	return &split{sep: right.keys[0], right: rpg}, nil
}

func (t *Tree) splitInternal(pg uint32, n *node) (*split, error) {
	// Split by bytes; keys[m] moves up to the parent.
	half, acc, m := n.size()/2, nodeHeader, 0
	for m < len(n.keys)-2 {
		acc += 2 + len(n.keys[m]) + 4
		m++
		if acc >= half {
			break
		}
	}
	rpg, err := t.v.Alloc()
	if err != nil {
		return nil, err
	}
	right := &node{keys: append([][]byte(nil), n.keys[m+1:]...), children: append([]uint32(nil), n.children[m+1:]...)}
	left := &node{keys: n.keys[:m], children: n.children[:m+1]}
	t.store(pg, left)
	t.store(rpg, right)
	return &split{sep: n.keys[m], right: rpg}, nil
}

// Delete removes key and reports whether it existed.
func (t *Tree) Delete(key []byte) (bool, error) {
	pg, n, err := t.findLeaf(key)
	if err != nil {
		return false, err
	}
	i, ok := leafIndex(n, key)
	if !ok {
		return false, nil
	}
	if err := t.freeValue(n.vals[i]); err != nil {
		return false, err
	}
	n.keys = append(n.keys[:i], n.keys[i+1:]...)
	n.vals = append(n.vals[:i], n.vals[i+1:]...)
	t.store(pg, n)
	return true, nil
}

// Last returns the greatest key in the tree.
func (t *Tree) Last() ([]byte, bool, error) {
	return t.lastIn(t.root)
}

func (t *Tree) lastIn(pg uint32) ([]byte, bool, error) {
	n, err := t.load(pg)
	if err != nil {
		return nil, false, err
	}
	if n.leaf {
		if len(n.keys) == 0 {
			return nil, false, nil
		}
		return n.keys[len(n.keys)-1], true, nil
	}
	// Leaves may be empty after deletes, so fall back to left siblings.
	for i := len(n.children) - 1; i >= 0; i-- {
		k, ok, err := t.lastIn(n.children[i])
		if err != nil || ok {
			return k, ok, err
		}
	}
	return nil, false, nil
}

// Drop frees every page of the tree rooted at root, including the root.
func Drop(v *pager.View, root uint32) error {
	t := Open(v, root)
	return t.drop(root)
}

func (t *Tree) drop(pg uint32) error {
	n, err := t.load(pg)
	if err != nil {
		return err
	}
	if n.leaf {
		for _, v := range n.vals {
			if err := t.freeValue(v); err != nil {
				return err
			}
		}
	} else {
		for _, c := range n.children {
			if err := t.drop(c); err != nil {
				return err
			}
		}
	}
	t.v.Free(pg)
	return nil
}

// Cursor iterates over the tree in key order.
type Cursor struct {
	t   *Tree
	n   *node
	i   int
	err error
}

// Seek positions a cursor at the first key >= key (nil = first key).
func (t *Tree) Seek(key []byte) *Cursor {
	c := &Cursor{t: t}
	_, n, err := t.findLeaf(key)
	if err != nil {
		c.err = err
		return c
	}
	c.n = n
	c.i, _ = leafIndex(n, key)
	c.normalize()
	return c
}

func (c *Cursor) normalize() {
	for c.n != nil && c.i >= len(c.n.keys) {
		if c.n.next == 0 {
			c.n = nil
			return
		}
		n, err := c.t.load(c.n.next)
		if err != nil {
			c.err, c.n = err, nil
			return
		}
		c.n, c.i = n, 0
	}
}

// Valid reports whether the cursor points at an entry.
func (c *Cursor) Valid() bool { return c.n != nil && c.err == nil }

// Err returns the first error encountered.
func (c *Cursor) Err() error { return c.err }

// Key returns the current key. It must not be modified.
func (c *Cursor) Key() []byte { return c.n.keys[c.i] }

// Value returns the current value, reading overflow pages if needed.
func (c *Cursor) Value() ([]byte, error) { return c.t.readValue(c.n.vals[c.i]) }

// Next advances to the next entry.
func (c *Cursor) Next() {
	if c.n == nil {
		return
	}
	c.i++
	c.normalize()
}
