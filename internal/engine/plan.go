package engine

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// accessKind is how rows of one table are located.
type accessKind int

const (
	accFull accessKind = iota
	accRowidEq
	accRowidIn
	accRowidRange
	accIndex
	accUnion // OR of indexable conditions: union of several access paths
	accHash  // equi-join without index: hash the table once, probe per row
)

// access is the chosen access path for one table.
//
// Index paths use equality on the first len(prefix) index columns, then
// optionally an IN list or a range on the next column.
type access struct {
	kind   accessKind
	colTyp value.Type // rowid paths: Integer; index paths: type of the IN/range column
	eq     []evalFn   // rowid: = (1 element) or IN list
	lo, hi evalFn
	loInc  bool
	hiInc  bool

	ix       *Index
	prefix   []evalFn
	prefTyp  []value.Type
	prefColl []string // collations of the prefix columns (search values are folded)
	coll     string   // collation of the IN/range column
	in       []evalFn
	desc     string

	reverse bool // visit rows in descending key order (for ORDER BY ... DESC)

	subs    []*access // accUnion
	hashCol int       // accHash: hashed column (-1 = rowid)
	hash    map[string][]hashRow
}

type hashRow struct {
	rowid int64
	row   []any
}

// level is one table in a FROM/JOIN chain.
type level struct {
	t       *Table
	alias   string
	offset  int // first slot of this table in env.row
	left    bool
	acc     *access
	filters []evalFn // conditions checked once this level's row is bound
	virtual [][]any  // rows of a derived table (FROM (SELECT ...))
}

func (l *level) rowidSlot() int { return l.offset + len(l.t.Columns) }

// conjuncts splits an AND tree into its parts.
func conjuncts(e sql.Expr) []sql.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*sql.Binary); ok && b.Op == "AND" {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	if bt, ok := e.(*sql.Between); ok && !bt.Not {
		return []sql.Expr{&sql.Binary{Op: ">=", L: bt.X, R: bt.Lo}, &sql.Binary{Op: "<=", L: bt.X, R: bt.Hi}}
	}
	return []sql.Expr{e}
}

// maxLevel returns the highest FROM level referenced by e (-1 if none).
func maxLevel(e sql.Expr, sc *scope) (int, error) {
	lvl := -1
	var err error
	walk(e, func(n sql.Expr) {
		if c, ok := n.(*sql.ColumnRef); ok && err == nil {
			var i int
			if i, err = sc.resolve(c.Table, c.Name); err == nil {
				lvl = max(lvl, sc.cols[i].level)
			} else if sc.isOuter(c.Table, c.Name) {
				err = nil // a constant for this query
			}
		}
	})
	return lvl, err
}

type candidate struct {
	col   int // column index in table, or -1 for rowid
	op    string
	other sql.Expr
	list  []sql.Expr
}

// columnOf returns the table column referenced by e if it belongs to level l.
func columnOf(e sql.Expr, sc *scope, l *level, li int) (int, bool) {
	c, ok := e.(*sql.ColumnRef)
	if !ok {
		return 0, false
	}
	i, err := sc.resolve(c.Table, c.Name)
	if err != nil || sc.cols[i].level != li {
		return 0, false
	}
	pos := i - l.offset
	if pos == len(l.t.Columns) || pos == l.t.RowidCol {
		return -1, true // rowid
	}
	return pos, true
}

var flipOp = map[string]string{"=": "=", "<": ">", "<=": ">=", ">": "<", ">=": "<="}

// chooseAccess picks the cheapest access path for level li given the
// conditions that may be used to locate its rows.
func chooseAccess(sc *scope, l *level, li int, conds []sql.Expr, c *compiler) (*access, error) {
	byCol := map[int][]candidate{}
	for _, e := range conds {
		switch x := e.(type) {
		case *sql.Binary:
			op, ok := flipOp[x.Op]
			if !ok {
				continue
			}
			// An index (or the rowid) can only serve a comparison made with
			// the column's own collation.
			coll := c.comparisonCollation(x.L, x.R)
			if coll == "BINARY" {
				coll = ""
			}
			colColl := func(col int) string {
				if col < 0 {
					return ""
				}
				return l.t.Columns[col].Collate
			}
			if col, ok := columnOf(x.L, sc, l, li); ok && refsBelow(x.R, sc, li) && coll == colColl(col) {
				byCol[col] = append(byCol[col], candidate{col: col, op: x.Op, other: x.R})
			} else if col, ok := columnOf(x.R, sc, l, li); ok && refsBelow(x.L, sc, li) && coll == colColl(col) {
				byCol[col] = append(byCol[col], candidate{col: col, op: op, other: x.L})
			}
		case *sql.In:
			if x.Not {
				continue
			}
			if col, ok := columnOf(x.X, sc, l, li); ok {
				all := true
				for _, a := range x.List {
					all = all && refsBelow(a, sc, li)
				}
				if all {
					byCol[col] = append(byCol[col], candidate{col: col, op: "IN", list: x.List})
				}
			}
		}
	}
	find := func(col int, ops ...string) *candidate {
		for i, cd := range byCol[col] {
			for _, op := range ops {
				if cd.op == op {
					return &byCol[col][i]
				}
			}
		}
		return nil
	}
	compileList := func(list []sql.Expr) ([]evalFn, error) {
		var out []evalFn
		for _, e := range list {
			f, err := c.compile(e)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, nil
	}
	name := l.t.Name

	// Equality on the rowid always wins.
	if cd := find(-1, "="); cd != nil {
		fns, err := compileList([]sql.Expr{cd.other})
		if err != nil {
			return nil, err
		}
		return &access{kind: accRowidEq, eq: fns, colTyp: value.Integer,
			desc: fmt.Sprintf("SEARCH %s USING ROWID (rowid=?)", name)}, nil
	}

	// Score every index by how much of its key the conditions pin down.
	type plan struct {
		ix    *Index
		eqN   int
		in    *candidate
		rng   bool
		score int
	}
	var best *plan
	for _, ix := range l.t.indexes {
		p := &plan{ix: ix}
		for _, col := range ix.cols {
			if l.t.Columns[col].Type == value.Any || find(col, "=") == nil {
				break
			}
			p.eqN++
		}
		if p.eqN < len(ix.cols) && l.t.Columns[ix.cols[p.eqN]].Type != value.Any {
			next := ix.cols[p.eqN]
			p.in = find(next, "IN")
			p.rng = p.in == nil && find(next, "<", "<=", ">", ">=") != nil
		}
		switch {
		case p.eqN == len(ix.cols) && ix.Unique:
			p.score = 900
		case p.eqN > 0:
			p.score = 100 + 10*p.eqN
			if p.in != nil {
				p.score += 5
			} else if p.rng {
				p.score += 3
			}
			if ix.Unique {
				p.score++
			}
		case p.in != nil:
			p.score = 70
		case p.rng:
			p.score = 40
		default:
			continue
		}
		if best == nil || p.score > best.score {
			best = p
		}
	}
	rowidIn, rowidRange := find(-1, "IN"), find(-1, "<", "<=", ">", ">=") != nil
	switch {
	case best != nil && best.score >= 100:
	case rowidIn != nil:
		fns, err := compileList(rowidIn.list)
		if err != nil {
			return nil, err
		}
		return &access{kind: accRowidIn, eq: fns, colTyp: value.Integer,
			desc: fmt.Sprintf("SEARCH %s USING ROWID (rowid IN (%d values))", name, len(fns))}, nil
	case best != nil && best.score >= 70:
	case rowidRange:
		a := &access{kind: accRowidRange, colTyp: value.Integer}
		parts, err := setRange(a, byCol[-1], "rowid", c)
		if err != nil {
			return nil, err
		}
		a.desc = fmt.Sprintf("SEARCH %s USING ROWID RANGE (%s)", name, strings.Join(parts, " AND "))
		return a, nil
	case best == nil:
		if a, err := orAccess(sc, l, li, conds, c); a != nil || err != nil {
			return a, err
		}
		if a, err := hashAccess(sc, l, li, conds, c); a != nil || err != nil {
			return a, err
		}
		return &access{kind: accFull, desc: "SCAN " + name}, nil
	}

	ix := best.ix
	a := &access{kind: accIndex, ix: ix}
	var parts []string
	for _, col := range ix.cols[:best.eqN] {
		f, err := c.compile(find(col, "=").other)
		if err != nil {
			return nil, err
		}
		a.prefix = append(a.prefix, f)
		a.prefTyp = append(a.prefTyp, l.t.Columns[col].Type)
		a.prefColl = append(a.prefColl, l.t.Columns[col].Collate)
		parts = append(parts, l.t.Columns[col].Name+"=?")
	}
	if best.eqN < len(ix.cols) {
		next := ix.cols[best.eqN]
		a.colTyp = l.t.Columns[next].Type
		a.coll = l.t.Columns[next].Collate
		colName := l.t.Columns[next].Name
		if best.in != nil {
			fns, err := compileList(best.in.list)
			if err != nil {
				return nil, err
			}
			a.in = fns
			parts = append(parts, fmt.Sprintf("%s IN (%d values)", colName, len(fns)))
		} else if best.rng {
			rp, err := setRange(a, byCol[next], colName, c)
			if err != nil {
				return nil, err
			}
			parts = append(parts, rp...)
		}
	}
	kind := ""
	if best.rng {
		kind = " RANGE"
	}
	a.desc = fmt.Sprintf("SEARCH %s USING INDEX %s%s (%s)", name, ix.Name, kind, strings.Join(parts, " AND "))
	return a, nil
}

// setRange fills a's bounds from the range candidates of one column.
func setRange(a *access, cands []candidate, colName string, c *compiler) ([]string, error) {
	var lo, hi sql.Expr
	for _, cd := range cands {
		switch cd.op {
		case ">", ">=":
			if lo == nil {
				lo, a.loInc = cd.other, cd.op == ">="
			}
		case "<", "<=":
			if hi == nil {
				hi, a.hiInc = cd.other, cd.op == "<="
			}
		}
	}
	var parts []string
	var err error
	if lo != nil {
		if a.lo, err = c.compile(lo); err != nil {
			return nil, err
		}
		parts = append(parts, colName+map[bool]string{true: ">=", false: ">"}[a.loInc]+"?")
	}
	if hi != nil {
		if a.hi, err = c.compile(hi); err != nil {
			return nil, err
		}
		parts = append(parts, colName+map[bool]string{true: "<=", false: "<"}[a.hiInc]+"?")
	}
	return parts, nil
}

// disjuncts splits an OR tree into its parts.
func disjuncts(e sql.Expr) []sql.Expr {
	if b, ok := e.(*sql.Binary); ok && b.Op == "OR" {
		return append(disjuncts(b.L), disjuncts(b.R)...)
	}
	return []sql.Expr{e}
}

// orAccess handles "a = 1 OR b = 2 ...": if every disjunct of an OR
// condition can be located through the rowid or an index, the rows are the
// union of those lookups. The OR itself is still checked as a filter.
func orAccess(sc *scope, l *level, li int, conds []sql.Expr, c *compiler) (*access, error) {
	for _, e := range conds {
		parts := disjuncts(e)
		if len(parts) < 2 {
			continue
		}
		a := &access{kind: accUnion}
		var descs []string
		for _, d := range parts {
			sub, err := chooseAccess(sc, l, li, conjuncts(d), c)
			if err != nil {
				return nil, err
			}
			if sub.kind == accFull || sub.kind == accHash {
				a = nil
				break
			}
			a.subs = append(a.subs, sub)
			descs = append(descs, sub.desc)
		}
		if a != nil {
			a.desc = fmt.Sprintf("SEARCH %s USING UNION (%s)", l.t.Name, strings.Join(descs, "; "))
			return a, nil
		}
	}
	return nil, nil
}

// hashAccess picks a hash join for an inner join level that is compared for
// equality with earlier tables but has no usable index: the table is read
// once into a hash table instead of being scanned for every outer row.
func hashAccess(sc *scope, l *level, li int, conds []sql.Expr, c *compiler) (*access, error) {
	if li == 0 {
		return nil, nil
	}
	for _, e := range conds {
		b, ok := e.(*sql.Binary)
		if !ok || b.Op != "=" {
			continue
		}
		col, other := 0, sql.Expr(nil)
		if cl, ok := columnOf(b.L, sc, l, li); ok && refsBelow(b.R, sc, li) {
			col, other = cl, b.R
		} else if cr, ok := columnOf(b.R, sc, l, li); ok && refsBelow(b.L, sc, li) {
			col, other = cr, b.L
		} else {
			continue
		}
		if col >= 0 && l.t.Columns[col].Collate != "" || c.comparisonCollation(b.L, b.R) != "" {
			continue // hash keys use binary equality
		}
		f, err := c.compile(other)
		if err != nil {
			return nil, err
		}
		name := "rowid"
		if col >= 0 {
			name = l.t.Columns[col].Name
		}
		return &access{kind: accHash, hashCol: col, eq: []evalFn{f},
			desc: fmt.Sprintf("SCAN %s USING HASH (%s=?)", l.t.Name, name)}, nil
	}
	return nil, nil
}

func describeOp(col, op string, n int) string {
	if op == "IN" {
		return fmt.Sprintf("%s IN (%d values)", col, n)
	}
	return col + "=?"
}

// refsBelow reports whether e only references tables before level li.
func refsBelow(e sql.Expr, sc *scope, li int) bool {
	ok := true
	walk(e, func(n sql.Expr) {
		switch x := n.(type) {
		case *sql.ColumnRef:
			i, err := sc.resolve(x.Table, x.Name)
			if err != nil && !sc.isOuter(x.Table, x.Name) || err == nil && sc.cols[i].level >= li {
				ok = false
			}
		case *sql.Func:
			if isAggregateCall(x) {
				ok = false
			}
		case *sql.Subquery, *sql.Exists:
			ok = false
		case *sql.In:
			if x.Sub != nil {
				ok = false
			}
		}
	})
	return ok
}

// keyValue converts a search value to the key column's type. ok=false means
// no stored value can compare equal / in range using the index ordering, so
// the bound must be handled differently by the caller.
func keyValue(v any, t value.Type) (any, bool) {
	switch t {
	case value.Integer:
		switch x := v.(type) {
		case int64:
			return x, true
		case float64:
			if x == math.Trunc(x) && x >= math.MinInt64 && x < math.MaxInt64 {
				return int64(x), true
			}
		}
	case value.Real:
		switch x := v.(type) {
		case int64:
			return float64(x), true
		case float64:
			return x, true
		}
	case value.Text:
		if s, ok := v.(string); ok {
			return s, true
		}
	case value.Blob:
		if b, ok := v.([]byte); ok {
			return b, true
		}
	}
	return nil, false
}

// rangeBound converts a range bound to the key type. For an INTEGER key and a
// fractional bound it rounds toward the inside of the range. ok=false means
// the bound cannot be used and the range must stay open on that side.
func rangeBound(v any, t value.Type, lower bool, inc bool) (any, bool, bool) {
	if f, isF := v.(float64); isF && t == value.Integer && f != math.Trunc(f) && !math.IsNaN(f) {
		if lower {
			f = math.Ceil(f)
		} else {
			f = math.Floor(f)
		}
		if f < math.MinInt64 || f >= math.MaxInt64 {
			return nil, false, inc
		}
		return int64(f), true, true
	}
	k, ok := keyValue(v, t)
	return k, ok, inc
}

// scan feeds every row located by acc to fn. fn returns false to stop.
func (x *execCtx) scan(t *Table, acc *access, e *env, visit func(rowid int64, row []any) (bool, error)) error {
	fn := func(rowid int64, row []any) (bool, error) {
		if err := x.tick(); err != nil {
			return false, err
		}
		return visit(rowid, row)
	}
	tree := btree.Open(x.v, t.Root)
	emit := func(rowid int64) (bool, error) {
		row, ok, err := x.getRow(t, rowid)
		if err != nil || !ok {
			return err == nil, err
		}
		return fn(rowid, row)
	}
	switch acc.kind {
	case accRowidEq, accRowidIn:
		seen := map[int64]bool{}
		for _, f := range acc.eq {
			v, err := f(e)
			if err != nil {
				return err
			}
			k, ok := keyValue(v, value.Integer)
			if !ok || seen[k.(int64)] {
				continue // NULL or incomparable type: cannot be equal
			}
			seen[k.(int64)] = true
			if cont, err := emit(k.(int64)); err != nil || !cont {
				return err
			}
		}
		return nil
	case accIndex:
		return x.scanIndex(acc, e, emit)
	case accUnion:
		return x.scanUnion(t, acc, e, visit)
	case accRowidRange:
		start := []byte(nil)
		var stop []byte
		stopInc := false
		if acc.lo != nil {
			v, err := acc.lo(e)
			if err != nil {
				return err
			}
			if v == nil {
				return nil
			}
			if k, ok, inc := rangeBound(v, value.Integer, true, acc.loInc); ok {
				id := k.(int64)
				if !inc {
					if id == math.MaxInt64 {
						return nil
					}
					id++
				}
				start = value.EncodeRowID(id)
			}
		}
		if acc.hi != nil {
			v, err := acc.hi(e)
			if err != nil {
				return err
			}
			if v == nil {
				return nil
			}
			if k, ok, inc := rangeBound(v, value.Integer, false, acc.hiInc); ok {
				stop, stopInc = value.EncodeRowID(k.(int64)), inc
			}
		}
		if acc.reverse {
			return x.scanRowsReverse(t, stop, stopInc, start, fn)
		}
		c := tree.Seek(start)
		for ; c.Valid(); c.Next() {
			if stop != nil {
				cmp := bytes.Compare(c.Key(), stop)
				if cmp > 0 || cmp == 0 && !stopInc {
					break
				}
			}
			data, err := c.Value()
			if err != nil {
				return err
			}
			row, err := x.decodeRow(t, data)
			if err != nil {
				return err
			}
			if cont, err := fn(value.DecodeRowID(c.Key()), row); err != nil || !cont {
				return err
			}
		}
		return c.Err()
	}
	if acc.reverse {
		return x.scanRowsReverse(t, nil, true, nil, fn)
	}
	c := tree.Seek(nil)
	for ; c.Valid(); c.Next() {
		data, err := c.Value()
		if err != nil {
			return err
		}
		row, err := x.decodeRow(t, data)
		if err != nil {
			return err
		}
		if cont, err := fn(value.DecodeRowID(c.Key()), row); err != nil || !cont {
			return err
		}
	}
	return c.Err()
}

// scanHash probes the level's hash table (built on first use).
func (x *execCtx) scanHash(l *level, e *env, visit func(rowid int64, row []any) (bool, error)) error {
	acc := l.acc
	if acc.hash == nil {
		acc.hash = map[string][]hashRow{}
		add := func(rowid int64, row []any) (bool, error) {
			k := any(rowid)
			if acc.hashCol >= 0 {
				k = row[acc.hashCol]
			}
			if k != nil {
				key := string(numKey(k))
				acc.hash[key] = append(acc.hash[key], hashRow{rowid, row})
			}
			return true, nil
		}
		var err error
		if l.virtual != nil {
			for i, row := range l.virtual {
				if _, err = add(int64(i+1), row); err != nil {
					break
				}
			}
		} else {
			err = x.scan(l.t, &access{kind: accFull}, e, add)
		}
		if err != nil {
			acc.hash = nil
			return err
		}
	}
	v, err := acc.eq[0](e)
	if err != nil || v == nil {
		return err
	}
	for _, hr := range acc.hash[string(numKey(v))] {
		if cont, err := visit(hr.rowid, hr.row); err != nil || !cont {
			return err
		}
	}
	return nil
}

// scanUnion visits the rows found by any of several access paths, once
// each, in rowid order.
func (x *execCtx) scanUnion(t *Table, acc *access, e *env, visit func(rowid int64, row []any) (bool, error)) error {
	rows := map[int64][]any{}
	for _, sub := range acc.subs {
		err := x.scan(t, sub, e, func(rowid int64, row []any) (bool, error) {
			rows[rowid] = row
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if cont, err := visit(id, rows[id]); err != nil || !cont {
			return err
		}
	}
	return nil
}

// scanRowsReverse visits table rows in descending rowid order, from hi
// (nil = the end; exclusive unless hiInc) down to lo (nil = the start,
// inclusive).
func (x *execCtx) scanRowsReverse(t *Table, hi []byte, hiInc bool, lo []byte, fn func(int64, []any) (bool, error)) error {
	c := btree.Open(x.v, t.Root).SeekReverse(hi)
	for ; c.Valid(); c.Prev() {
		if hi != nil && !hiInc && bytes.Equal(c.Key(), hi) {
			continue
		}
		if lo != nil && bytes.Compare(c.Key(), lo) < 0 {
			break
		}
		data, err := c.Value()
		if err != nil {
			return err
		}
		row, err := x.decodeRow(t, data)
		if err != nil {
			return err
		}
		if cont, err := fn(value.DecodeRowID(c.Key()), row); err != nil || !cont {
			return err
		}
	}
	return c.Err()
}

// scanLevel visits the rows of one FROM level.
func (x *execCtx) scanLevel(l *level, e *env, visit func(rowid int64, row []any) (bool, error)) error {
	if l.acc.kind == accHash {
		return x.scanHash(l, e, visit)
	}
	if l.virtual == nil {
		return x.scan(l.t, l.acc, e, visit)
	}
	for i, row := range l.virtual {
		if err := x.tick(); err != nil {
			return err
		}
		if cont, err := visit(int64(i+1), row); err != nil || !cont {
			return err
		}
	}
	return nil
}

// scanIndex visits the rows located by an index access path: equality on
// the leading columns, then an IN list or a range on the next column. Rows
// are emitted while the index is traversed, so LIMIT can stop early (DML
// collects its targets before modifying anything).
func (x *execCtx) scanIndex(acc *access, e *env, emit func(int64) (bool, error)) error {
	var prefix []byte
	for i, f := range acc.prefix {
		v, err := f(e)
		if err != nil {
			return err
		}
		k, ok := keyValue(v, acc.prefTyp[i])
		if !ok {
			return nil // NULL or incomparable: no row can be equal
		}
		prefix = value.AppendKey(prefix, foldValue(k, acc.prefColl[i]))
	}
	if len(acc.prefix) == len(acc.ix.cols) {
		prefix, _ = truncKey(prefix) // the whole key: find it by hash
	} else {
		prefix, _ = groupKey(prefix)
	}
	tree := btree.Open(x.v, acc.ix.Root)
	// collect returns the row ids of entries from start on that keep the
	// prefix, as long as accept says so.
	collect := func(start, prefix []byte, accept func(key []byte) (keep, more bool)) error {
		out := x.keyOrder(acc.ix, false, emit)
		c := tree.Seek(start)
		for ; c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
			if err := x.tick(); err != nil {
				return err
			}
			keep, more := true, true
			if accept != nil {
				keep, more = accept(c.Key())
			}
			if !more {
				break
			}
			if keep {
				if cont, err := out.push(c.Key()); err != nil || !cont {
					return err
				}
			}
		}
		if err := c.Err(); err != nil {
			return err
		}
		_, err := out.flush()
		return err
	}
	clone := func(b []byte) []byte { return append([]byte(nil), b...) }
	// bound appends a range bound to the prefix; trunc reports that it was
	// cut, so that entries sharing it must not be excluded by the scan (the
	// level's filters check the real condition).
	bound := func(k any) (b []byte, trunc bool) {
		return groupKey(value.AppendKey(clone(prefix), foldValue(k, acc.coll)))
	}

	if acc.in != nil {
		seen := map[string]bool{}
		for _, f := range acc.in {
			v, err := f(e)
			if err != nil {
				return err
			}
			k, ok := keyValue(v, acc.colTyp)
			if !ok {
				continue
			}
			p, _ := bound(k)
			if len(acc.prefix)+1 == len(acc.ix.cols) {
				p, _ = truncKey(value.AppendKey(clone(prefix), foldValue(k, acc.coll)))
			}
			if seen[string(p)] {
				continue
			}
			seen[string(p)] = true
			if err := collect(p, p, nil); err != nil {
				return err
			}
		}
		return nil
	}
	if acc.reverse {
		return x.scanIndexReverse(acc, e, tree, prefix, emit)
	}
	if acc.lo == nil && acc.hi == nil {
		return collect(prefix, prefix, nil)
	}
	start, _ := groupKey(append(clone(prefix), value.MinNonNullKey...)) // NULLs never satisfy a range
	var lowPrefix, stop []byte
	stopInc := false
	if acc.lo != nil {
		v, err := acc.lo(e)
		if err != nil || v == nil {
			return err
		}
		if k, ok, inc := rangeBound(v, acc.colTyp, true, acc.loInc); ok {
			var trunc bool
			start, trunc = bound(k)
			if !inc && !trunc {
				lowPrefix = start
			}
		}
	}
	if acc.hi != nil {
		v, err := acc.hi(e)
		if err != nil || v == nil {
			return err
		}
		if k, ok, inc := rangeBound(v, acc.colTyp, false, acc.hiInc); ok {
			var trunc bool
			stop, trunc = bound(k)
			stopInc = inc || trunc
		}
	}
	return collect(start, prefix, func(key []byte) (bool, bool) {
		if lowPrefix != nil && bytes.HasPrefix(key, lowPrefix) {
			return false, true
		}
		if stop != nil {
			if bytes.HasPrefix(key, stop) {
				return stopInc, stopInc
			}
			if bytes.Compare(key, stop) > 0 {
				return false, false
			}
		}
		return true, true
	})
}

// maxSuffix sorts after every key that starts with a given prefix (value
// tags are < 0xFF and a rowid suffix is 8 bytes).
var maxSuffix = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

// scanIndexReverse is scanIndex in descending key order (no IN lists).
func (x *execCtx) scanIndexReverse(acc *access, e *env, tree *btree.Tree, prefix []byte, emit func(int64) (bool, error)) error {
	clone := func(b []byte) []byte { return append([]byte(nil), b...) }
	bound := func(k any) (b []byte, trunc bool) {
		return groupKey(value.AppendKey(clone(prefix), foldValue(k, acc.coll)))
	}
	upper := append(clone(prefix), maxSuffix...)
	var lower []byte // stop below this key
	lowerExcl := false
	if acc.lo != nil || acc.hi != nil {
		lower, _ = groupKey(append(clone(prefix), value.MinNonNullKey...)) // NULLs never satisfy a range
	}
	if acc.hi != nil {
		v, err := acc.hi(e)
		if err != nil || v == nil {
			return err
		}
		if k, ok, inc := rangeBound(v, acc.colTyp, false, acc.hiInc); ok {
			var trunc bool
			upper, trunc = bound(k)
			if inc || trunc {
				upper = append(upper, maxSuffix...)
			}
		}
	}
	if acc.lo != nil {
		v, err := acc.lo(e)
		if err != nil || v == nil {
			return err
		}
		if k, ok, inc := rangeBound(v, acc.colTyp, true, acc.loInc); ok {
			var trunc bool
			lower, trunc = bound(k)
			lowerExcl = !inc && !trunc
		}
	}
	out := x.keyOrder(acc.ix, true, emit)
	c := tree.SeekReverse(upper)
	for ; c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Prev() {
		if err := x.tick(); err != nil {
			return err
		}
		key := c.Key()
		if lower != nil && (bytes.Compare(key, lower) < 0 || lowerExcl && bytes.HasPrefix(key, lower)) {
			break
		}
		if cont, err := out.push(key); err != nil || !cont {
			return err
		}
	}
	if err := c.Err(); err != nil {
		return err
	}
	_, err := out.flush()
	return err
}

// buildLevels resolves the FROM clause into a scope and per-table levels,
// pushes WHERE conjuncts down to the earliest level where they can be
// evaluated and chooses an access path per level. Remaining conditions are
// returned as the final filter.
func (x *execCtx) buildLevels(from []sql.TableRef, lefts []bool, ons []sql.Expr, where sql.Expr) (*scope, []*level, []evalFn, error) {
	sc := &scope{outer: x.outer}
	var levels []*level
	seen := map[string]bool{}
	for i, ref := range from {
		var t *Table
		var virtual [][]any
		if ref.Sub != nil {
			// Derived table: run the subquery once and scan its rows.
			res, err := x.execSelect(ref.Sub)
			if err != nil {
				return nil, nil, nil, err
			}
			t = &Table{Name: ref.Alias, RowidCol: -1}
			for _, c := range res.Columns {
				t.Columns = append(t.Columns, Column{Name: c, Type: value.Any})
			}
			virtual = res.Rows
			if virtual == nil {
				virtual = [][]any{}
			}
		} else if v := x.cat.view(ref.Name); v != nil && x.cat.table(ref.Name) == nil {
			alias := ref.Alias
			if alias == "" {
				alias = v.Name
			}
			var err error
			if t, virtual, err = x.viewTable(v, alias); err != nil {
				return nil, nil, nil, err
			}
		} else {
			var err error
			if t, err = x.table(ref.Name); err != nil {
				return nil, nil, nil, err
			}
		}
		alias := ref.Alias
		if alias == "" {
			alias = t.Name
		}
		if seen[strings.ToLower(alias)] {
			return nil, nil, nil, fmt.Errorf("bangdb: ambiguous table name %s (use an alias)", alias)
		}
		seen[strings.ToLower(alias)] = true
		l := &level{t: t, alias: alias, offset: len(sc.cols), left: lefts[i], virtual: virtual}
		for _, c := range t.Columns {
			sc.cols = append(sc.cols, scopeCol{table: strings.ToLower(alias), name: strings.ToLower(c.Name), level: i, coll: c.Collate})
		}
		sc.cols = append(sc.cols, scopeCol{table: strings.ToLower(alias), name: "rowid", level: i, hidden: true})
		levels = append(levels, l)
	}
	c := &compiler{x: x, sc: sc}
	accessConds := make([][]sql.Expr, len(levels))
	var final []evalFn
	// ON conditions belong to their join level.
	for i, on := range ons {
		for _, cj := range conjuncts(on) {
			if _, err := maxLevel(cj, sc); err != nil {
				return nil, nil, nil, err
			}
			f, err := c.compile(cj)
			if err != nil {
				return nil, nil, nil, err
			}
			levels[i].filters = append(levels[i].filters, f)
			if !containsSubquery(cj) {
				accessConds[i] = append(accessConds[i], cj)
			}
		}
	}
	for _, cj := range conjuncts(where) {
		if hasAggregate(cj) {
			return nil, nil, nil, fmt.Errorf("bangdb: misuse of aggregate function in WHERE")
		}
		lvl, err := maxLevel(cj, sc)
		if err != nil {
			return nil, nil, nil, err
		}
		f, err := c.compile(cj)
		if err != nil {
			return nil, nil, nil, err
		}
		lvl = max(lvl, 0)
		// A WHERE condition on an outer-joined table must see the
		// NULL-extended row, so it is evaluated after the join. Without
		// any table (SELECT ... WHERE ...) everything is a final filter.
		// Conditions with subqueries may reference any table; they run last.
		if len(levels) == 0 || levels[lvl].left || containsSubquery(cj) {
			final = append(final, f)
			continue
		}
		levels[lvl].filters = append(levels[lvl].filters, f)
		accessConds[lvl] = append(accessConds[lvl], cj)
	}
	for i, l := range levels {
		if l.virtual != nil {
			l.acc = &access{kind: accFull, desc: "SCAN SUBQUERY " + l.alias}
			if a, err := hashAccess(sc, l, i, accessConds[i], c); err != nil {
				return nil, nil, nil, err
			} else if a != nil {
				a.desc = fmt.Sprintf("SCAN SUBQUERY %s USING HASH%s", l.alias, strings.TrimPrefix(a.desc, "SCAN "+l.t.Name+" USING HASH"))
				l.acc = a
			}
			continue
		}
		acc, err := chooseAccess(sc, l, i, accessConds[i], c)
		if err != nil {
			return nil, nil, nil, err
		}
		l.acc = acc
	}
	return sc, levels, final, nil
}

// runJoin enumerates all combined rows of levels (nested loop join) and calls
// emit for each one passing all filters. emit returns false to stop.
func (x *execCtx) runJoin(levels []*level, final []evalFn, e *env, emit func() (bool, error)) error {
	stopped := false
	var run func(li int) error
	run = func(li int) error {
		if li == len(levels) {
			ok, err := passes(final, e)
			if err != nil || !ok {
				return err
			}
			cont, err := emit()
			if !cont {
				stopped = true
			}
			return err
		}
		l := levels[li]
		matched := false
		err := x.scanLevel(l, e, func(rowid int64, row []any) (bool, error) {
			copy(e.row[l.offset:], row)
			e.row[l.rowidSlot()] = rowid
			ok, err := passes(l.filters, e)
			if err != nil || !ok {
				return err == nil, err
			}
			matched = true
			if err := run(li + 1); err != nil {
				return false, err
			}
			return !stopped, nil
		})
		if err != nil || stopped {
			return err
		}
		if !matched && l.left {
			for i := l.offset; i <= l.rowidSlot(); i++ {
				e.row[i] = nil
			}
			return run(li + 1)
		}
		return nil
	}
	return run(0)
}

func passes(filters []evalFn, e *env) (bool, error) {
	for _, f := range filters {
		v, err := f(e)
		if err != nil {
			return false, err
		}
		if ok, _ := value.Truthy(v); !ok {
			return false, nil
		}
	}
	return true, nil
}

// keyOrder passes index entries in scan order to emit. Entries whose key was
// truncated (see truncKey) are adjacent but ordered by hash; keyOrder
// buffers each such group and emits it sorted by the full key, so index
// order stays exact for ORDER BY.
type keyOrder struct {
	x     *execCtx
	ix    *Index
	desc  bool
	emit  func(int64) (bool, error)
	part  []byte  // truncated key part of the buffered group
	group []int64 // its row ids
}

func (x *execCtx) keyOrder(ix *Index, desc bool, emit func(int64) (bool, error)) *keyOrder {
	return &keyOrder{x: x, ix: ix, desc: desc, emit: emit}
}

func (o *keyOrder) push(key []byte) (bool, error) {
	part, id := key[:len(key)-8], value.DecodeRowID(key[len(key)-8:])
	if len(o.group) > 0 && isTruncated(part) && bytes.Equal(truncGroup(part), o.part) {
		o.group = append(o.group, id)
		return true, nil
	}
	if cont, err := o.flush(); err != nil || !cont {
		return cont, err
	}
	if isTruncated(part) {
		o.part = append(o.part[:0], truncGroup(part)...)
		o.group = append(o.group, id)
		return true, nil
	}
	return o.emit(id)
}

func (o *keyOrder) flush() (bool, error) {
	group := o.group
	o.group = o.group[:0]
	if len(group) <= 1 {
		if len(group) == 1 {
			return o.emit(group[0])
		}
		return true, nil
	}
	t := o.x.cat.table(o.ix.Table)
	type entry struct {
		key []byte
		id  int64
	}
	entries := make([]entry, 0, len(group))
	for _, id := range group {
		row, ok, err := o.x.getRow(t, id)
		if err != nil {
			return false, err
		}
		if ok {
			entries = append(entries, entry{o.ix.fullKey(row), id})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		c := bytes.Compare(entries[i].key, entries[j].key)
		if o.desc {
			return c > 0
		}
		return c < 0
	})
	for _, e := range entries {
		if cont, err := o.emit(e.id); err != nil || !cont {
			return cont, err
		}
	}
	return true, nil
}
