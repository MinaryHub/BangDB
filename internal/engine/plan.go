package engine

import (
	"bytes"
	"fmt"
	"math"
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
	accIndexEq
	accIndexIn
	accIndexRange
)

// access is the chosen access path for one table.
type access struct {
	kind   accessKind
	ix     *Index     // for index paths
	colTyp value.Type // type of the key column
	eq     []evalFn   // eq (1 element) or IN list
	lo, hi evalFn
	loInc  bool
	hiInc  bool
	desc   string
}

// level is one table in a FROM/JOIN chain.
type level struct {
	t       *Table
	alias   string
	offset  int // first slot of this table in env.row
	left    bool
	acc     *access
	filters []evalFn // conditions checked once this level's row is bound
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
	var cands []candidate
	for _, e := range conds {
		switch x := e.(type) {
		case *sql.Binary:
			op, ok := flipOp[x.Op]
			if !ok {
				continue
			}
			if col, ok := columnOf(x.L, sc, l, li); ok && refsBelow(x.R, sc, li) {
				cands = append(cands, candidate{col: col, op: x.Op, other: x.R})
			} else if col, ok := columnOf(x.R, sc, l, li); ok && refsBelow(x.L, sc, li) {
				cands = append(cands, candidate{col: col, op: op, other: x.L})
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
					cands = append(cands, candidate{col: col, op: "IN", list: x.List})
				}
			}
		}
	}
	indexFor := func(col int) *Index {
		var best *Index
		for _, ix := range l.t.indexes {
			if ix.col == col && l.t.Columns[col].Type != value.Any && (best == nil || ix.Unique && !best.Unique) {
				best = ix
			}
		}
		return best
	}
	name := l.t.Name
	compile := func(e sql.Expr) (evalFn, error) { return c.compile(e) }
	// Priority: equality on rowid, unique index, index; IN lists; ranges.
	for _, pass := range []string{"=", "IN"} {
		var best *access
		for _, cd := range cands {
			if cd.op != pass {
				continue
			}
			var fns []evalFn
			if pass == "=" {
				f, err := compile(cd.other)
				if err != nil {
					return nil, err
				}
				fns = []evalFn{f}
			} else {
				for _, a := range cd.list {
					f, err := compile(a)
					if err != nil {
						return nil, err
					}
					fns = append(fns, f)
				}
			}
			if cd.col < 0 {
				k := accRowidEq
				if pass == "IN" {
					k = accRowidIn
				}
				return &access{kind: k, eq: fns, colTyp: value.Integer,
					desc: fmt.Sprintf("SEARCH %s USING ROWID (%s)", name, describeOp("rowid", pass, len(fns)))}, nil
			}
			ix := indexFor(cd.col)
			if ix == nil || best != nil && (best.ix.Unique || !ix.Unique) {
				continue
			}
			k := accIndexEq
			if pass == "IN" {
				k = accIndexIn
			}
			best = &access{kind: k, ix: ix, eq: fns, colTyp: l.t.Columns[cd.col].Type,
				desc: fmt.Sprintf("SEARCH %s USING INDEX %s (%s)", name, ix.Name, describeOp(ix.Column, pass, len(fns)))}
		}
		if best != nil {
			return best, nil
		}
	}
	// Ranges: prefer rowid, then any index. Combine lower and upper bounds.
	type rng struct {
		lo, hi       sql.Expr
		loInc, hiInc bool
	}
	ranges := map[int]*rng{}
	order := []int{}
	for _, cd := range cands {
		if cd.op == "=" || cd.op == "IN" {
			continue
		}
		if cd.col >= 0 && indexFor(cd.col) == nil {
			continue
		}
		r := ranges[cd.col]
		if r == nil {
			r = &rng{}
			ranges[cd.col] = r
			order = append(order, cd.col)
		}
		switch cd.op {
		case ">", ">=":
			if r.lo == nil {
				r.lo, r.loInc = cd.other, cd.op == ">="
			}
		case "<", "<=":
			if r.hi == nil {
				r.hi, r.hiInc = cd.other, cd.op == "<="
			}
		}
	}
	pick := -2
	for _, col := range order {
		if col == -1 {
			pick = -1
			break
		}
		if pick == -2 {
			pick = col
		}
	}
	if pick == -2 {
		return &access{kind: accFull, desc: "SCAN " + name}, nil
	}
	r := ranges[pick]
	a := &access{loInc: r.loInc, hiInc: r.hiInc}
	var err error
	if r.lo != nil {
		if a.lo, err = compile(r.lo); err != nil {
			return nil, err
		}
	}
	if r.hi != nil {
		if a.hi, err = compile(r.hi); err != nil {
			return nil, err
		}
	}
	colName := "rowid"
	if pick == -1 {
		a.kind, a.colTyp = accRowidRange, value.Integer
	} else {
		a.kind, a.ix, a.colTyp = accIndexRange, indexFor(pick), l.t.Columns[pick].Type
		colName = a.ix.Column
	}
	var parts []string
	if r.lo != nil {
		parts = append(parts, colName+map[bool]string{true: ">=", false: ">"}[r.loInc]+"?")
	}
	if r.hi != nil {
		parts = append(parts, colName+map[bool]string{true: "<=", false: "<"}[r.hiInc]+"?")
	}
	if pick == -1 {
		a.desc = fmt.Sprintf("SEARCH %s USING ROWID RANGE (%s)", name, strings.Join(parts, " AND "))
	} else {
		a.desc = fmt.Sprintf("SEARCH %s USING INDEX %s RANGE (%s)", name, a.ix.Name, strings.Join(parts, " AND "))
	}
	return a, nil
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
			if err != nil || sc.cols[i].level >= li {
				ok = false
			}
		case *sql.Func:
			if isAggregateCall(x) {
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
func (x *execCtx) scan(t *Table, acc *access, e *env, fn func(rowid int64, row []any) (bool, error)) error {
	tree := btree.Open(x.v, t.Root)
	emit := func(rowid int64) (bool, error) {
		row, ok, err := x.getRow(t, rowid)
		if err != nil || !ok {
			return err == nil, err
		}
		return fn(rowid, row)
	}
	switch acc.kind {
	case accRowidEq, accRowidIn, accIndexEq, accIndexIn:
		seen := map[string]bool{}
		for _, f := range acc.eq {
			v, err := f(e)
			if err != nil {
				return err
			}
			k, ok := keyValue(v, acc.colTyp)
			if !ok {
				continue // NULL or incomparable type: cannot be equal
			}
			if acc.kind == accRowidEq || acc.kind == accRowidIn {
				key := value.EncodeRowID(k.(int64))
				if seen[string(key)] {
					continue
				}
				seen[string(key)] = true
				if cont, err := emit(k.(int64)); err != nil || !cont {
					return err
				}
				continue
			}
			prefix := value.AppendKey(nil, k)
			if seen[string(prefix)] {
				continue
			}
			seen[string(prefix)] = true
			var ids []int64
			c := btree.Open(x.v, acc.ix.Root).Seek(prefix)
			for ; c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
				ids = append(ids, value.DecodeRowID(c.Key()[len(prefix):]))
			}
			if err := c.Err(); err != nil {
				return err
			}
			for _, id := range ids {
				if cont, err := emit(id); err != nil || !cont {
					return err
				}
			}
		}
		return nil
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
	case accIndexRange:
		start := value.MinNonNullKey
		var lowPrefix, stop []byte
		stopInc := false
		if acc.lo != nil {
			v, err := acc.lo(e)
			if err != nil {
				return err
			}
			if v == nil {
				return nil
			}
			if k, ok, inc := rangeBound(v, acc.colTyp, true, acc.loInc); ok {
				start = value.AppendKey(nil, k)
				if !inc {
					lowPrefix = start
				}
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
			if k, ok, inc := rangeBound(v, acc.colTyp, false, acc.hiInc); ok {
				stop, stopInc = value.AppendKey(nil, k), inc
			}
		}
		var ids []int64
		c := btree.Open(x.v, acc.ix.Root).Seek(start)
		for ; c.Valid(); c.Next() {
			key := c.Key()
			if lowPrefix != nil && bytes.HasPrefix(key, lowPrefix) {
				continue
			}
			if stop != nil {
				if bytes.HasPrefix(key, stop) {
					if !stopInc {
						break
					}
				} else if bytes.Compare(key, stop) > 0 {
					break
				}
			}
			ids = append(ids, value.DecodeRowID(key[len(key)-8:]))
		}
		if err := c.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if cont, err := emit(id); err != nil || !cont {
				return err
			}
		}
		return nil
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

// buildLevels resolves the FROM clause into a scope and per-table levels,
// pushes WHERE conjuncts down to the earliest level where they can be
// evaluated and chooses an access path per level. Remaining conditions are
// returned as the final filter.
func (x *execCtx) buildLevels(from []sql.TableRef, lefts []bool, ons []sql.Expr, where sql.Expr) (*scope, []*level, []evalFn, error) {
	sc := &scope{}
	var levels []*level
	seen := map[string]bool{}
	for i, ref := range from {
		t, err := x.table(ref.Name)
		if err != nil {
			return nil, nil, nil, err
		}
		alias := ref.Alias
		if alias == "" {
			alias = t.Name
		}
		if seen[strings.ToLower(alias)] {
			return nil, nil, nil, fmt.Errorf("bangdb: ambiguous table name %s (use an alias)", alias)
		}
		seen[strings.ToLower(alias)] = true
		l := &level{t: t, alias: alias, offset: len(sc.cols), left: lefts[i]}
		for _, c := range t.Columns {
			sc.cols = append(sc.cols, scopeCol{table: strings.ToLower(alias), name: strings.ToLower(c.Name), level: i})
		}
		sc.cols = append(sc.cols, scopeCol{table: strings.ToLower(alias), name: "rowid", level: i, hidden: true})
		levels = append(levels, l)
	}
	c := &compiler{sc: sc}
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
			accessConds[i] = append(accessConds[i], cj)
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
		// NULL-extended row, so it is evaluated after the join.
		if levels[lvl].left {
			final = append(final, f)
			continue
		}
		levels[lvl].filters = append(levels[lvl].filters, f)
		accessConds[lvl] = append(accessConds[lvl], cj)
	}
	for i, l := range levels {
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
		err := x.scan(l.t, l.acc, e, func(rowid int64, row []any) (bool, error) {
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
