// Package memtree is a persistent (copy-on-write) B+tree of Go values, used
// by the in-memory table store.
//
// A Tree value is immutable once published: changes copy the nodes on the
// path from the root and return a new Tree, so snapshots are just old Tree
// values. Copying every node of every change would be wasteful inside one
// transaction, so changes carry a mutation token (pager.Token): a node
// created under the current token is changed in place, older nodes are
// copied first. The pager starts a new token at every savepoint, so a state
// that may be returned to is never changed.
package memtree

import (
	"sort"

	"github.com/minaryhub/bangdb/internal/pager"
)

const (
	maxEntries = 64 // per leaf, and children per internal node
	minEntries = maxEntries / 4
)

// Tree is a sorted map from K to V. The zero value is not usable; use New.
type Tree[K, V any] struct {
	root *node[K, V]
	size int
	cmp  func(a, b K) int
}

type node[K, V any] struct {
	owner *pager.Token
	// Leaf: entry keys. Internal: keys[i] separates kids[i] (keys < keys[i])
	// from kids[i+1] (keys >= keys[i]).
	keys []K
	vals []V // leaves
	kids []*node[K, V]
}

func (n *node[K, V]) leaf() bool { return n.kids == nil }

// entries is the fill measure: entries of a leaf, children of an internal
// node.
func (n *node[K, V]) entries() int {
	if n.leaf() {
		return len(n.keys)
	}
	return len(n.kids)
}

// New returns an empty tree ordered by cmp.
func New[K, V any](cmp func(a, b K) int) Tree[K, V] { return Tree[K, V]{cmp: cmp} }

// Len returns the number of entries.
func (t Tree[K, V]) Len() int { return t.size }

// lowerBound is the first index i with keys[i] >= k.
func (t Tree[K, V]) lowerBound(keys []K, k K) int {
	return sort.Search(len(keys), func(i int) bool { return t.cmp(keys[i], k) >= 0 })
}

// child is the index of the child of internal node n that may hold k.
func (t Tree[K, V]) child(n *node[K, V], k K) int {
	return sort.Search(len(n.keys), func(i int) bool { return t.cmp(k, n.keys[i]) < 0 })
}

// Get returns the value stored under k.
func (t Tree[K, V]) Get(k K) (V, bool) {
	n := t.root
	for n != nil && !n.leaf() {
		n = n.kids[t.child(n, k)]
	}
	if n != nil {
		if i := t.lowerBound(n.keys, k); i < len(n.keys) && t.cmp(n.keys[i], k) == 0 {
			return n.vals[i], true
		}
	}
	var zero V
	return zero, false
}

func (n *node[K, V]) mutable(tok *pager.Token) *node[K, V] {
	if n.owner == tok {
		return n
	}
	c := &node[K, V]{owner: tok, keys: append(make([]K, 0, len(n.keys)+1), n.keys...)}
	if n.leaf() {
		c.vals = append(make([]V, 0, len(n.vals)+1), n.vals...)
	} else {
		c.kids = append(make([]*node[K, V], 0, len(n.kids)+1), n.kids...)
	}
	return c
}

// Put returns the tree with k set to v.
func (t Tree[K, V]) Put(tok *pager.Token, k K, v V) Tree[K, V] {
	if t.root == nil {
		t.root = &node[K, V]{owner: tok, keys: []K{k}, vals: []V{v}}
		t.size = 1
		return t
	}
	r := t.root.mutable(tok)
	right, sep, replaced := t.insert(r, tok, k, v)
	if right != nil {
		r = &node[K, V]{owner: tok, keys: []K{sep}, kids: []*node[K, V]{r, right}}
	}
	t.root = r
	if !replaced {
		t.size++
	}
	return t
}

// insert adds k to the mutable node n. If n overflows it is split and the
// new right sibling and its separator are returned.
func (t Tree[K, V]) insert(n *node[K, V], tok *pager.Token, k K, v V) (right *node[K, V], sep K, replaced bool) {
	if n.leaf() {
		i := t.lowerBound(n.keys, k)
		if i < len(n.keys) && t.cmp(n.keys[i], k) == 0 {
			n.vals[i] = v
			return nil, sep, true
		}
		n.keys = insertAt(n.keys, i, k)
		n.vals = insertAt(n.vals, i, v)
		if len(n.keys) <= maxEntries {
			return nil, sep, false
		}
		mid := len(n.keys) / 2
		right = &node[K, V]{owner: tok, keys: append([]K(nil), n.keys[mid:]...), vals: append([]V(nil), n.vals[mid:]...)}
		n.keys, n.vals = n.keys[:mid], clearTail(n.vals, mid)
		return right, right.keys[0], false
	}
	ci := t.child(n, k)
	c := n.kids[ci].mutable(tok)
	n.kids[ci] = c
	r, s, replaced := t.insert(c, tok, k, v)
	if r == nil {
		return nil, sep, replaced
	}
	n.keys = insertAt(n.keys, ci, s)
	n.kids = insertAt(n.kids, ci+1, r)
	if len(n.kids) <= maxEntries {
		return nil, sep, replaced
	}
	mid := len(n.kids) / 2
	right = &node[K, V]{owner: tok, keys: append([]K(nil), n.keys[mid:]...), kids: append([]*node[K, V](nil), n.kids[mid:]...)}
	sep = n.keys[mid-1]
	n.keys, n.kids = n.keys[:mid-1], clearTail(n.kids, mid)
	return right, sep, replaced
}

// Delete returns the tree without k and whether k was present.
func (t Tree[K, V]) Delete(tok *pager.Token, k K) (Tree[K, V], bool) {
	if _, ok := t.Get(k); !ok {
		return t, false
	}
	r := t.root.mutable(tok)
	t.remove(r, tok, k)
	switch {
	case r.leaf() && len(r.keys) == 0:
		r = nil
	case !r.leaf() && len(r.kids) == 1:
		r = r.kids[0]
	}
	t.root = r
	t.size--
	return t, true
}

// remove deletes k (known to be present) below the mutable node n.
func (t Tree[K, V]) remove(n *node[K, V], tok *pager.Token, k K) {
	if n.leaf() {
		i := t.lowerBound(n.keys, k)
		n.keys = removeAt(n.keys, i)
		n.vals = removeAt(n.vals, i)
		return
	}
	ci := t.child(n, k)
	c := n.kids[ci].mutable(tok)
	n.kids[ci] = c
	t.remove(c, tok, k)
	if c.entries() < minEntries {
		t.rebalance(n, tok, ci)
	}
}

// rebalance fixes the underfull child ci of n by merging it with a sibling
// or borrowing one entry from it.
func (t Tree[K, V]) rebalance(n *node[K, V], tok *pager.Token, ci int) {
	li := ci - 1
	if ci == 0 {
		li = 0
	}
	if len(n.kids) < 2 {
		return
	}
	left, right := n.kids[li].mutable(tok), n.kids[li+1].mutable(tok)
	n.kids[li], n.kids[li+1] = left, right
	if left.entries()+right.entries() <= maxEntries {
		if left.leaf() {
			left.keys = append(left.keys, right.keys...)
			left.vals = append(left.vals, right.vals...)
		} else {
			left.keys = append(append(left.keys, n.keys[li]), right.keys...)
			left.kids = append(left.kids, right.kids...)
		}
		n.keys = removeAt(n.keys, li)
		n.kids = removeAt(n.kids, li+1)
		return
	}
	if ci == li { // the left one is short: take the right's first entry
		if left.leaf() {
			left.keys = append(left.keys, right.keys[0])
			left.vals = append(left.vals, right.vals[0])
			right.keys, right.vals = removeAt(right.keys, 0), removeAt(right.vals, 0)
			n.keys[li] = right.keys[0]
		} else {
			left.keys = append(left.keys, n.keys[li])
			left.kids = append(left.kids, right.kids[0])
			n.keys[li] = right.keys[0]
			right.keys, right.kids = removeAt(right.keys, 0), removeAt(right.kids, 0)
		}
		return
	}
	// The right one is short: take the left's last entry.
	last := len(left.keys) - 1
	if left.leaf() {
		right.keys = insertAt(right.keys, 0, left.keys[last])
		right.vals = insertAt(right.vals, 0, left.vals[last])
		left.keys, left.vals = left.keys[:last], clearTail(left.vals, last)
		n.keys[li] = right.keys[0]
	} else {
		right.keys = insertAt(right.keys, 0, n.keys[li])
		right.kids = insertAt(right.kids, 0, left.kids[len(left.kids)-1])
		n.keys[li] = left.keys[last]
		left.keys, left.kids = left.keys[:last], clearTail(left.kids, len(left.kids)-1)
	}
}

// Last returns the greatest key.
func (t Tree[K, V]) Last() (K, bool) {
	var zero K
	n := t.root
	if n == nil {
		return zero, false
	}
	for !n.leaf() {
		n = n.kids[len(n.kids)-1]
	}
	return n.keys[len(n.keys)-1], true
}

func insertAt[T any](s []T, i int, v T) []T {
	var zero T
	s = append(s, zero)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func removeAt[T any](s []T, i int) []T {
	copy(s[i:], s[i+1:])
	var zero T
	s[len(s)-1] = zero // let the garbage collector have it
	return s[:len(s)-1]
}

// clearTail shortens s to n, clearing the dropped elements.
func clearTail[T any](s []T, n int) []T {
	clear(s[n:])
	return s[:n]
}

// ---- iteration ----

// Iter walks the tree in key order (or reverse). It must not be used across
// changes made with the token of the tree it iterates.
type Iter[K, V any] struct {
	stack []frame[K, V] // internal nodes and the index of the child taken
	leaf  *node[K, V]
	i     int
	desc  bool
	buf   [6]frame[K, V] // stack storage for trees up to 7 levels
}

func newIter[K, V any](desc bool) *Iter[K, V] {
	it := &Iter[K, V]{desc: desc}
	it.stack = it.buf[:0]
	return it
}

type frame[K, V any] struct {
	n *node[K, V]
	i int
}

// Seek returns an ascending iterator from the first key >= k.
func (t Tree[K, V]) Seek(k K) *Iter[K, V] {
	it := newIter[K, V](false)
	n := t.root
	if n == nil {
		return it
	}
	for !n.leaf() {
		ci := t.child(n, k)
		it.stack = append(it.stack, frame[K, V]{n, ci})
		n = n.kids[ci]
	}
	it.leaf, it.i = n, t.lowerBound(n.keys, k)
	it.settle()
	return it
}

// First returns an ascending iterator from the smallest key.
func (t Tree[K, V]) First() *Iter[K, V] {
	it := newIter[K, V](false)
	if t.root != nil {
		it.down(t.root, false)
	}
	return it
}

// SeekReverse returns a descending iterator from the greatest key <= k.
func (t Tree[K, V]) SeekReverse(k K) *Iter[K, V] {
	it := newIter[K, V](true)
	n := t.root
	if n == nil {
		return it
	}
	for !n.leaf() {
		ci := t.child(n, k)
		it.stack = append(it.stack, frame[K, V]{n, ci})
		n = n.kids[ci]
	}
	it.leaf = n
	it.i = sort.Search(len(n.keys), func(i int) bool { return t.cmp(n.keys[i], k) > 0 }) - 1
	it.settle()
	return it
}

// LastIter returns a descending iterator from the greatest key.
func (t Tree[K, V]) LastIter() *Iter[K, V] {
	it := newIter[K, V](true)
	if t.root != nil {
		it.down(t.root, true)
	}
	return it
}

// down descends from n to its first (or last) leaf.
func (it *Iter[K, V]) down(n *node[K, V], last bool) {
	for !n.leaf() {
		ci := 0
		if last {
			ci = len(n.kids) - 1
		}
		it.stack = append(it.stack, frame[K, V]{n, ci})
		n = n.kids[ci]
	}
	it.leaf, it.i = n, 0
	if last {
		it.i = len(n.keys) - 1
	}
}

// settle moves to the next leaf (in the iteration direction) while the
// position is outside the current leaf.
func (it *Iter[K, V]) settle() {
	for it.leaf != nil && (it.i < 0 || it.i >= len(it.leaf.keys)) {
		// Climb to the nearest ancestor with a sibling in our direction.
		for {
			if len(it.stack) == 0 {
				it.leaf = nil
				return
			}
			f := &it.stack[len(it.stack)-1]
			if it.desc {
				f.i--
			} else {
				f.i++
			}
			if f.i >= 0 && f.i < len(f.n.kids) {
				it.down(f.n.kids[f.i], it.desc)
				break
			}
			it.stack = it.stack[:len(it.stack)-1]
		}
	}
}

// Valid reports whether the iterator is at an entry.
func (it *Iter[K, V]) Valid() bool { return it.leaf != nil }

// Key returns the current key.
func (it *Iter[K, V]) Key() K { return it.leaf.keys[it.i] }

// Value returns the current value.
func (it *Iter[K, V]) Value() V { return it.leaf.vals[it.i] }

// Next moves to the next entry in the iteration direction.
func (it *Iter[K, V]) Next() {
	if it.desc {
		it.i--
	} else {
		it.i++
	}
	it.settle()
}
