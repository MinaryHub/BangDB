package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/minaryhub/bangdb/internal/pager"
)

// CheckResult summarizes a structural check of one tree.
type CheckResult struct {
	Entries  int
	Problems []string
}

// Check verifies the tree rooted at root: every node decodes, keys are
// strictly ascending and within the bounds set by their parents, all leaves
// are at the same depth, the leaf chain links the leaves in key order, and
// overflow chains have the recorded length. mark is called for every page the
// tree uses (nodes and overflow pages) so callers can detect shared or lost
// pages.
func Check(v *pager.View, root uint32, mark func(pg uint32)) CheckResult {
	c := &checker{t: Open(v, root), mark: mark}
	c.walk(root, nil, nil, 0)
	for i, pg := range c.leaves {
		want := uint32(0)
		if i+1 < len(c.leaves) {
			want = c.leaves[i+1]
		}
		if c.nexts[i] != want {
			c.problem("leaf %d links to %d, expected %d", pg, c.nexts[i], want)
		}
	}
	return CheckResult{Entries: c.entries, Problems: c.problems}
}

type checker struct {
	t         *Tree
	mark      func(uint32)
	leafDepth int
	leaves    []uint32
	nexts     []uint32
	entries   int
	problems  []string
}

func (c *checker) problem(format string, args ...any) {
	if len(c.problems) < 100 {
		c.problems = append(c.problems, fmt.Sprintf(format, args...))
	}
}

// walk checks the subtree at pg whose keys must be in [lo, hi).
func (c *checker) walk(pg uint32, lo, hi []byte, depth int) {
	if depth > 64 {
		c.problem("tree deeper than 64 levels at page %d (cycle?)", pg)
		return
	}
	c.mark(pg)
	n, err := c.t.load(pg)
	if err != nil {
		c.problem("page %d: %v", pg, err)
		return
	}
	if n.size() > pager.UsableSize {
		c.problem("page %d: content larger than a page", pg)
	}
	for i, k := range n.keys {
		if i > 0 && bytes.Compare(n.keys[i-1], k) >= 0 {
			c.problem("page %d: keys out of order at %d", pg, i)
		}
		if lo != nil && bytes.Compare(k, lo) < 0 || hi != nil && bytes.Compare(k, hi) >= 0 {
			c.problem("page %d: key %d outside the range given by its parent", pg, i)
		}
	}
	if n.leaf {
		if len(c.leaves) == 0 {
			c.leafDepth = depth
		} else if depth != c.leafDepth {
			c.problem("leaf %d at depth %d, other leaves at %d", pg, depth, c.leafDepth)
		}
		c.leaves = append(c.leaves, pg)
		c.nexts = append(c.nexts, n.next)
		c.entries += len(n.keys)
		for i, v := range n.vals {
			if v.overflow {
				c.checkOverflow(pg, i, v)
			}
		}
		return
	}
	if len(n.children) != len(n.keys)+1 {
		c.problem("page %d: %d keys but %d children", pg, len(n.keys), len(n.children))
		return
	}
	for i, child := range n.children {
		clo, chi := lo, hi
		if i > 0 {
			clo = n.keys[i-1]
		}
		if i < len(n.keys) {
			chi = n.keys[i]
		}
		c.walk(child, clo, chi, depth+1)
	}
}

func (c *checker) checkOverflow(leaf uint32, i int, v value) {
	total, pg, hops := uint32(0), v.first, 0
	for pg != 0 {
		if hops++; hops > 1<<20 {
			c.problem("leaf %d entry %d: overflow chain does not end", leaf, i)
			return
		}
		c.mark(pg)
		b, err := c.t.v.Read(pg)
		if err != nil {
			c.problem("overflow page %d: %v", pg, err)
			return
		}
		if b[0] != typeOverflow {
			c.problem("page %d: expected an overflow page", pg)
			return
		}
		total += uint32(binary.BigEndian.Uint16(b[5:]))
		pg = binary.BigEndian.Uint32(b[1:])
	}
	if total != v.total {
		c.problem("leaf %d entry %d: overflow chain holds %d bytes, expected %d", leaf, i, total, v.total)
	}
}
