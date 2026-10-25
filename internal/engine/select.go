package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/sql"
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
	items   []evalFn
	orderBy []orderFn
	agg     bool
	aggs    []*aggSpec
	groupBy []evalFn
	having  evalFn
}

type orderFn struct {
	outIdx int // >= 0: sort by output column
	fn     evalFn
	desc   bool
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
	p.sc, p.levels, p.final, err = x.buildLevels(from, lefts, ons, st.Where)
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
	c := &compiler{x: x, sc: p.sc, allowAgg: p.agg, aggs: &p.aggs}

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
		if of.outIdx < 0 {
			f, err := c.compile(o.Expr)
			if err != nil {
				return nil, err
			}
			of.fn = f
		}
		p.orderBy = append(p.orderBy, of)
	}

	if p.agg {
		gc := &compiler{x: x, sc: p.sc}
		for _, g := range st.GroupBy {
			if hasAggregate(g) {
				return nil, errors.New("bangdb: aggregate functions are not allowed in GROUP BY")
			}
			f, err := gc.compile(g)
			if err != nil {
				return nil, err
			}
			p.groupBy = append(p.groupBy, f)
		}
		if st.Having != nil {
			if p.having, err = c.compile(st.Having); err != nil {
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
	return Result{Columns: r.p.cols, Rows: rows}, nil
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

func (x *execCtx) prepareSelect(st *sql.Select) (*selectRun, error) {
	if len(st.Compound) > 0 {
		return x.prepareCompound(st)
	}
	p, err := x.planSelect(st)
	if err != nil {
		return nil, err
	}
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
	e := &env{row: make([]any, len(p.sc.cols)), args: x.args}
	streaming := !p.agg && len(p.orderBy) == 0 && !st.Distinct
	var out []outRow
	sortOut := func() {
		sort.SliceStable(out, func(i, j int) bool {
			for k, o := range p.orderBy {
				c := value.Compare(out[i].keys[k], out[j].keys[k])
				if c != 0 {
					return (c < 0) != o.desc
				}
			}
			return false
		})
	}
	topK := int64(-1)
	if len(p.orderBy) > 0 && limit >= 0 && !st.Distinct {
		topK = offset + limit
	}
	emitted, skipped := int64(0), int64(0)
	stopped := false
	project := func() error {
		vals := make([]any, len(p.items))
		for i, f := range p.items {
			v, err := f(e)
			if err != nil {
				return err
			}
			vals[i] = v
		}
		if streaming {
			cont, err := sink(vals)
			emitted++
			stopped = stopped || !cont
			return err
		}
		row := outRow{vals: vals}
		if len(p.orderBy) > 0 {
			row.keys = make([]any, len(p.orderBy))
			for i, o := range p.orderBy {
				if o.outIdx >= 0 {
					row.keys[i] = vals[o.outIdx]
					continue
				}
				v, err := o.fn(e)
				if err != nil {
					return err
				}
				row.keys[i] = v
			}
		}
		out = append(out, row)
		if topK >= 0 && int64(len(out)) >= 2*topK+1024 {
			sortOut()
			out = out[:topK]
		}
		return nil
	}

	if !p.agg {
		emit := func() (bool, error) {
			if streaming && skipped < offset {
				skipped++
				return true, nil
			}
			if streaming && limit >= 0 && emitted >= limit {
				return false, nil
			}
			if err := project(); err != nil {
				return false, err
			}
			return !stopped && !(streaming && limit >= 0 && emitted >= limit), nil
		}
		if len(p.levels) == 0 {
			if ok, err := passes(p.final, e); err != nil {
				return err
			} else if ok {
				if _, err := emit(); err != nil {
					return err
				}
			}
		} else if limit != 0 || !streaming {
			if err := x.runJoin(p.levels, p.final, e, emit); err != nil {
				return err
			}
		}
	} else if err := x.runGrouped(p, e, project); err != nil {
		return err
	}
	if streaming {
		return nil
	}

	if st.Distinct {
		seen := map[string]bool{}
		uniq := out[:0]
		for _, r := range out {
			var k []byte
			for _, v := range r.vals {
				k = value.AppendKey(k, v)
			}
			if !seen[string(k)] {
				seen[string(k)] = true
				uniq = append(uniq, r)
			}
		}
		out = uniq
	}
	if len(p.orderBy) > 0 {
		sortOut()
	}
	if offset > 0 {
		out = out[min(int(offset), len(out)):]
	}
	if limit >= 0 && int64(len(out)) > limit {
		out = out[:limit]
	}
	for _, r := range out {
		if cont, err := sink(r.vals); err != nil || !cont {
			return err
		}
	}
	return nil
}

type group struct {
	row    []any
	states []*aggState
}

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
			g = newGroup()
			groups[string(key)] = g
			order = append(order, g)
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
	for _, g := range order {
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
				continue
			}
		}
		if err := project(); err != nil {
			return err
		}
	}
	return nil
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
			if len(st.OrderBy) > 0 {
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
		cols = append(cols, scopeCol{table: strings.ToLower(alias), name: strings.ToLower(c.Name), level: level, hidden: qualifiedOnly})
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
		prefix := ix.key(row)
		c := btree.Open(x.v, ix.Root).Seek(prefix)
		if c.Valid() && bytes.HasPrefix(c.Key(), prefix) {
			out = append(out, conflict{value.DecodeRowID(c.Key()[len(prefix):]), ix.Columns})
		}
		if err := c.Err(); err != nil {
			return nil, err
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
		c := &compiler{x: x}
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
						if err := x.deleteRow(t, cf.rowid, old); err != nil {
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
				for _, s := range up.sets {
					if newRow[s.col], err = s.fn(up.e); err != nil {
						return Result{}, err
					}
				}
				id, err := x.updateRow(t, hit.rowid, old, newRow)
				if err != nil {
					return Result{}, err
				}
				res.RowsAffected++
				if err := ret.add(&res, id, newRow); err != nil {
					return Result{}, err
				}
				continue
			}
		}
		id, err := x.insertRow(t, row)
		if err != nil {
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
	_, targets, err := x.collect(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	res := Result{RowsAffected: int64(len(targets))}
	if ret != nil {
		res.Columns = ret.cols
	}
	for _, tg := range targets {
		if err := ret.add(&res, tg.rowid, tg.row); err != nil {
			return Result{}, err
		}
		if err := x.deleteRow(t, tg.rowid, tg.row); err != nil {
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
	for _, tg := range targets {
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
		id, err := x.updateRow(t, tg.rowid, tg.row, newRow)
		if err != nil {
			return Result{}, err
		}
		if err := ret.add(&res, id, newRow); err != nil {
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
	if newID != oldID {
		if _, exists, err := x.getRow(t, newID); err != nil {
			return 0, err
		} else if exists {
			return 0, constraintErr("UNIQUE constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
		}
	}
	for _, ix := range t.indexes {
		if newID == oldID && bytes.Equal(ix.key(old), ix.key(newRow)) {
			continue
		}
		if err := x.checkUnique(t, ix, newRow, oldID); err != nil {
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
	return newID, x.recordSequence(t, newID)
}

// writeRow stores a row and its index entries without constraint checks.
func (x *execCtx) writeRow(t *Table, rowid int64, row []any) error {
	for _, ix := range t.indexes {
		k, err := indexEntry(ix, row, rowid)
		if err != nil {
			return err
		}
		if err := btree.Open(x.v, ix.Root).Put(k, nil); err != nil {
			return err
		}
	}
	return btree.Open(x.v, t.Root).Put(value.EncodeRowID(rowid), value.EncodeRow(row))
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
	r := &selectRun{st: st, p: &selectPlan{cols: first.p.cols}, limit: -1, parts: []*selectRun{first}}
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
