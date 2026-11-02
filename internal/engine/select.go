package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

type outRow struct {
	vals []any
	keys []any
}

// selectPlan is a compiled SELECT.
type selectPlan struct {
	sc      *scope
	levels  []*level
	final   []evalFn
	cols    []string
	types   []string
	items   []evalFn
	exprs   []sql.Expr // output expressions (with * expanded)
	orderBy []orderFn
	agg     bool
	aggs    []*aggSpec
	wins    []*winSpec
	groupBy []evalFn
	having  evalFn
	ordered bool // the scan already delivers rows in ORDER BY order
}

type orderFn struct {
	outIdx int // >= 0: sort by output column
	fn     evalFn
	desc   bool
	coll   string // collation applied to the sort key
}

func (x *execCtx) planSelect(st *sql.Select) (*selectPlan, error) {
	p := &selectPlan{}
	var from []sql.TableRef
	var lefts []bool
	var ons []sql.Expr
	if st.From != nil {
		from = append(from, *st.From)
		lefts = append(lefts, false)
		ons = append(ons, nil)
		for _, j := range st.Joins {
			from = append(from, j.Table)
			lefts = append(lefts, j.Left)
			ons = append(ons, j.On)
		}
	}
	var err error
	keep := x.keepJoinOrder
	x.keepJoinOrder = len(st.OrderBy) > 0 && st.Limit != nil
	p.sc, p.levels, p.final, err = x.buildLevels(from, lefts, ons, st.Where)
	x.keepJoinOrder = keep
	if err != nil {
		return nil, err
	}

	p.agg = len(st.GroupBy) > 0 || st.Having != nil
	for _, it := range st.Items {
		if !it.Star && hasAggregate(it.Expr) {
			p.agg = true
		}
	}
	for _, o := range st.OrderBy {
		if hasAggregate(o.Expr) {
			p.agg = true
		}
	}
	c := &compiler{x: x, sc: p.sc, allowAgg: p.agg, aggs: &p.aggs, wins: &p.wins}

	// Projection.
	var exprs []sql.Expr
	for _, it := range st.Items {
		if it.Star {
			if st.From == nil {
				return nil, errors.New("bangdb: no tables specified for *")
			}
			matched := false
			for _, col := range p.sc.cols {
				if col.hidden || it.StarTable != "" && col.table != strings.ToLower(it.StarTable) {
					continue
				}
				matched = true
				exprs = append(exprs, &sql.ColumnRef{Table: col.table, Name: col.name})
				p.cols = append(p.cols, p.displayName(col))
			}
			if !matched {
				return nil, fmt.Errorf("bangdb: no such table: %s", it.StarTable)
			}
			continue
		}
		exprs = append(exprs, it.Expr)
		p.cols = append(p.cols, outputName(it))
	}
	p.exprs = exprs
	for _, e := range exprs {
		p.types = append(p.types, p.declType(e))
	}
	for _, e := range exprs {
		f, err := c.compile(e)
		if err != nil {
			return nil, err
		}
		p.items = append(p.items, f)
	}

	c.aliases = map[string]sql.Expr{}
	for _, it := range st.Items {
		if it.Alias != "" {
			c.aliases[strings.ToLower(it.Alias)] = it.Expr
		}
	}
	// ORDER BY: output ordinal, output alias, or an expression.
	for _, o := range st.OrderBy {
		of := orderFn{outIdx: -1, desc: o.Desc}
		if lit, ok := o.Expr.(*sql.Literal); ok {
			n, isInt := lit.Value.(int64)
			if !isInt || n < 1 || int(n) > len(p.items) {
				return nil, fmt.Errorf("bangdb: ORDER BY term out of range: %s", sql.FormatExpr(o.Expr))
			}
			of.outIdx = int(n) - 1
		} else if cr, ok := o.Expr.(*sql.ColumnRef); ok && cr.Table == "" {
			for i, it := range st.Items {
				if it.Alias != "" && strings.EqualFold(it.Alias, cr.Name) {
					of.outIdx = i
					break
				}
			}
		}
		if of.outIdx >= 0 {
			of.coll = c.collationOf(exprs[of.outIdx])
		}
		if of.outIdx < 0 {
			f, err := c.compile(o.Expr)
			if err != nil {
				return nil, err
			}
			of.fn = withCollation(f, c.collationOf(o.Expr))
		}
		p.orderBy = append(p.orderBy, of)
	}

	if !p.agg && !st.Distinct && len(st.OrderBy) > 0 && len(p.levels) > 0 && len(p.wins) == 0 {
		p.ordered = p.useScanOrder(st)
	}
	defer p.coveringIndexes() // once every expression is compiled
	if p.agg {
		gc := &compiler{x: x, sc: p.sc, aliases: c.aliases} // GROUP BY may name a select alias
		for _, g := range st.GroupBy {
			if hasAggregate(g) {
				return nil, errors.New("bangdb: aggregate functions are not allowed in GROUP BY")
			}
			f, err := gc.compile(g)
			if err != nil {
				return nil, err
			}
			p.groupBy = append(p.groupBy, withCollation(f, gc.collationOf(g)))
		}
		if st.Having != nil {
			hc := *c
			hc.wins = nil
			if p.having, err = hc.compile(st.Having); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

func isColumn(e sql.Expr) bool { _, ok := e.(*sql.ColumnRef); return ok }

// outputName is the result column name of a select item.
func outputName(it sql.SelectItem) string {
	switch {
	case it.Alias != "":
		return it.Alias
	case isColumn(it.Expr):
		return it.Expr.(*sql.ColumnRef).Name
	}
	name := sql.FormatExpr(it.Expr)
	if _, isBin := it.Expr.(*sql.Binary); isBin {
		name = name[1 : len(name)-1]
	}
	return name
}

func (p *selectPlan) displayName(col scopeCol) string {
	for _, l := range p.levels {
		if strings.ToLower(l.alias) == col.table {
			for _, c := range l.t.Columns {
				if strings.ToLower(c.Name) == col.name {
					return c.Name
				}
			}
		}
	}
	return col.name
}

func evalInt(e sql.Expr, args []any, what string) (int64, error) {
	f, err := (&compiler{}).compile(e)
	if err != nil {
		return 0, err
	}
	v, err := f(&env{args: args})
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		if fl, isF := v.(float64); isF && fl == float64(int64(fl)) {
			return int64(fl), nil
		}
		return 0, fmt.Errorf("bangdb: %s must be an integer", what)
	}
	return n, nil
}

func (x *execCtx) execSelect(st *sql.Select) (Result, error) {
	r, err := x.prepareSelect(st)
	if err != nil {
		return Result{}, err
	}
	var rows [][]any
	err = x.runSelect(r, func(vals []any) (bool, error) {
		rows = append(rows, vals)
		return true, nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Columns: r.p.cols, Types: r.p.types, Rows: rows}, nil
}

// declType returns the declared type of an output expression that is a
// plain column reference ("" otherwise).
func (p *selectPlan) declType(e sql.Expr) string {
	ref, ok := e.(*sql.ColumnRef)
	if !ok {
		return ""
	}
	i, err := p.sc.resolve(ref.Table, ref.Name)
	if err != nil {
		return ""
	}
	l := p.levels[p.sc.cols[i].level]
	col := i - l.offset
	if col == len(l.t.Columns) {
		return "INTEGER" // rowid
	}
	c := l.t.Columns[col]
	if c.DeclType != "" {
		return c.DeclType
	}
	if c.Type == value.Any {
		return ""
	}
	return string(c.Type)
}

// selectRun is a planned SELECT ready to execute.
type selectRun struct {
	st            *sql.Select
	p             *selectPlan
	limit, offset int64

	// compound SELECT (UNION / INTERSECT / EXCEPT)
	parts []*selectRun
	ops   []string
	order []compoundOrder
}

type compoundOrder struct {
	col  int
	desc bool
}

// small reports whether the result is known to be small (a few hundred rows
// at most), so it can be produced at once instead of streamed.
func (r *selectRun) small() bool {
	if r.parts != nil {
		return false
	}
	if r.limit >= 0 && r.limit <= 256 {
		return true
	}
	p := r.p
	if p.agg && len(p.groupBy) == 0 || len(p.levels) == 0 {
		return true
	}
	if len(p.levels) != 1 || p.levels[0].virtual != nil {
		return false
	}
	switch acc := p.levels[0].acc; acc.kind {
	case accRowidEq:
		return true
	case accRowidIn:
		return len(acc.eq) <= 256
	case accIndex:
		return acc.ix.Unique && len(acc.prefix) == len(acc.ix.cols) ||
			acc.ix.Unique && len(acc.prefix) == len(acc.ix.cols)-1 && acc.in != nil && len(acc.in) <= 256
	}
	return false
}

func (x *execCtx) prepareSelect(st *sql.Select) (*selectRun, error) {
	if len(st.Compound) > 0 {
		return x.prepareCompound(st)
	}
	var p *selectPlan
	if x.plans != nil && x.outer == nil {
		p = x.plans.get(st, x.cat)
	}
	if p == nil {
		var err error
		if p, err = x.planSelect(st); err != nil {
			return nil, err
		}
		if x.plans != nil && x.outer == nil && p.cacheable(st) {
			x.plans.put(st, x.cat, p)
		}
	}
	var err error
	r := &selectRun{st: st, p: p, limit: -1}
	if st.Limit != nil {
		if r.limit, err = evalInt(st.Limit, x.args, "LIMIT"); err != nil {
			return nil, err
		}
	}
	if st.Offset != nil {
		if r.offset, err = evalInt(st.Offset, x.args, "OFFSET"); err != nil {
			return nil, err
		}
		r.offset = max(r.offset, 0)
	}
	return r, nil
}

// runSelect executes a planned SELECT and passes each result row to sink,
// which returns false to stop early. Queries without sorting, grouping or
// DISTINCT stream rows as they are found; others are buffered (sorting with
// LIMIT keeps only the top rows in memory).
func (x *execCtx) runSelect(r *selectRun, sink func(vals []any) (bool, error)) error {
	if r.parts != nil {
		return x.runCompound(r, sink)
	}
	p, st, limit, offset := r.p, r.st, r.limit, r.offset
	if !p.agg && (len(p.orderBy) == 0 || p.ordered) && !st.Distinct && len(p.wins) == 0 {
		return x.runStreaming(r, sink)
	}
	e := &env{row: make([]any, len(p.sc.cols)), args: x.args}
	keyLess := func(a, b []any) bool {
		for k, o := range p.orderBy {
			if c := value.Compare(a[k], b[k]); c != 0 {
				return (c < 0) != o.desc
			}
		}
		return false
	}
	var out []outRow
	sortOut := func() {
		sort.SliceStable(out, func(i, j int) bool { return keyLess(out[i].keys, out[j].keys) })
	}
	topK := int64(-1)
	if len(p.orderBy) > 0 && limit >= 0 && !st.Distinct {
		topK = offset + limit
	}
	emitted, skipped := int64(0), int64(0)
	stopped := false

	// Buffered rows pass DISTINCT, then ORDER BY (top-K, or a sorter that
	// spills to disk), then OFFSET / LIMIT.
	cfg := x.spillCfg()
	final := func(vals []any) error {
		if stopped {
			return nil
		}
		if skipped < offset {
			skipped++
			return nil
		}
		if limit >= 0 && emitted >= limit {
			stopped = true
			return nil
		}
		cont, err := sink(vals)
		emitted++
		stopped = !cont || limit >= 0 && emitted >= limit
		return err
	}
	var orderSort *extSorter
	if topK < 0 && len(p.orderBy) > 0 {
		orderSort = newSorter(cfg, keyLess)
		defer orderSort.Close()
	}
	nk := len(p.orderBy)
	ordered := func(row outRow) error {
		switch {
		case topK >= 0:
			out = append(out, row)
			if int64(len(out)) >= 2*topK+1024 {
				sortOut()
				out = out[:topK]
			}
			return nil
		case orderSort != nil:
			return orderSort.Add(append(row.keys[:nk:nk], row.vals...))
		}
		return final(row.vals)
	}
	var seen map[string]bool
	seenBytes := 0
	var distinctSort *extSorter
	if st.Distinct {
		seen = map[string]bool{}
		distinctSort = newSorter(cfg, func(a, b []any) bool { return bytes.Compare(a[0].([]byte), b[0].([]byte)) < 0 })
		defer distinctSort.Close()
	}
	stage := func(row outRow) error {
		if !st.Distinct {
			return ordered(row)
		}
		var k []byte
		for _, v := range row.vals {
			k = value.AppendKey(k, v)
		}
		if seen[string(k)] {
			return nil
		}
		if cfg.limit <= 0 || seenBytes < cfg.limit/2 {
			seen[string(k)] = true
			seenBytes += 2*len(k) + 96
			return ordered(row)
		}
		// Too many distinct rows to remember: sort the rest by value and
		// drop adjacent duplicates at the end.
		rec := append([]any{k}, row.keys...)
		return distinctSort.Add(append(rec, row.vals...))
	}
	project := func() error {
		vals := make([]any, len(p.items))
		for i, f := range p.items {
			v, err := f(e)
			if err != nil {
				return err
			}
			vals[i] = v
		}
		row := outRow{vals: vals}
		if len(p.orderBy) > 0 {
			row.keys = make([]any, len(p.orderBy))
			for i, o := range p.orderBy {
				if o.outIdx >= 0 {
					row.keys[i] = foldValue(vals[o.outIdx], o.coll)
					continue
				}
				v, err := o.fn(e)
				if err != nil {
					return err
				}
				row.keys[i] = v
			}
		}
		return stage(row)
	}
	// With window functions the rows are collected first; they are
	// projected once the windows are computed.
	var winRows []winRow
	produce := project
	if len(p.wins) > 0 {
		produce = func() error {
			r, err := collectWindowRow(p.wins, e)
			winRows = append(winRows, r)
			return err
		}
	}

	if !p.agg {
		emit := func() (bool, error) {
			if err := produce(); err != nil {
				return false, err
			}
			return !stopped, nil
		}
		if len(p.levels) == 0 {
			if ok, err := passes(p.final, e); err != nil {
				return err
			} else if ok {
				if _, err := emit(); err != nil {
					return err
				}
			}
		} else {
			if err := x.runJoin(p.levels, p.final, e, emit); err != nil {
				return err
			}
		}
	} else if err := x.runGrouped(p, e, produce); err != nil {
		return err
	}
	if len(p.wins) > 0 {
		vals, err := computeWindows(p.wins, winRows, x.args)
		if err != nil {
			return err
		}
		for i, r := range winRows {
			e.row, e.aggs, e.wins = r.row, r.aggs, vals[i]
			if err := project(); err != nil {
				return err
			}
			winRows[i] = winRow{}
		}
	}
	if distinctSort != nil {
		it, err := distinctSort.Finish()
		if err != nil {
			return err
		}
		var last []byte
		for {
			rec, err := it.Next()
			if err != nil || rec == nil {
				if err != nil {
					return err
				}
				break
			}
			k := rec[0].([]byte)
			if last != nil && bytes.Equal(k, last) {
				continue
			}
			last = k
			if err := ordered(outRow{keys: rec[1 : 1+nk], vals: rec[1+nk:]}); err != nil {
				return err
			}
		}
	}
	if topK >= 0 {
		sortOut()
		for _, r := range out {
			if err := final(r.vals); err != nil || stopped {
				return err
			}
		}
		return nil
	}
	if orderSort != nil {
		it, err := orderSort.Finish()
		if err != nil {
			return err
		}
		for !stopped {
			rec, err := it.Next()
			if err != nil || rec == nil {
				return err
			}
			if err := final(rec[nk:]); err != nil {
				return err
			}
		}
	}
	return nil
}

// runStreaming is runSelect for queries whose rows come out in scan order
// (no grouping, DISTINCT or sorting): each row is projected and passed on
// as it is found, honouring OFFSET and LIMIT. It keeps per-run state small,
// as it serves the common short queries.
func (x *execCtx) runStreaming(r *selectRun, sink func(vals []any) (bool, error)) error {
	p := r.p
	if r.limit == 0 {
		return nil
	}
	s := streamState{x: x, p: p, sink: sink, limit: r.limit, offset: r.offset}
	s.e = &s.env
	s.e.args = x.args
	if n := len(p.sc.cols); n <= len(s.e.buf) {
		s.e.row = s.e.buf[:n:n]
	} else {
		s.e.row = make([]any, n)
	}
	if len(p.levels) == 0 {
		ok, err := passes(p.final, s.e)
		if err != nil || !ok {
			return err
		}
		_, err = s.emit()
		return err
	}
	return x.runJoin(p.levels, p.final, s.e, s.emit)
}

type streamState struct {
	x                *execCtx
	p                *selectPlan
	e                *env
	sink             func(vals []any) (bool, error)
	limit, offset    int64
	emitted, skipped int64
	env              env // s.e points here (one allocation for both)
}

func (s *streamState) emit() (bool, error) {
	if s.skipped < s.offset {
		s.skipped++
		return true, nil
	}
	vals := make([]any, len(s.p.items))
	for i, f := range s.p.items {
		v, err := f(s.e)
		if err != nil {
			return false, err
		}
		vals[i] = v
	}
	cont, err := s.sink(vals)
	s.emitted++
	return cont && err == nil && (s.limit < 0 || s.emitted < s.limit), err
}

type group struct {
	row    []any
	states []*aggState
}

// runGrouped aggregates in a hash table. When the groups outgrow the sort
// memory limit, rows of further groups are sorted (spilling to disk) and
// aggregated one group at a time after the in-memory groups.
func (x *execCtx) runGrouped(p *selectPlan, e *env, project func() error) error {
	groups := map[string]*group{}
	var order []*group
	newGroup := func() *group {
		g := &group{row: append([]any(nil), e.row...)}
		for _, s := range p.aggs {
			g.states = append(g.states, newAggState(s))
		}
		return g
	}
	cfg := x.spillCfg()
	used := 0
	spill := newSorter(cfg, func(a, b []any) bool { return bytes.Compare(a[0].([]byte), b[0].([]byte)) < 0 })
	defer spill.Close()
	spilling := false
	emit := func() (bool, error) {
		var key []byte
		for _, f := range p.groupBy {
			v, err := f(e)
			if err != nil {
				return false, err
			}
			key = value.AppendKey(key, v)
		}
		g := groups[string(key)]
		if g == nil {
			if spilling {
				return true, spill.Add(append([]any{key}, e.row...))
			}
			g = newGroup()
			groups[string(key)] = g
			order = append(order, g)
			used += 2*len(key) + rowBytes(g.row) + 160*len(g.states) + 160 // map entry, group, states
			spilling = cfg.limit > 0 && used > cfg.limit/2
		}
		for _, st := range g.states {
			if err := st.step(e); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if len(p.levels) == 0 {
		if ok, err := passes(p.final, e); err != nil {
			return err
		} else if ok {
			if _, err := emit(); err != nil {
				return err
			}
		}
	} else if err := x.runJoin(p.levels, p.final, e, emit); err != nil {
		return err
	}
	// An aggregate query without GROUP BY always yields one row.
	if len(order) == 0 && len(p.groupBy) == 0 {
		for i := range e.row {
			e.row[i] = nil
		}
		order = append(order, newGroup())
	}
	finish := func(g *group) error {
		e.row = g.row
		e.aggs = make([]any, len(g.states))
		for i, st := range g.states {
			v, err := st.result()
			if err != nil {
				return err
			}
			e.aggs[i] = v
		}
		if p.having != nil {
			v, err := p.having(e)
			if err != nil {
				return err
			}
			if ok, _ := value.Truthy(v); !ok {
				return nil
			}
		}
		return project()
	}
	for _, g := range order {
		if err := finish(g); err != nil {
			return err
		}
	}
	if !spilling {
		return nil
	}
	groups, order = nil, nil
	it, err := spill.Finish()
	if err != nil {
		return err
	}
	row := make([]any, len(e.row))
	var cur *group
	var curKey []byte
	for {
		rec, err := it.Next()
		if err != nil {
			return err
		}
		if cur != nil && (rec == nil || !bytes.Equal(rec[0].([]byte), curKey)) {
			if err := finish(cur); err != nil {
				return err
			}
			cur = nil
		}
		if rec == nil {
			return nil
		}
		copy(row, rec[1:])
		e.row = row
		if cur == nil {
			cur, curKey = newGroup(), rec[0].([]byte)
		}
		for _, st := range cur.states {
			if err := st.step(e); err != nil {
				return err
			}
		}
	}
}

// ---- EXPLAIN ----

func (x *execCtx) explain(s sql.Stmt) (Result, error) {
	res := Result{Columns: []string{"plan"}}
	add := func(line string) { res.Rows = append(res.Rows, []any{line}) }
	var levels []*level
	switch st := s.(type) {
	case *sql.Select:
		p, err := x.planSelect(st)
		if err != nil {
			return res, err
		}
		levels = p.levels
		if len(levels) == 0 {
			add("CONSTANT ROW")
		}
		defer func() {
			if p.agg {
				add("AGGREGATE")
			}
			if len(st.OrderBy) > 0 && !p.ordered {
				add("SORT")
			}
		}()
	case *sql.Update:
		_, lv, _, err := x.buildLevels([]sql.TableRef{{Name: st.Table}}, []bool{false}, []sql.Expr{nil}, st.Where)
		if err != nil {
			return res, err
		}
		levels = lv
		add("UPDATE " + st.Table)
	case *sql.Delete:
		_, lv, _, err := x.buildLevels([]sql.TableRef{{Name: st.Table}}, []bool{false}, []sql.Expr{nil}, st.Where)
		if err != nil {
			return res, err
		}
		levels = lv
		add("DELETE FROM " + st.Table)
	default:
		add(fmt.Sprintf("%T", s)[5:])
		return res, nil
	}
	for i, l := range levels {
		line := strings.Repeat("  ", i) + l.acc.desc
		if l.left {
			line += " (LEFT JOIN)"
		}
		add(line)
	}
	return res, nil
}

// ---- DML ----

// tableScope is the scope of one table's columns plus its hidden rowid.
func tableScope(t *Table, alias string, level int, qualifiedOnly bool) []scopeCol {
	var cols []scopeCol
	for _, c := range t.Columns {
		cols = append(cols, scopeCol{table: strings.ToLower(alias), name: strings.ToLower(c.Name), level: level, hidden: qualifiedOnly, coll: c.Collate})
	}
	return append(cols, scopeCol{table: strings.ToLower(alias), name: "rowid", level: level, hidden: true})
}

// returning evaluates a RETURNING list for changed rows.
type returning struct {
	cols []string
	fns  []evalFn
	e    *env
}

func (x *execCtx) compileReturning(t *Table, items []sql.SelectItem) (*returning, error) {
	if len(items) == 0 {
		return nil, nil
	}
	sc := &scope{cols: tableScope(t, t.Name, 0, false)}
	c := &compiler{x: x, sc: sc}
	r := &returning{e: &env{row: make([]any, len(sc.cols)), args: x.args}}
	for _, it := range items {
		if it.Star {
			if it.StarTable != "" && !strings.EqualFold(it.StarTable, t.Name) {
				return nil, fmt.Errorf("bangdb: no such table: %s", it.StarTable)
			}
			for i, col := range t.Columns {
				i := i
				r.cols = append(r.cols, col.Name)
				r.fns = append(r.fns, func(e *env) (any, error) { return e.row[i], nil })
			}
			continue
		}
		if hasAggregate(it.Expr) {
			return nil, errors.New("bangdb: aggregate functions are not allowed in RETURNING")
		}
		f, err := c.compile(it.Expr)
		if err != nil {
			return nil, err
		}
		r.cols = append(r.cols, outputName(it))
		r.fns = append(r.fns, f)
	}
	return r, nil
}

// add appends the RETURNING values of one row to res.
func (r *returning) add(res *Result, rowid int64, row []any) error {
	if r == nil {
		return nil
	}
	copy(r.e.row, row)
	r.e.row[len(row)] = rowid
	out := make([]any, len(r.fns))
	for i, f := range r.fns {
		v, err := f(r.e)
		if err != nil {
			return err
		}
		out[i] = v
	}
	res.Rows = append(res.Rows, out)
	return nil
}

// conflict is an existing row that clashes with a new row.
type conflict struct {
	rowid int64
	cols  []string // the PRIMARY KEY / UNIQUE columns that clash
}

// conflicts lists existing rows that share a key with row.
func (x *execCtx) conflicts(t *Table, row []any) ([]conflict, error) {
	var out []conflict
	if t.RowidCol >= 0 && row[t.RowidCol] != nil {
		if _, ok, err := x.getRow(t, row[t.RowidCol].(int64)); err != nil {
			return nil, err
		} else if ok {
			out = append(out, conflict{row[t.RowidCol].(int64), []string{t.Columns[t.RowidCol].Name}})
		}
	}
	for _, ix := range t.indexes {
		if !ix.Unique || ix.hasNull(row) {
			continue
		}
		if ok, err := ix.covers(row); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		id, found, err := x.findKey(t, ix, row, math.MinInt64)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, conflict{id, ix.Columns})
		}
	}
	return out, nil
}

func sameColumns(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			found = found || strings.EqualFold(x, y)
		}
		if !found {
			return false
		}
	}
	return true
}

// upsert is a compiled ON CONFLICT DO UPDATE clause.
type upsert struct {
	sets  []colSet
	where evalFn
	e     *env
}

type colSet struct {
	col int
	fn  evalFn
}

func (x *execCtx) execInsert(st *sql.Insert) (Result, error) {
	t, err := x.table(st.Table)
	if err != nil {
		return Result{}, err
	}
	positions := make([]int, 0, len(t.Columns))
	if len(st.Columns) == 0 {
		for i := range t.Columns {
			positions = append(positions, i)
		}
	} else {
		used := map[int]bool{}
		for _, name := range st.Columns {
			i := t.ColIndex(name)
			if i < 0 {
				return Result{}, fmt.Errorf("bangdb: table %s has no column named %s", t.Name, name)
			}
			if used[i] {
				return Result{}, fmt.Errorf("bangdb: column %s specified more than once", name)
			}
			used[i] = true
			positions = append(positions, i)
		}
	}
	// Defaults for columns that were not given.
	defaults := make([]evalFn, len(t.Columns))
	for i, c := range t.Columns {
		if c.defExpr != nil {
			if defaults[i], err = (&compiler{}).compile(c.defExpr); err != nil {
				return Result{}, err
			}
		}
	}
	e := &env{args: x.args}

	// Source rows: evaluated completely before anything is inserted, so
	// INSERT INTO t SELECT ... FROM t sees the table as it was.
	var src [][]any
	if st.Select != nil {
		res, err := x.execSelect(st.Select)
		if err != nil {
			return Result{}, err
		}
		if len(res.Columns) != len(positions) {
			return Result{}, fmt.Errorf("bangdb: SELECT returns %d columns for %d target columns", len(res.Columns), len(positions))
		}
		src = res.Rows
	} else {
		c := &compiler{x: x, sc: &scope{outer: x.outer}} // outer: NEW/OLD in a trigger
		for _, exprs := range st.Rows {
			if len(exprs) != len(positions) {
				return Result{}, fmt.Errorf("bangdb: %d values for %d columns", len(exprs), len(positions))
			}
			vals := make([]any, len(exprs))
			for j, ex := range exprs {
				f, err := c.compile(ex)
				if err != nil {
					return Result{}, err
				}
				if vals[j], err = f(e); err != nil {
					return Result{}, err
				}
			}
			src = append(src, vals)
		}
	}

	ret, err := x.compileReturning(t, st.Returning)
	if err != nil {
		return Result{}, err
	}
	oc := st.OnConflict
	if oc != nil && len(oc.Target) > 0 {
		ok := t.RowidCol >= 0 && sameColumns(oc.Target, []string{t.Columns[t.RowidCol].Name})
		for _, ix := range t.indexes {
			ok = ok || ix.Unique && sameColumns(oc.Target, ix.Columns)
		}
		if !ok {
			return Result{}, errors.New("bangdb: ON CONFLICT target does not match any PRIMARY KEY or UNIQUE constraint")
		}
	}
	var up *upsert
	if oc != nil && !oc.DoNothing {
		sc := &scope{cols: append(tableScope(t, t.Name, 0, false), tableScope(t, "excluded", 1, true)...)}
		c := &compiler{x: x, sc: sc}
		up = &upsert{e: &env{row: make([]any, len(sc.cols)), args: x.args}}
		for _, a := range oc.Sets {
			i := t.ColIndex(a.Column)
			if i < 0 {
				return Result{}, fmt.Errorf("bangdb: no such column: %s", a.Column)
			}
			f, err := c.compile(a.Value)
			if err != nil {
				return Result{}, err
			}
			up.sets = append(up.sets, colSet{i, f})
		}
		if oc.Where != nil {
			if up.where, err = c.compile(oc.Where); err != nil {
				return Result{}, err
			}
		}
	}

	res := Result{Columns: nil}
	if ret != nil {
		res.Columns = ret.cols
	}
	var maxID int64
	for _, vals := range src {
		row := make([]any, len(t.Columns))
		given := make([]bool, len(t.Columns))
		for j, v := range vals {
			row[positions[j]] = v
			given[positions[j]] = true
		}
		for i := range row {
			if !given[i] && defaults[i] != nil {
				if row[i], err = defaults[i](e); err != nil {
					return Result{}, err
				}
			}
		}
		if err := prepareRow(t, row); err != nil {
			return Result{}, err
		}
		if oc != nil || st.Or != "" {
			clashes, err := x.conflicts(t, row)
			if err != nil {
				return Result{}, err
			}
			var hit *conflict
			for i := range clashes {
				if oc == nil || len(oc.Target) == 0 || sameColumns(oc.Target, clashes[i].cols) {
					hit = &clashes[i]
					break
				}
			}
			switch {
			case hit == nil:
				// No (matching) conflict: insert normally; other conflicts
				// raise the usual constraint error.
			case st.Or == "IGNORE" || oc != nil && oc.DoNothing:
				continue
			case st.Or == "REPLACE":
				done := map[int64]bool{}
				for _, cf := range clashes {
					if done[cf.rowid] {
						continue
					}
					done[cf.rowid] = true
					old, ok, err := x.getRow(t, cf.rowid)
					if err != nil {
						return Result{}, err
					}
					if ok {
						if err := x.removeRow(t, cf.rowid, old); err != nil {
							return Result{}, err
						}
					}
				}
			default: // DO UPDATE
				old, _, err := x.getRow(t, hit.rowid)
				if err != nil {
					return Result{}, err
				}
				n := len(t.Columns) + 1
				copy(up.e.row, old)
				up.e.row[n-1] = hit.rowid
				copy(up.e.row[n:], row)
				up.e.row[2*n-1] = nil
				if up.where != nil {
					v, err := up.where(up.e)
					if err != nil {
						return Result{}, err
					}
					if ok, _ := value.Truthy(v); !ok {
						continue
					}
				}
				newRow := append([]any(nil), old...)
				setCols := make([]int, len(up.sets))
				for i, s := range up.sets {
					if newRow[s.col], err = s.fn(up.e); err != nil {
						return Result{}, err
					}
					setCols[i] = s.col
				}
				if err := x.fireTriggers(t, "BEFORE", "UPDATE", hit.rowid, old, hit.rowid, newRow, setCols); err != nil {
					if errors.Is(err, errIgnoreRow) {
						continue
					}
					return Result{}, err
				}
				id, err := x.updateRow(t, hit.rowid, old, newRow)
				if err != nil {
					return Result{}, err
				}
				res.RowsAffected++
				if err := ret.add(&res, id, newRow); err != nil {
					return Result{}, err
				}
				if err := x.afterTriggers(t, "UPDATE", hit.rowid, old, id, newRow, setCols); err != nil {
					return Result{}, err
				}
				continue
			}
		}
		newID := int64(-1) // as in SQLite: unknown before the insert
		if t.RowidCol >= 0 && row[t.RowidCol] != nil {
			newID = row[t.RowidCol].(int64)
		}
		if err := x.fireTriggers(t, "BEFORE", "INSERT", 0, nil, newID, row, nil); err != nil {
			if errors.Is(err, errIgnoreRow) {
				continue
			}
			return Result{}, err
		}
		id, err := x.insertRow(t, row)
		if err != nil {
			return Result{}, err
		}
		if err := x.afterTriggers(t, "INSERT", 0, nil, id, row, nil); err != nil {
			return Result{}, err
		}
		res.RowsAffected++
		res.LastInsertID = id
		maxID = max(maxID, id)
		if err := ret.add(&res, id, row); err != nil {
			return Result{}, err
		}
	}
	return res, x.recordSequence(t, maxID)
}

type target struct {
	rowid int64
	row   []any
}

// collect finds the rows of a single table matching where.
func (x *execCtx) collect(t *Table, where sql.Expr) (*scope, []target, error) {
	sc, levels, final, err := x.buildLevels([]sql.TableRef{{Name: t.Name}}, []bool{false}, []sql.Expr{nil}, where)
	if err != nil {
		return nil, nil, err
	}
	e := &env{row: make([]any, len(sc.cols)), args: x.args}
	var out []target
	err = x.runJoin(levels, final, e, func() (bool, error) {
		out = append(out, target{rowid: e.row[len(t.Columns)].(int64), row: append([]any(nil), e.row[:len(t.Columns)]...)})
		return true, nil
	})
	return sc, out, err
}

func (x *execCtx) execDelete(st *sql.Delete) (Result, error) {
	t, err := x.table(st.Table)
	if err != nil {
		return Result{}, err
	}
	ret, err := x.compileReturning(t, st.Returning)
	if err != nil {
		return Result{}, err
	}
	triggers := x.hasTriggers(t)
	if ret == nil && !triggers {
		if n, done, err := x.deleteByTime(t, st.Where); done || err != nil {
			return Result{RowsAffected: n}, err
		}
	}
	_, targets, err := x.collect(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	res := Result{RowsAffected: int64(len(targets))}
	if ret != nil {
		res.Columns = ret.cols
	}
	for _, tg := range targets {
		if len(t.referencedBy) > 0 || triggers {
			// Foreign key actions or triggers may have removed or changed the row.
			row, ok, err := x.getRow(t, tg.rowid)
			if err != nil {
				return Result{}, err
			}
			if !ok {
				res.RowsAffected--
				continue
			}
			tg.row = row
		}
		if err := x.fireTriggers(t, "BEFORE", "DELETE", tg.rowid, tg.row, 0, nil, nil); err != nil {
			if errors.Is(err, errIgnoreRow) {
				res.RowsAffected--
				continue
			}
			return Result{}, err
		}
		if err := ret.add(&res, tg.rowid, tg.row); err != nil {
			return Result{}, err
		}
		if err := x.removeRow(t, tg.rowid, tg.row); err != nil {
			return Result{}, err
		}
		if err := x.afterTriggers(t, "DELETE", tg.rowid, tg.row, 0, nil, nil); err != nil {
			return Result{}, err
		}
	}
	return res, nil
}

func (x *execCtx) execUpdate(st *sql.Update) (Result, error) {
	t, err := x.table(st.Table)
	if err != nil {
		return Result{}, err
	}
	ret, err := x.compileReturning(t, st.Returning)
	if err != nil {
		return Result{}, err
	}
	sc, targets, err := x.collect(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	c := &compiler{x: x, sc: sc}
	var sets []colSet
	for _, a := range st.Sets {
		i := t.ColIndex(a.Column)
		if i < 0 {
			return Result{}, fmt.Errorf("bangdb: no such column: %s", a.Column)
		}
		f, err := c.compile(a.Value)
		if err != nil {
			return Result{}, err
		}
		sets = append(sets, colSet{i, f})
	}
	res := Result{RowsAffected: int64(len(targets))}
	if ret != nil {
		res.Columns = ret.cols
	}
	e := &env{row: make([]any, len(sc.cols)), args: x.args}
	triggers := x.hasTriggers(t)
	setCols := make([]int, len(sets))
	for i, s := range sets {
		setCols[i] = s.col
	}
	for _, tg := range targets {
		if len(t.referencedBy) > 0 || triggers {
			row, ok, err := x.getRow(t, tg.rowid)
			if err != nil {
				return Result{}, err
			}
			if !ok {
				res.RowsAffected--
				continue
			}
			tg.row = row
		}
		copy(e.row, tg.row)
		e.row[len(t.Columns)] = tg.rowid
		newRow := append([]any(nil), tg.row...)
		for _, s := range sets {
			v, err := s.fn(e)
			if err != nil {
				return Result{}, err
			}
			newRow[s.col] = v
		}
		if err := x.fireTriggers(t, "BEFORE", "UPDATE", tg.rowid, tg.row, tg.rowid, newRow, setCols); err != nil {
			if errors.Is(err, errIgnoreRow) {
				res.RowsAffected--
				continue
			}
			return Result{}, err
		}
		id, err := x.updateRow(t, tg.rowid, tg.row, newRow)
		if err != nil {
			return Result{}, err
		}
		if err := ret.add(&res, id, newRow); err != nil {
			return Result{}, err
		}
		if err := x.afterTriggers(t, "UPDATE", tg.rowid, tg.row, id, newRow, setCols); err != nil {
			return Result{}, err
		}
	}
	return res, nil
}

// updateRow replaces row oldID (with content old) by newRow, checking
// types, NOT NULL and uniqueness and maintaining indexes. It returns the
// row's (possibly changed) rowid.
func (x *execCtx) updateRow(t *Table, oldID int64, old, newRow []any) (int64, error) {
	if err := prepareRow(t, newRow); err != nil {
		return 0, err
	}
	newID := oldID
	if t.RowidCol >= 0 {
		if newRow[t.RowidCol] == nil {
			return 0, constraintErr("NOT NULL constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
		}
		newID = newRow[t.RowidCol].(int64)
	}
	if tc := t.timeColumn(); tc >= 0 && !bytes.Equal(value.AppendKey(nil, old[tc]), value.AppendKey(nil, newRow[tc])) {
		// The row's time is its position: it moves.
		var err error
		if newID, err = x.rows(t).(store.RowidAssigner).AssignRowid(newRow); err != nil {
			return 0, err
		}
	}
	if newID != oldID {
		if _, exists, err := x.getRow(t, newID); err != nil {
			return 0, err
		} else if exists {
			return 0, constraintErr("UNIQUE constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
		}
	}
	for _, ix := range t.indexes {
		if newID == oldID && ix.pred == nil && bytes.Equal(ix.fullKey(old), ix.fullKey(newRow)) {
			continue
		}
		if err := x.checkUnique(t, ix, newRow, oldID); err != nil {
			return 0, err
		}
	}
	if len(t.ForeignKeys) > 0 {
		if err := x.checkParents(t, newRow, old); err != nil {
			return 0, err
		}
	}
	if err := x.deleteRow(t, oldID, old); err != nil {
		return 0, err
	}
	if err := x.writeRow(t, newID, newRow); err != nil {
		return 0, err
	}
	x.noteRowid(t, newID)
	if err := x.parentUpdated(t, old, newRow); err != nil {
		return 0, err
	}
	return newID, x.recordSequence(t, newID)
}

// writeRow stores a row and its index entries without constraint checks.
func (x *execCtx) writeRow(t *Table, rowid int64, row []any) error {
	for _, ix := range t.indexes {
		k, err := indexEntry(ix, row, rowid)
		if err != nil {
			return err
		}
		if k == nil {
			continue
		}
		if err := x.index(ix).Put(k); err != nil {
			return err
		}
	}
	if err := x.rows(t).Put(rowid, row); err != nil {
		return err
	}
	return x.v.SpillPoint()
}

// prepareCompound plans every SELECT of a UNION / INTERSECT / EXCEPT chain.
// ORDER BY may name result columns or use their ordinal positions.
func (x *execCtx) prepareCompound(st *sql.Select) (*selectRun, error) {
	head := *st
	head.Compound, head.OrderBy, head.Limit, head.Offset = nil, nil, nil, nil
	first, err := x.prepareSelect(&head)
	if err != nil {
		return nil, err
	}
	r := &selectRun{st: st, p: &selectPlan{cols: first.p.cols, types: first.p.types}, limit: -1, parts: []*selectRun{first}}
	for _, op := range st.Compound {
		part, err := x.prepareSelect(op.Select)
		if err != nil {
			return nil, err
		}
		if len(part.p.cols) != len(first.p.cols) {
			return nil, fmt.Errorf("bangdb: SELECTs to the left and right of %s do not have the same number of result columns", op.Op)
		}
		r.parts = append(r.parts, part)
		r.ops = append(r.ops, op.Op)
	}
	for _, o := range st.OrderBy {
		col := -1
		switch e := o.Expr.(type) {
		case *sql.Literal:
			if n, ok := e.Value.(int64); ok && n >= 1 && int(n) <= len(r.p.cols) {
				col = int(n) - 1
			}
		case *sql.ColumnRef:
			for i, name := range r.p.cols {
				if e.Table == "" && strings.EqualFold(name, e.Name) {
					col = i
					break
				}
			}
		}
		if col < 0 {
			return nil, fmt.Errorf("bangdb: ORDER BY term %s does not match any column of the compound result", sql.FormatExpr(o.Expr))
		}
		r.order = append(r.order, compoundOrder{col, o.Desc})
	}
	if st.Limit != nil {
		if r.limit, err = evalInt(st.Limit, x.args, "LIMIT"); err != nil {
			return nil, err
		}
	}
	if st.Offset != nil {
		if r.offset, err = evalInt(st.Offset, x.args, "OFFSET"); err != nil {
			return nil, err
		}
		r.offset = max(r.offset, 0)
	}
	return r, nil
}

func rowKey(vals []any) string {
	var k []byte
	for _, v := range vals {
		k = append(k, numKey(v)...)
	}
	return string(k)
}

func (x *execCtx) runCompound(r *selectRun, sink func(vals []any) (bool, error)) error {
	collect := func(part *selectRun) ([][]any, error) {
		var rows [][]any
		err := x.runSelect(part, func(vals []any) (bool, error) {
			rows = append(rows, vals)
			return true, nil
		})
		return rows, err
	}
	distinct := func(rows [][]any) [][]any {
		seen := map[string]bool{}
		out := rows[:0]
		for _, row := range rows {
			if k := rowKey(row); !seen[k] {
				seen[k] = true
				out = append(out, row)
			}
		}
		return out
	}
	rows, err := collect(r.parts[0])
	if err != nil {
		return err
	}
	for i, op := range r.ops {
		right, err := collect(r.parts[i+1])
		if err != nil {
			return err
		}
		switch op {
		case "UNION ALL":
			rows = append(rows, right...)
		case "UNION":
			rows = distinct(append(rows, right...))
		case "INTERSECT", "EXCEPT":
			in := map[string]bool{}
			for _, row := range right {
				in[rowKey(row)] = true
			}
			var out [][]any
			for _, row := range distinct(rows) {
				if in[rowKey(row)] == (op == "INTERSECT") {
					out = append(out, row)
				}
			}
			rows = out
		}
	}
	if len(r.order) > 0 {
		sort.SliceStable(rows, func(i, j int) bool {
			for _, o := range r.order {
				if c := value.Compare(rows[i][o.col], rows[j][o.col]); c != 0 {
					return (c < 0) != o.desc
				}
			}
			return false
		})
	}
	if r.offset > 0 {
		rows = rows[min(int(r.offset), len(rows)):]
	}
	if r.limit >= 0 && int64(len(rows)) > r.limit {
		rows = rows[:r.limit]
	}
	for _, row := range rows {
		if cont, err := sink(row); err != nil || !cont {
			return err
		}
	}
	return nil
}

// useScanOrder checks whether the first table can be read in ORDER BY order
// (rowid order or the order of an index), adjusting its access path if
// needed. The nested-loop join preserves the order of the first table.
func (p *selectPlan) useScanOrder(st *sql.Select) bool {
	l := p.levels[0]
	if l.virtual != nil {
		return false
	}
	var cols []int // table column positions; -1 = rowid
	var dirs []bool
	for i, o := range st.OrderBy {
		e := o.Expr
		if of := p.orderBy[i]; of.outIdx >= 0 {
			e = p.exprs[of.outIdx]
		}
		ref, ok := e.(*sql.ColumnRef)
		if !ok {
			return false
		}
		idx, err := p.sc.resolve(ref.Table, ref.Name)
		if err != nil || p.sc.cols[idx].level != 0 {
			return false
		}
		col := idx - l.offset
		if col == len(l.t.Columns) || col == l.t.RowidCol || col == l.t.timeColumn() {
			col = -1 // rowid order (a time-series table's time is its rowid order)
		} else if l.t.Columns[col].Type == value.Any {
			return false // mixed types: index order differs from value order
		}
		cols = append(cols, col)
		dirs = append(dirs, o.Desc)
	}
	// uniform reports the common direction of the given ORDER BY items.
	uniform := func(keep func(i int) bool) (desc, ok bool) {
		first := true
		for i, d := range dirs {
			if !keep(i) {
				continue
			}
			if first {
				desc, first = d, false
			} else if d != desc {
				return false, false
			}
		}
		return desc, true
	}
	all := func(int) bool { return true }
	acc := l.acc
	if len(cols) == 1 && cols[0] == -1 {
		desc := dirs[0]
		switch acc.kind {
		case accFull, accRowidRange, accTimeRange:
			acc.reverse = desc
			if l.t.timeColumn() >= 0 {
				acc.desc += " (ordered by time)"
			} else {
				acc.desc += " (ordered by rowid)"
			}
			return true
		case accRowidEq:
			return true
		}
		// An index fully fixed by equality also yields rowid order.
	}
	// Index entries are ordered by (columns..., rowid), so a trailing rowid
	// matches once every index column is used.
	matches := func(ix *Index, from int, want []int) bool {
		for i, c := range want {
			pos := from + i
			switch {
			case c < 0 && pos == len(ix.cols) && i == len(want)-1:
			case pos >= len(ix.cols) || c < 0 || ix.cols[pos] != c:
				return false
			}
		}
		return true
	}
	switch acc.kind {
	case accIndex:
		if acc.in != nil {
			return false
		}
		// Columns fixed by equality are constant and can be skipped.
		eqN := len(acc.prefix)
		var rest []int
		isFixed := map[int]bool{}
		for i, c := range cols {
			for _, ec := range acc.ix.cols[:eqN] {
				isFixed[i] = isFixed[i] || ec == c
			}
			if !isFixed[i] {
				rest = append(rest, c)
			}
		}
		desc, ok := uniform(func(i int) bool { return !isFixed[i] })
		if !ok || !matches(acc.ix, eqN, rest) {
			return false
		}
		acc.reverse = desc
		acc.desc += " (ordered by index)"
		return true
	case accFull:
		// Reading through an index costs a row lookup per entry; it pays off
		// when LIMIT stops the scan early.
		desc, ok := uniform(all)
		if st.Limit == nil || !ok {
			return false
		}
		for _, ix := range l.t.indexes {
			if ix.Where == "" && matches(ix, 0, cols) {
				l.acc = &access{kind: accIndex, ix: ix, reverse: desc,
					desc: fmt.Sprintf("SCAN %s USING INDEX %s (ordered by index)", l.t.Name, ix.Name)}
				return true
			}
		}
	}
	return false
}

// explainAnalyze runs a SELECT and reports its plan with the number of
// scans, rows visited and rows passed at each level, and the run time.
func (x *execCtx) explainAnalyze(s sql.Stmt) (Result, error) {
	st, ok := s.(*sql.Select)
	if !ok {
		return Result{}, errors.New("bangdb: EXPLAIN ANALYZE supports only SELECT")
	}
	plans := x.plans
	x.plans = nil // a private plan: the counters must not be shared
	defer func() { x.plans = plans }()
	r, err := x.prepareSelect(st)
	if err != nil {
		return Result{}, err
	}
	runs := []*selectRun{r}
	if r.parts != nil {
		runs = r.parts
	}
	for _, part := range runs {
		for _, l := range part.p.levels {
			l.stats = &levelStats{}
		}
	}
	start := time.Now()
	n := 0
	if err := x.runSelect(r, func([]any) (bool, error) { n++; return true, nil }); err != nil {
		return Result{}, err
	}
	elapsed := time.Since(start)

	res := Result{Columns: []string{"plan"}}
	add := func(line string) { res.Rows = append(res.Rows, []any{line}) }
	for i, part := range runs {
		if len(runs) > 1 {
			if i > 0 {
				add(r.ops[i-1])
			}
		}
		p := part.p
		if len(p.levels) == 0 {
			add("CONSTANT ROW")
		}
		for d, l := range p.levels {
			line := strings.Repeat("  ", d) + l.acc.desc
			if l.left {
				line += " (LEFT JOIN)"
			}
			add(fmt.Sprintf("%s (loops=%d scanned=%d rows=%d)", line, l.stats.loops, l.stats.scanned, l.stats.rows))
		}
		if p.agg {
			add("AGGREGATE")
		}
		if len(p.wins) > 0 {
			add("WINDOW")
		}
		if len(part.st.OrderBy) > 0 && !p.ordered {
			add("SORT")
		}
	}
	add(fmt.Sprintf("RESULT rows=%d time=%s", n, elapsed.Round(time.Microsecond)))
	return res, nil
}
