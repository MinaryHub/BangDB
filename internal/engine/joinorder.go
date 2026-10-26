package engine

import (
	"bytes"
	"math"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
)

// ---- ANALYZE ----

// analyze collects, for every index of the table(s), the average number of
// entries per distinct value of each key prefix (Index.Stats), which the
// planner uses to estimate how many rows an index lookup returns.
func (x *execCtx) analyze(st *sql.Analyze) error {
	var tables []*Table
	if st.Table != "" {
		t, err := x.table(st.Table)
		if err != nil {
			return err
		}
		tables = append(tables, t)
	} else {
		names := x.cat.TableNames()
		sort.Strings(names)
		for _, n := range names {
			tables = append(tables, x.cat.table(n))
		}
	}
	x.mutableCatalog()
	for _, t := range tables {
		t = x.cat.table(t.Name)
		for _, ix := range t.indexes {
			stats, err := x.indexStats(ix)
			if err != nil {
				return err
			}
			ix.Stats = stats
			if err := saveEntry(x.v, indexKey(ix.Name), ix); err != nil {
				return err
			}
		}
	}
	return nil
}

func (x *execCtx) indexStats(ix *Index) ([]float64, error) {
	n := len(ix.cols)
	distinct := make([]int64, n)
	var total int64
	var prev []byte
	ends := make([]int, n)
	prevEnds := make([]int, n)
	it := x.index(ix).Seek(nil)
	for ; it.Valid(); it.Next() {
		if err := x.tick(); err != nil {
			return nil, err
		}
		k := it.Key()
		total++
		keyPrefixEnds(k, ends)
		for i := 0; i < n; i++ {
			if prev == nil || !bytes.Equal(k[:ends[i]], prev[:prevEnds[i]]) {
				// This and every longer prefix are new.
				for j := i; j < n; j++ {
					distinct[j]++
				}
				break
			}
		}
		prev = append(prev[:0], k...)
		copy(prevEnds, ends)
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	stats := make([]float64, n)
	for i := range stats {
		stats[i] = float64(total) / float64(max(distinct[i], 1))
	}
	return stats, nil
}

// keyPrefixEnds sets ends[i] to the end of the i+1th value encoded in key
// (see value.AppendKey). A truncated long key counts as one value.
func keyPrefixEnds(key []byte, ends []int) {
	p := 0
	for i := range ends {
		p = keyValueEnd(key, p)
		ends[i] = p
	}
}

func keyValueEnd(k []byte, p int) int {
	if p >= len(k) {
		return len(k)
	}
	switch k[p] {
	case 0x01: // NULL
		return p + 1
	case 0x10, 0x11: // integer, real
		return min(p+9, len(k))
	case 0x20, 0x30: // text, blob: escaped, terminated by 00 01
		for i := p + 1; i+1 < len(k); i++ {
			if k[i] == 0 {
				if k[i+1] == 1 {
					return i + 2
				}
				i++ // 00 FF
			}
		}
	}
	return len(k)
}

// ---- join order ----

// joinOrder returns a better order for the levels of an inner join (a
// permutation of their indexes), or nil to keep the FROM order. It
// estimates the cost of nested-loop orders from table sizes and index
// statistics and builds an order greedily, taking at each step the table
// that is cheapest to join next; the result is used only if it is clearly
// cheaper than the FROM order, so hand-ordered queries keep their plan.
func (x *execCtx) joinOrder(sc *scope, levels []*level, conds []sql.Expr, c *compiler) []int {
	n := len(levels)
	if n < 2 || n > 8 {
		return nil
	}
	sizes := make([]float64, n)
	for i, l := range levels {
		if l.virtual != nil {
			sizes[i] = float64(len(l.virtual))
		} else if est, err := x.rows(l.t).EstimateRows(); err == nil {
			sizes[i] = float64(max(est, 1))
		} else {
			return nil
		}
	}
	orig := make([]int, len(sc.cols))
	for i, col := range sc.cols {
		orig[i] = col.level
	}
	defer func() {
		for i := range sc.cols {
			sc.cols[i].level = orig[i]
		}
	}()
	// cost of an order prefix: placing order[k] at position k after
	// order[:k]; returns the cost of that level and its output rows per
	// outer row.
	step := func(order []int) (cost, rows float64, ok bool) {
		pos := make([]int, n)
		for i := range pos {
			pos[i] = n // unplaced
		}
		for k, li := range order {
			pos[li] = k
		}
		for i := range sc.cols {
			sc.cols[i].level = pos[orig[i]]
		}
		k := len(order) - 1
		li := order[k]
		var mine []sql.Expr
		for _, e := range conds {
			if lvl, err := maxLevel(e, sc); err == nil && lvl == k {
				mine = append(mine, e)
			}
		}
		l := levels[li]
		var acc *access
		var err error
		if l.virtual != nil {
			acc, err = hashAccess(sc, l, k, mine, c)
			if acc == nil && err == nil {
				acc = &access{kind: accFull}
			}
		} else {
			acc, err = chooseAccess(sc, l, k, mine, c)
		}
		if err != nil {
			return 0, 0, false
		}
		r := estimateRows(acc, l, sizes[li])
		// Conditions the access path does not use filter further.
		for range mine {
			r *= 0.7
		}
		r = max(r, 0.1)
		probe := r
		switch acc.kind {
		case accIndex, accRowidEq, accRowidIn:
			probe += math.Log2(sizes[li] + 1)
		case accHash:
			probe = 1 + r // plus building the hash once (added by the caller)
		case accFull, accUnion:
			probe = sizes[li]
		}
		return probe, r, true
	}
	total := func(order []int) (float64, bool) {
		var cost, outer float64 = 0, 1
		for k := range order {
			c, r, ok := step(order[:k+1])
			if !ok {
				return 0, false
			}
			cost += outer * c
			outer *= r
		}
		return cost, true
	}

	fromOrder := make([]int, n)
	for i := range fromOrder {
		fromOrder[i] = i
	}
	fromCost, ok := total(fromOrder)
	if !ok {
		return nil
	}
	var order []int
	used := make([]bool, n)
	outer := 1.0
	for len(order) < n {
		best, bestCost, bestRows := -1, 0.0, 0.0
		for li := 0; li < n; li++ {
			if used[li] {
				continue
			}
			c, r, ok := step(append(order, li))
			if !ok {
				return nil
			}
			c *= outer
			if best < 0 || c < bestCost || c == bestCost && r < bestRows {
				best, bestCost, bestRows = li, c, r
			}
		}
		order = append(order, best)
		used[best] = true
		outer *= bestRows
	}
	greedyCost, ok := total(order)
	if !ok || greedyCost*2 > fromCost {
		return nil
	}
	return order
}

// estimateRows estimates how many rows one use of acc returns from a
// table of size rows.
func estimateRows(acc *access, l *level, size float64) float64 {
	switch acc.kind {
	case accRowidEq:
		return 1
	case accRowidIn:
		return float64(len(acc.eq))
	case accRowidRange, accTimeRange:
		return rangeShare(acc, size)
	case accHash:
		return max(size/10, 1)
	case accUnion:
		return size / 4
	case accIndex:
		ix := acc.ix
		eqN := len(acc.prefix)
		perKey := func(k int) float64 { // rows per distinct value of k columns
			if k == 0 {
				return size
			}
			if k <= len(ix.Stats) {
				return max(ix.Stats[k-1], 1)
			}
			if k == len(ix.cols) && ix.Unique {
				return 1
			}
			return max(size/math.Pow(10, float64(k)), 1)
		}
		switch {
		case acc.in != nil:
			return perKey(eqN+1) * float64(len(acc.in))
		case acc.lo != nil || acc.hi != nil:
			return max(perKey(eqN)/4, 1)
		}
		return perKey(eqN)
	}
	return size
}

func rangeShare(acc *access, size float64) float64 {
	if acc.lo != nil && acc.hi != nil {
		return max(size/8, 1)
	}
	return max(size/4, 1)
}

// reorderJoin returns the join order to use instead of the FROM order, or
// nil. Joins with LEFT JOIN keep their order, as do queries whose ORDER BY
// with LIMIT may be served by scanning the first table in order
// (keepJoinOrder).
func (x *execCtx) reorderJoin(sc *scope, levels []*level, lefts []bool, ons []sql.Expr, where sql.Expr, c *compiler) []int {
	if len(levels) < 2 || x.keepJoinOrder {
		return nil
	}
	for _, left := range lefts {
		if left {
			return nil
		}
	}
	var conds []sql.Expr
	for _, e := range append(append([]sql.Expr(nil), ons...), where) {
		for _, cj := range conjuncts(e) {
			if containsSubquery(cj) || hasAggregate(cj) {
				continue
			}
			if _, err := maxLevel(cj, sc); err != nil {
				return nil // reported by the regular path
			}
			conds = append(conds, cj)
		}
	}
	return x.joinOrder(sc, levels, conds, c)
}

// Index returns the index with the given name, or nil.
func (c *Catalog) Index(name string) *Index { return c.index(name) }

// coveringIndexes marks index access paths that can be served from the
// index entries alone: every column the query references at that level
// is an indexed column (stored in the key with its own value, so not
// under a collation) or the rowid.
func (p *selectPlan) coveringIndexes() {
	for _, l := range p.levels {
		acc := l.acc
		if l.virtual != nil || acc == nil || acc.kind != accIndex || acc.covering {
			continue
		}
		ok := true
		for col := range l.t.Columns {
			slot := l.offset + col
			if slot >= len(p.sc.used) || !p.sc.used[slot] || col == l.t.RowidCol {
				continue
			}
			found := false
			for i, c := range acc.ix.cols {
				if c == col && acc.ix.colls[i] == "" {
					found = true
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			acc.covering = true
			acc.desc = strings.Replace(acc.desc, "USING INDEX", "USING COVERING INDEX", 1)
		}
	}
}
