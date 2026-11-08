// Package btree implements a B+tree of byte-string keys and values stored in
// pager pages. Leaves are linked left-to-right for range scans. Values larger
// than InlineMax are spilled to overflow page chains.
//
// Deletion rebalances: a node that falls below a quarter of a page is merged
// with a sibling (freeing a page) or takes entries from it. Builder loads
// sorted entries into densely packed pages (used by VACUUM).
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/minaryhub/bangdb/internal/pager"
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

// load returns the node at pg. It may be shared with other readers through
// the pager's decoded-page cache and must not be modified; use loadMut.
func (t *Tree) load(pg uint32) (*node, error) {
	b, err := t.v.Read(pg)
	if err != nil {
		return nil, err
	}
	obj, err := t.v.Decoded(b, decodeAny)
	if err != nil {
		return nil, fmt.Errorf("%w (page %d)", err, pg)
	}
	return obj.(*node), nil
}

func decodeAny(b []byte) (any, error) { return decode(b) }

// loadDescend returns the node at pg for a writer walking down the tree:
// the view's own copy if it has one (the writer may change it next), else
// the shared decoded node. The caller must not modify it; use loadMut.
func (t *Tree) loadDescend(pg uint32) (*node, error) {
	if l, ok := t.v.PeekLazy(pg); ok {
		return l.(*node), nil
	}
	b, err := t.v.Read(pg)
	if err != nil {
		return nil, err
	}
	if obj, ok := t.v.PeekOwned(pg, b); ok {
		return obj.(*node), nil
	}
	obj, err := t.v.Decoded(b, decodeAny)
	if err != nil {
		return nil, fmt.Errorf("%w (page %d)", err, pg)
	}
	return obj.(*node), nil
}

// loadMut returns a private copy of the node at pg for modification. It
// does not populate the decoded-page cache: the page is about to change.
func (t *Tree) loadMut(pg uint32) (*node, error) {
	if l, ok := t.v.TakeLazy(pg); ok {
		return l.(*node), nil // the view's own: changed in place
	}
	b, err := t.v.Read(pg)
	if err != nil {
		return nil, err
	}
	if obj, ok := t.v.TakeOwned(pg, b); ok {
		return obj.(*node), nil // written by this view: already private
	}
	obj, ok := t.v.Cached(b)
	if !ok {
		n, err := decode(b)
		if err != nil {
			return nil, fmt.Errorf("%w (page %d)", err, pg)
		}
		return n, nil // freshly decoded: already private
	}
	n := obj.(*node)
	c := *n
	c.keys = append(make([][]byte, 0, len(n.keys)+1), n.keys...)
	if n.leaf {
		c.vals = append(make([]value, 0, len(n.vals)+1), n.vals...)
	} else {
		c.children = append(make([]uint32, 0, len(n.children)+1), n.children...)
	}
	return &c, nil
}

// store writes n to page pg. The page stays in decoded form in the view
// (pager.Lazy) and is encoded at commit or when its bytes are read.
func (t *Tree) store(pg uint32, n *node) { t.v.WriteLazy(pg, n) }

// Encode implements pager.Lazy.
func (n *node) Encode() []byte { return n.encode() }

// Clone implements pager.Lazy.
func (n *node) Clone() pager.Lazy {
	c := *n
	c.keys = append([][]byte(nil), n.keys...)
	c.vals = append([]value(nil), n.vals...)
	c.children = append([]uint32(nil), n.children...)
	if n.leaf {
		c.children = nil
	} else {
		c.vals = nil
	}
	return &c
}

// childIndex: children[i] holds keys < keys[i]; children[i+1] holds keys >= keys[i].
func childIndex(n *node, key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(key, n.keys[i]) < 0 })
}

func leafIndex(n *node, key []byte) (int, bool) {
	i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) >= 0 })
	return i, i < len(n.keys) && bytes.Equal(n.keys[i], key)
}

// findLeaf serves point lookups (Get, Has): they hold no node after
// returning, so the view's own nodes can be used (loadDescend), which
// spares decoding pages the writer has just changed.
func (t *Tree) findLeaf(key []byte) (uint32, *node, error) {
	pg := t.root
	for {
		n, err := t.loadDescend(pg)
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
	// Internal nodes are descended through the shared (read-only) decoded
	// node; a private copy is made only when a child split changes them.
	n, err := t.loadDescend(pg)
	if err != nil {
		return nil, err
	}
	if n.leaf {
		if n, err = t.loadMut(pg); err != nil {
			return nil, err
		}
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
	if n, err = t.loadMut(pg); err != nil {
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

// leafSplit returns the index of the first entry of the right half when
// splitting leaf n by bytes, so both halves fit even with variable-size cells.
func leafSplit(n *node) int {
	half, acc, m := n.size()/2, nodeHeader, 0
	for m < len(n.keys)-1 {
		acc += (&node{leaf: true, keys: n.keys[m : m+1], vals: n.vals[m : m+1]}).size() - nodeHeader
		m++
		if acc >= half {
			break
		}
	}
	return m
}

// internalSplit returns the index of the key that moves up when splitting
// internal node n by bytes.
func internalSplit(n *node) int {
	half, acc, m := n.size()/2, nodeHeader, 0
	for m < len(n.keys)-2 {
		acc += 2 + len(n.keys[m]) + 4
		m++
		if acc >= half {
			break
		}
	}
	return m
}

func (t *Tree) splitLeaf(pg uint32, n *node) (*split, error) {
	m := leafSplit(n)
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
	m := internalSplit(n) // keys[m] moves up to the parent
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

// minFill is the size below which a non-root node is rebalanced.
const minFill = pager.UsableSize / 4

// Delete removes key and reports whether it existed.
func (t *Tree) Delete(key []byte) (bool, error) {
	found, _, err := t.del(t.root, key)
	if err != nil || !found {
		return found, err
	}
	// An internal root left with a single child absorbs it, so the tree
	// shrinks in height while the root page number stays the same.
	for {
		n, err := t.load(t.root)
		if err != nil {
			return true, err
		}
		if n.leaf || len(n.keys) > 0 {
			return true, nil
		}
		child := n.children[0]
		b, err := t.v.Read(child)
		if err != nil {
			return true, err
		}
		t.v.Write(t.root, append([]byte(nil), b...))
		t.v.Free(child)
	}
}

// del removes key from the subtree at pg. under reports that the node at pg
// is now below minFill.
func (t *Tree) del(pg uint32, key []byte) (found, under bool, err error) {
	n, err := t.loadDescend(pg) // read-only until something changes (see insert)
	if err != nil {
		return false, false, err
	}
	if n.leaf {
		if n, err = t.loadMut(pg); err != nil {
			return false, false, err
		}
		i, ok := leafIndex(n, key)
		if !ok {
			return false, false, nil
		}
		if err := t.freeValue(n.vals[i]); err != nil {
			return false, false, err
		}
		n.keys = append(n.keys[:i], n.keys[i+1:]...)
		n.vals = append(n.vals[:i], n.vals[i+1:]...)
		t.store(pg, n)
		return true, n.size() < minFill, nil
	}
	ci := childIndex(n, key)
	found, under, err = t.del(n.children[ci], key)
	if err != nil || !found || !under {
		return found, false, err
	}
	if n, err = t.loadMut(pg); err != nil {
		return true, false, err
	}
	changed, err := t.rebalance(n, ci)
	if err != nil || !changed {
		return true, false, err
	}
	t.store(pg, n)
	return true, n.size() < minFill, nil
}

// rebalance fixes the underfull child ci of n by merging it with a sibling
// or, when both do not fit in one page, by sharing entries evenly. It
// reports whether n changed (the caller stores it).
func (t *Tree) rebalance(n *node, ci int) (bool, error) {
	if len(n.children) < 2 {
		return false, nil
	}
	li := ci - 1
	if ci == 0 {
		li = 0
	}
	lpg, rpg := n.children[li], n.children[li+1]
	left, err := t.loadDescend(lpg) // read only: the result is built anew
	if err != nil {
		return false, err
	}
	right, err := t.loadDescend(rpg)
	if err != nil {
		return false, err
	}
	var merged *node
	if left.leaf {
		merged = &node{leaf: true, next: right.next,
			keys: append(append([][]byte(nil), left.keys...), right.keys...),
			vals: append(append([]value(nil), left.vals...), right.vals...)}
	} else {
		merged = &node{
			keys:     append(append(append([][]byte(nil), left.keys...), n.keys[li]), right.keys...),
			children: append(append([]uint32(nil), left.children...), right.children...)}
	}
	if merged.size() <= pager.UsableSize {
		t.store(lpg, merged)
		t.v.Free(rpg)
		n.keys = append(n.keys[:li], n.keys[li+1:]...)
		n.children = append(n.children[:li+1], n.children[li+2:]...)
		return true, nil
	}
	// Redistribute. The new separator may be longer than the old one; give
	// up if the parent would no longer fit in its page.
	var nl, nr *node
	var sep []byte
	if merged.leaf {
		m := leafSplit(merged)
		// Capped slices: the halves share arrays and both stay owned by
		// the view (see store), so appending to one must not clobber the
		// other.
		nl = &node{leaf: true, keys: merged.keys[:m:m], vals: merged.vals[:m:m], next: rpg}
		nr = &node{leaf: true, keys: merged.keys[m:], vals: merged.vals[m:], next: right.next}
		sep = nr.keys[0]
	} else {
		m := internalSplit(merged)
		nl = &node{keys: merged.keys[:m:m], children: merged.children[: m+1 : m+1]}
		nr = &node{keys: merged.keys[m+1:], children: merged.children[m+1:]}
		sep = merged.keys[m]
	}
	if n.size()-len(n.keys[li])+len(sep) > pager.UsableSize {
		return false, nil
	}
	t.store(lpg, nl)
	t.store(rpg, nr)
	n.keys[li] = sep
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

// RevCursor iterates over the tree in descending key order. Leaves are only
// linked forward, so it keeps the path from the root.
type RevCursor struct {
	t     *Tree
	stack []revFrame // internal nodes on the path, with the child index taken
	n     *node      // current leaf
	i     int
	err   error
}

type revFrame struct {
	n  *node
	ci int
}

// SeekReverse positions a reverse cursor at the last key <= key (nil = the
// last key of the tree).
func (t *Tree) SeekReverse(key []byte) *RevCursor {
	c := &RevCursor{t: t}
	pg := t.root
	for {
		n, err := t.load(pg)
		if err != nil {
			c.err = err
			return c
		}
		if n.leaf {
			c.n = n
			if key == nil {
				c.i = len(n.keys) - 1
			} else {
				c.i = sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) > 0 }) - 1
			}
			break
		}
		ci := len(n.children) - 1
		if key != nil {
			ci = childIndex(n, key)
		}
		c.stack = append(c.stack, revFrame{n, ci})
		pg = n.children[ci]
	}
	if c.i < 0 {
		c.Prev()
	}
	return c
}

// Valid reports whether the cursor points at an entry.
func (c *RevCursor) Valid() bool { return c.n != nil && c.err == nil && c.i >= 0 }

// Err returns the first error encountered.
func (c *RevCursor) Err() error { return c.err }

// Key returns the current key. It must not be modified.
func (c *RevCursor) Key() []byte { return c.n.keys[c.i] }

// Value returns the current value.
func (c *RevCursor) Value() ([]byte, error) { return c.t.readValue(c.n.vals[c.i]) }

// Prev moves to the previous entry.
func (c *RevCursor) Prev() {
	if c.n == nil {
		return
	}
	c.i--
	for c.i < 0 {
		// Climb to the nearest ancestor with a left sibling subtree.
		for len(c.stack) > 0 && c.stack[len(c.stack)-1].ci == 0 {
			c.stack = c.stack[:len(c.stack)-1]
		}
		if len(c.stack) == 0 {
			c.n = nil
			return
		}
		top := &c.stack[len(c.stack)-1]
		top.ci--
		pg := top.n.children[top.ci]
		// Descend along the rightmost path.
		for {
			n, err := c.t.load(pg)
			if err != nil {
				c.err, c.n = err, nil
				return
			}
			if n.leaf {
				c.n, c.i = n, len(n.keys)-1
				break
			}
			c.stack = append(c.stack, revFrame{n, len(n.children) - 1})
			pg = n.children[len(n.children)-1]
		}
	}
}

// Estimate returns an approximate number of entries: the fan-out of the
// nodes on the path to a middle leaf, multiplied together and by the size of
// that leaf. It reads one page per level. Exact for single-leaf trees.
func (t *Tree) Estimate() (int64, error) {
	est := int64(1)
	pg := t.root
	for depth := 0; depth < 64; depth++ {
		n, err := t.load(pg)
		if err != nil {
			return 0, err
		}
		if n.leaf {
			return est * int64(len(n.keys)), nil
		}
		est *= int64(len(n.children))
		pg = n.children[len(n.children)/2]
	}
	return 0, errBadPage
}
