package engine

import (
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
	c := &compiler{sc: p.sc, allowAgg: p.agg, aggs: &p.aggs}

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
		switch {
		case it.Alias != "":
			p.cols = append(p.cols, it.Alias)
		case isColumn(it.Expr):
			p.cols = append(p.cols, it.Expr.(*sql.ColumnRef).Name)
		default:
			name := sql.FormatExpr(it.Expr)
			if _, isBin := it.Expr.(*sql.Binary); isBin {
				name = name[1 : len(name)-1]
			}
			p.cols = append(p.cols, name)
		}
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
		gc := &compiler{sc: p.sc}
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
	p, err := x.planSelect(st)
	if err != nil {
		return Result{}, err
	}
	limit, offset := int64(-1), int64(0)
	if st.Limit != nil {
		if limit, err = evalInt(st.Limit, x.args, "LIMIT"); err != nil {
			return Result{}, err
		}
	}
	if st.Offset != nil {
		if offset, err = evalInt(st.Offset, x.args, "OFFSET"); err != nil {
			return Result{}, err
		}
		offset = max(offset, 0)
	}

	e := &env{row: make([]any, len(p.sc.cols)), args: x.args}
	var out []outRow
	project := func() error {
		r := outRow{vals: make([]any, len(p.items))}
		for i, f := range p.items {
			v, err := f(e)
			if err != nil {
				return err
			}
			r.vals[i] = v
		}
		if len(p.orderBy) > 0 {
			r.keys = make([]any, len(p.orderBy))
			for i, o := range p.orderBy {
				if o.outIdx >= 0 {
					r.keys[i] = r.vals[o.outIdx]
					continue
				}
				v, err := o.fn(e)
				if err != nil {
					return err
				}
				r.keys[i] = v
			}
		}
		out = append(out, r)
		return nil
	}
	// Without sorting, grouping or DISTINCT we can stop as soon as LIMIT is met.
	streaming := !p.agg && len(p.orderBy) == 0 && !st.Distinct
	skipped := int64(0)

	if !p.agg {
		emit := func() (bool, error) {
			if streaming && skipped < offset {
				skipped++
				return true, nil
			}
			if streaming && limit >= 0 && int64(len(out)) >= limit {
				return false, nil
			}
			if err := project(); err != nil {
				return false, err
			}
			return !(streaming && limit >= 0 && int64(len(out)) >= limit), nil
		}
		if len(p.levels) == 0 {
			if ok, err := passes(p.final, e); err != nil {
				return Result{}, err
			} else if ok {
				if _, err := emit(); err != nil {
					return Result{}, err
				}
			}
		} else if limit != 0 || !streaming {
			if err := x.runJoin(p.levels, p.final, e, emit); err != nil {
				return Result{}, err
			}
		}
	} else {
		if err := x.runGrouped(p, e, project); err != nil {
			return Result{}, err
		}
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
	if !streaming {
		if offset > 0 {
			out = out[min(int(offset), len(out)):]
		}
		if limit >= 0 && int64(len(out)) > limit {
			out = out[:limit]
		}
	}
	res := Result{Columns: p.cols, Rows: make([][]any, len(out))}
	for i, r := range out {
		res.Rows[i] = r.vals
	}
	return res, nil
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
	c := &compiler{}
	e := &env{args: x.args}
	var res Result
	for _, exprs := range st.Rows {
		if len(exprs) != len(positions) {
			return Result{}, fmt.Errorf("bangdb: %d values for %d columns", len(exprs), len(positions))
		}
		row := make([]any, len(t.Columns))
		given := make([]bool, len(t.Columns))
		for j, ex := range exprs {
			f, err := c.compile(ex)
			if err != nil {
				return Result{}, err
			}
			v, err := f(e)
			if err != nil {
				return Result{}, err
			}
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
		id, err := x.insertRow(t, row)
		if err != nil {
			return Result{}, err
		}
		res.RowsAffected++
		res.LastInsertID = id
	}
	return res, nil
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
	_, targets, err := x.collect(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	for _, tg := range targets {
		if err := x.deleteRow(t, tg.rowid, tg.row); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: int64(len(targets))}, nil
}

func (x *execCtx) execUpdate(st *sql.Update) (Result, error) {
	t, err := x.table(st.Table)
	if err != nil {
		return Result{}, err
	}
	sc, targets, err := x.collect(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	c := &compiler{sc: sc}
	type set struct {
		col int
		fn  evalFn
	}
	var sets []set
	for _, a := range st.Sets {
		i := t.ColIndex(a.Column)
		if i < 0 {
			return Result{}, fmt.Errorf("bangdb: no such column: %s", a.Column)
		}
		f, err := c.compile(a.Value)
		if err != nil {
			return Result{}, err
		}
		sets = append(sets, set{i, f})
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
		if err := prepareRow(t, newRow); err != nil {
			return Result{}, err
		}
		newID := tg.rowid
		if t.RowidCol >= 0 {
			if newRow[t.RowidCol] == nil {
				return Result{}, constraintErr("NOT NULL constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
			}
			newID = newRow[t.RowidCol].(int64)
		}
		if newID != tg.rowid {
			if _, exists, err := x.getRow(t, newID); err != nil {
				return Result{}, err
			} else if exists {
				return Result{}, constraintErr("UNIQUE constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
			}
		}
		for _, ix := range t.indexes {
			if newID == tg.rowid && value.Compare(tg.row[ix.col], newRow[ix.col]) == 0 &&
				value.TypeOf(tg.row[ix.col]) == value.TypeOf(newRow[ix.col]) {
				continue
			}
			if err := x.checkUnique(t, ix, newRow[ix.col], tg.rowid); err != nil {
				return Result{}, err
			}
		}
		if err := x.deleteRow(t, tg.rowid, tg.row); err != nil {
			return Result{}, err
		}
		if t.RowidCol >= 0 {
			newRow[t.RowidCol] = newID
		}
		if err := x.writeRow(t, newID, newRow); err != nil {
			return Result{}, err
		}
		x.noteRowid(t, newID)
	}
	return Result{RowsAffected: int64(len(targets))}, nil
}

// writeRow stores a row and its index entries without constraint checks.
func (x *execCtx) writeRow(t *Table, rowid int64, row []any) error {
	for _, ix := range t.indexes {
		k, err := indexEntry(row[ix.col], rowid)
		if err != nil {
			return err
		}
		if err := btree.Open(x.v, ix.Root).Put(k, nil); err != nil {
			return err
		}
	}
	return btree.Open(x.v, t.Root).Put(value.EncodeRowID(rowid), value.EncodeRow(row))
}
