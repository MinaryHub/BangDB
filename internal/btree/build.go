package btree

import (
	"bytes"
	"errors"

	"github.com/minaryhub/bangdb/internal/pager"
)

// buildFill is how full Builder packs pages, leaving some room so that the
// first inserts after a rebuild do not split every page.
const buildFill = pager.UsableSize * 9 / 10

// Builder creates a new tree from entries added in strictly ascending key
// order, writing each page once and packing pages densely.
type Builder struct {
	t     *Tree
	cur   *node
	pg    uint32
	level []buildEntry // finished leaves
	last  []byte
}

type buildEntry struct {
	min []byte // smallest key in the subtree
	pg  uint32
}

// NewBuilder starts a tree in v.
func NewBuilder(v *pager.View) *Builder { return &Builder{t: &Tree{v: v}} }

// SetView continues the build in another view of the same pager (after the
// previous one was committed, to bound memory use).
func (b *Builder) SetView(v *pager.View) { b.t.v = v }

// Add appends an entry; keys must be strictly ascending.
func (b *Builder) Add(key, val []byte) error {
	if len(key) > MaxKey {
		return ErrKeyTooLarge
	}
	if b.last != nil && bytes.Compare(key, b.last) <= 0 {
		return errors.New("btree: builder keys out of order")
	}
	b.last = append(b.last[:0], key...)
	v, err := b.t.writeValue(val)
	if err != nil {
		return err
	}
	if b.cur == nil {
		if b.pg, err = b.t.v.Alloc(); err != nil {
			return err
		}
		b.cur = &node{leaf: true}
	}
	b.cur.keys = append(b.cur.keys, append([]byte(nil), key...))
	b.cur.vals = append(b.cur.vals, v)
	if b.cur.size() <= buildFill || len(b.cur.keys) == 1 {
		return nil
	}
	// Close the leaf without its last entry, which starts the next one.
	n := len(b.cur.keys) - 1
	lastKey, lastVal := b.cur.keys[n], b.cur.vals[n]
	b.cur.keys, b.cur.vals = b.cur.keys[:n], b.cur.vals[:n]
	next, err := b.t.v.Alloc()
	if err != nil {
		return err
	}
	b.cur.next = next
	b.t.store(b.pg, b.cur)
	b.level = append(b.level, buildEntry{b.cur.keys[0], b.pg})
	b.cur, b.pg = &node{leaf: true, keys: [][]byte{lastKey}, vals: []value{lastVal}}, next
	return nil
}

// Finish writes the remaining pages and returns the root page.
func (b *Builder) Finish() (uint32, error) {
	if b.cur == nil {
		return Create(b.t.v)
	}
	b.t.store(b.pg, b.cur)
	level := append(b.level, buildEntry{b.cur.keys[0], b.pg})
	b.cur, b.level = nil, nil
	for len(level) > 1 {
		var groups [][]buildEntry
		var g []buildEntry
		size := nodeHeader
		for _, e := range level {
			if len(g) > 0 && size+2+len(e.min)+4 > buildFill {
				groups = append(groups, g)
				g, size = nil, nodeHeader
			}
			if len(g) > 0 {
				size += 2 + len(e.min) + 4
			}
			g = append(g, e)
		}
		if len(g) == 1 && len(groups) > 0 {
			// An internal node needs two children: borrow one.
			prev := groups[len(groups)-1]
			g = append([]buildEntry{prev[len(prev)-1]}, g...)
			groups[len(groups)-1] = prev[:len(prev)-1]
		}
		groups = append(groups, g)
		var up []buildEntry
		for _, g := range groups {
			n := &node{children: []uint32{g[0].pg}}
			for _, e := range g[1:] {
				n.keys = append(n.keys, e.min)
				n.children = append(n.children, e.pg)
			}
			pg, err := b.t.v.Alloc()
			if err != nil {
				return 0, err
			}
			b.t.store(pg, n)
			up = append(up, buildEntry{g[0].min, pg})
		}
		level = up
	}
	return level[0].pg, nil
}
