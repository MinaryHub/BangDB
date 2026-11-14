package engine

import (
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// Condition push-down into derived tables, views and common table
// expressions.
//
// These are materialized when the FROM clause is planned, so without help
//
//	SELECT des FROM subclass_closure WHERE anc = 7
//
// computes the closure of every class before keeping the rows of class 7.
// When the enclosing query compares a column of such a table with a value
// that is fixed for the statement (col = const, col IN (consts)), the
// comparison is added to the body, applied to the expression that produces
// that column:
//
//   - a plain SELECT gets it in its WHERE (in its HAVING if it groups),
//   - every SELECT of a UNION / INTERSECT / EXCEPT gets it,
//   - a recursive WITH table gets it in its initial SELECT(s) only, and only
//     for a column that every recursive SELECT copies unchanged from the row
//     it extends (like the ancestor column of a closure): every row then
//     carries the value of the seed it grew from, so seeds that fail the
//     condition produce only rows that fail it.
//
// The enclosing query keeps its condition, so the pushed copy only has to
// keep every row the original would keep; it may keep more (a NOCASE column
// compares more loosely than the untyped column of the derived table). The
// body is left alone when the condition could change its meaning: LIMIT or
// OFFSET, window functions, * items, a column that is not a plain
// pass-through of a recursive table, or a value that depends on the row.

// pushdownOff disables push-down (tests compare results with and without).
var pushdownOff bool

// derivedFilter is a condition "column = one of vals" on a derived table.
type derivedFilter struct {
	col  string // lower-case column name of the derived table
	vals []any
	desc string // for EXPLAIN: "anc=?"
}

// derivedFilters returns the conditions of the enclosing query that compare
// a column of the derived table at FROM position i (called alias) with
// statement constants: the WHERE conjuncts and the table's own ON
// conjuncts.
func (x *execCtx) derivedFilters(from []sql.TableRef, i int, ons []sql.Expr, where sql.Expr, alias string) []derivedFilter {
	if pushdownOff {
		return nil
	}
	conds := conjuncts(where)
	if i < len(ons) {
		conds = append(conds, conjuncts(ons[i])...)
	}
	var out []derivedFilter
	for _, cj := range conds {
		var col *sql.ColumnRef
		var valExprs []sql.Expr
		switch e := cj.(type) {
		case *sql.Binary:
			if e.Op != "=" && e.Op != "==" {
				continue
			}
			if c, ok := e.L.(*sql.ColumnRef); ok && statementConstant(e.R) {
				col, valExprs = c, []sql.Expr{e.R}
			} else if c, ok := e.R.(*sql.ColumnRef); ok && statementConstant(e.L) {
				col, valExprs = c, []sql.Expr{e.L}
			}
		case *sql.In:
			if c, ok := e.X.(*sql.ColumnRef); ok && !e.Not && e.Sub == nil && len(e.List) > 0 {
				col, valExprs = c, e.List
				for _, v := range e.List {
					if !statementConstant(v) {
						col = nil
						break
					}
				}
			}
		}
		if col == nil {
			continue
		}
		switch {
		case col.Table != "" && !strings.EqualFold(col.Table, alias):
			continue
		case col.Table == "" && len(from) != 1:
			continue // might name a column of another table
		}
		vals := make([]any, 0, len(valExprs))
		ok := true
		for _, ve := range valExprs {
			f, err := (&compiler{}).compile(ve)
			if err != nil {
				ok = false
				break
			}
			v, err := f(&env{args: x.args})
			if err != nil {
				ok = false
				break
			}
			vals = append(vals, v)
		}
		if !ok {
			continue
		}
		desc := col.Name + "=?"
		if len(vals) > 1 {
			desc = col.Name + " IN (...)"
		}
		out = append(out, derivedFilter{col: strings.ToLower(col.Name), vals: vals, desc: desc})
	}
	return out
}

// statementConstant reports whether e has the same value for every row of
// the statement: literals, parameters and arithmetic on them (no columns,
// subqueries, function calls or collations).
func statementConstant(e sql.Expr) bool {
	ok := true
	walk(e, func(n sql.Expr) {
		switch n.(type) {
		case *sql.Literal, *sql.Param, *sql.Unary, *sql.Binary:
		default:
			ok = false
		}
	})
	if b, isBin := e.(*sql.Binary); isBin {
		switch b.Op {
		case "+", "-", "*", "/", "%", "||":
		default:
			return false
		}
	}
	return ok
}

// filterExpr is "e = v" or "e IN (vs)".
func (f derivedFilter) on(e sql.Expr) sql.Expr {
	if len(f.vals) == 1 {
		return &sql.Binary{Op: "=", L: e, R: &sql.Literal{Value: f.vals[0]}}
	}
	list := make([]sql.Expr, len(f.vals))
	for i, v := range f.vals {
		list[i] = &sql.Literal{Value: v}
	}
	return &sql.In{X: e, List: list}
}

func and(a, b sql.Expr) sql.Expr {
	if a == nil {
		return b
	}
	return &sql.Binary{Op: "AND", L: a, R: b}
}

// outputNames are the column names of sel's result (declared names win),
// or nil if they cannot be told without running it (a * item).
func outputNames(sel *sql.Select, declared []string) []string {
	if len(declared) > 0 {
		return declared
	}
	names := make([]string, len(sel.Items))
	for i, it := range sel.Items {
		if it.Star {
			return nil
		}
		names[i] = outputName(it)
	}
	return names
}

// columnIndex finds the position of a filter's column in names (-1: not
// there or not unique).
func columnIndex(names []string, col string) int {
	k := -1
	for i, n := range names {
		if strings.EqualFold(n, col) {
			if k >= 0 {
				return -1
			}
			k = i
		}
	}
	return k
}

// pushIntoPart returns a copy of the single SELECT p (without its compound
// parts' handling) with "item k satisfies f" added, or false if that could
// change what p returns.
func pushIntoPart(p *sql.Select, k int, f derivedFilter) (*sql.Select, bool) {
	if k >= len(p.Items) {
		return nil, false
	}
	grouped := len(p.GroupBy) > 0
	for _, it := range p.Items {
		if it.Star || hasWindow(it.Expr) {
			return nil, false
		}
		if hasAggregate(it.Expr) && !grouped {
			return nil, false // one-row aggregate: a filter would change it
		}
	}
	cp := *p
	if grouped {
		cp.Having = and(cp.Having, f.on(p.Items[k].Expr))
	} else {
		cp.Where = and(cp.Where, f.on(p.Items[k].Expr))
	}
	return &cp, true
}

// pushIntoSelect applies filters to a (possibly compound) SELECT whose
// result columns are names. It returns the new SELECT and the filters it
// took, or nil if nothing could be pushed.
func pushIntoSelect(sel *sql.Select, names []string, filters []derivedFilter) (*sql.Select, []derivedFilter) {
	if sel.Limit != nil || sel.Offset != nil || names == nil {
		return nil, nil
	}
	out := sel
	var pushed []derivedFilter
	for _, f := range filters {
		k := columnIndex(names, f.col)
		if k < 0 {
			continue
		}
		head, ok := pushIntoPart(out, k, f)
		if !ok {
			continue
		}
		parts := make([]sql.SetOp, len(out.Compound))
		for i, op := range out.Compound {
			if op.Select.Limit != nil || op.Select.Offset != nil {
				ok = false
				break
			}
			p, partOK := pushIntoPart(op.Select, k, f)
			if !partOK {
				ok = false
				break
			}
			parts[i] = sql.SetOp{Op: op.Op, Select: p}
		}
		if !ok {
			continue
		}
		if len(parts) > 0 {
			head.Compound = parts
		}
		out, pushed = head, append(pushed, f)
	}
	if pushed == nil {
		return nil, nil
	}
	return out, pushed
}

// pushIntoRecursive applies filters to the body of the recursive WITH table
// def, like pushIntoSelect.
func (x *execCtx) pushIntoRecursive(def *cteDef, filters []derivedFilter) (*sql.Select, []derivedFilter) {
	body := def.sel
	if body.Limit != nil || body.Offset != nil {
		return nil, nil
	}
	parts := []*sql.Select{body}
	for _, op := range body.Compound {
		parts = append(parts, op.Select)
	}
	first := -1
	for i, p := range parts {
		if i == 0 {
			head := *body
			head.With, head.Compound, head.OrderBy = nil, nil, nil
			p = &head
		}
		if selectRefersTo(p, def.name) {
			first = i
			break
		}
	}
	if first <= 0 {
		return nil, nil // not recursive after all, or malformed (computeRecursive reports it)
	}
	names := outputNames(body, def.columns)
	if names == nil {
		return nil, nil
	}
	out := body
	var pushed []derivedFilter
	for _, f := range filters {
		k := columnIndex(names, f.col)
		if k < 0 {
			continue
		}
		ok := true
		for _, p := range parts[first:] {
			if !x.passesThrough(p, def.name, names[k], k) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		head, headOK := pushIntoPart(out, k, f)
		if !headOK {
			continue
		}
		comp := make([]sql.SetOp, len(out.Compound))
		copy(comp, out.Compound)
		for i := 1; i < first; i++ {
			p, partOK := pushIntoPart(comp[i-1].Select, k, f)
			if !partOK {
				ok = false
				break
			}
			comp[i-1].Select = p
		}
		if !ok {
			continue
		}
		head.Compound = comp
		out, pushed = head, append(pushed, f)
	}
	if pushed == nil {
		return nil, nil
	}
	return out, pushed
}

// passesThrough reports whether item k of the recursive SELECT p is the
// column k (called name) of the row of table cte it extends.
func (x *execCtx) passesThrough(p *sql.Select, cte, name string, k int) bool {
	if k >= len(p.Items) || p.Items[k].Star {
		return false
	}
	ref, ok := p.Items[k].Expr.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(ref.Name, name) {
		return false
	}
	refs := []*sql.TableRef{}
	if p.From != nil {
		refs = append(refs, p.From)
	}
	for i := range p.Joins {
		refs = append(refs, &p.Joins[i].Table)
	}
	self := ""
	for _, r := range refs {
		if r.Sub == nil && !r.Func && strings.EqualFold(r.Name, cte) {
			self = r.Alias
			if self == "" {
				self = r.Name
			}
		}
	}
	if self == "" {
		return false
	}
	if ref.Table != "" {
		return strings.EqualFold(ref.Table, self)
	}
	// Unqualified: no other table of the SELECT may have a column of that name.
	for _, r := range refs {
		if r.Sub == nil && !r.Func && strings.EqualFold(r.Name, cte) {
			continue
		}
		if r.Sub != nil || r.Func || x.cte(r.Name) != nil {
			return false
		}
		t := x.cat.table(r.Name)
		if t == nil {
			return false
		}
		for _, c := range t.Columns {
			if strings.EqualFold(c.Name, name) {
				return false
			}
		}
	}
	return true
}

// filteredCTE computes the WITH table def with filters pushed into its
// body, without caching the result in def. pushed is nil if nothing could be
// pushed (then the caller computes def as usual).
func (x *execCtx) filteredCTE(def *cteDef, name string, filters []derivedFilter) (*Table, [][]any, []derivedFilter, error) {
	if def.done || def.running || len(filters) == 0 {
		return nil, nil, nil, nil
	}
	var sel *sql.Select
	var pushed []derivedFilter
	if def.recursive && selectRefersTo(def.sel, def.name) {
		sel, pushed = x.pushIntoRecursive(def, filters)
	} else {
		sel, pushed = pushIntoSelect(def.sel, outputNames(def.sel, def.columns), filters)
	}
	if sel == nil {
		return nil, nil, nil, nil
	}
	tmp := &cteDef{name: def.name, columns: def.columns, sel: sel, recursive: def.recursive, env: def.env}
	if def.recursive && def.env != nil && def.env.def == def {
		// the body names itself: let that name find the filtered copy
		tmp.env = &cteScope{name: def.env.name, def: tmp, parent: def.env.parent}
	}
	plans := x.plans // the copies must not fill the plan cache
	x.plans = nil
	err := x.computeCTE(tmp)
	x.plans = plans
	if err != nil {
		return nil, nil, nil, err
	}
	t := &Table{Name: name, RowidCol: -1}
	for _, c := range tmp.cols {
		t.Columns = append(t.Columns, Column{Name: c, Type: value.Any})
	}
	rows := tmp.rows
	if rows == nil {
		rows = [][]any{}
	}
	return t, rows, pushed, nil
}

// execPushed runs sel with filters pushed into it when possible; pushed
// lists the ones that were.
func (x *execCtx) execPushed(sel *sql.Select, declared []string, filters []derivedFilter) (Result, []derivedFilter, error) {
	if len(filters) > 0 {
		if cp, pushed := pushIntoSelect(sel, outputNames(sel, declared), filters); cp != nil {
			plans := x.plans
			x.plans = nil
			res, err := x.execSelect(cp)
			x.plans = plans
			return res, pushed, err
		}
	}
	res, err := x.execSelect(sel)
	return res, nil, err
}

// pushedDesc describes pushed filters for EXPLAIN.
func pushedDesc(filters []derivedFilter) string {
	if len(filters) == 0 {
		return ""
	}
	parts := make([]string, len(filters))
	for i, f := range filters {
		parts[i] = f.desc
	}
	return " (pushed " + strings.Join(parts, " AND ") + ")"
}
