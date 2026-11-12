package engine

import (
	"container/heap"
	"fmt"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// This file implements common table expressions (WITH).
//
// A CTE is computed once per statement, the first time a FROM clause names
// it, and then scanned like a derived table. Its body sees the CTEs written
// before it (and those of enclosing statements), but not the row of an
// enclosing query. With WITH RECURSIVE a body may also name itself: it must
// then be "initial SELECT(s) UNION [ALL] recursive SELECT(s)", and is
// computed as in SQLite: the rows of the initial part form a queue; rows
// are taken from the queue, added to the result and fed to the recursive
// part, whose rows join the queue, until it is empty. An ORDER BY on the
// body orders the queue (so it chooses depth- or breadth-first), LIMIT and
// OFFSET bound the result.

// cteScope is the chain of CTEs visible at some point of a statement
// (innermost first).
type cteScope struct {
	name   string // lower case
	def    *cteDef
	parent *cteScope
}

// cteDef is one CTE of the statement being executed.
type cteDef struct {
	name      string
	columns   []string // declared column names, if any
	sel       *sql.Select
	env       *cteScope // the CTEs its body sees
	recursive bool      // declared under WITH RECURSIVE

	done bool
	cols []string
	rows [][]any

	// Set while a recursive CTE is computed.
	running bool
	recPart *sql.Select // the recursive SELECT that may name the CTE
	work    [][]any     // the rows that SELECT sees as the CTE
	levels  []*level    // the FROM levels reading work
}

// cte returns the visible CTE called name, or nil.
func (x *execCtx) cte(name string) *cteDef {
	if x.ctes == nil {
		return nil
	}
	name = strings.ToLower(name)
	for s := x.ctes; s != nil; s = s.parent {
		if s.name == name {
			return s.def
		}
	}
	return nil
}

// pushCTEs makes the CTEs of a WITH clause visible and returns a function
// that hides them again.
func (x *execCtx) pushCTEs(w *sql.With) func() {
	saved := x.ctes
	if w != nil {
		for i := range w.CTEs {
			c := &w.CTEs[i]
			def := &cteDef{name: c.Name, columns: c.Columns, sel: c.Select, recursive: w.Recursive, env: x.ctes}
			x.ctes = &cteScope{name: strings.ToLower(c.Name), def: def, parent: x.ctes}
			if w.Recursive {
				def.env = x.ctes // the body may name itself
			}
		}
	}
	return func() { x.ctes = saved }
}

// cteTable returns the CTE as a virtual table called name, computing it on
// first use.
func (x *execCtx) cteTable(def *cteDef, name string) (*Table, [][]any, error) {
	rows := def.rows
	switch {
	case def.running:
		// A reference from inside its own (recursive) body.
		if def.recPart == nil || x.curSelect != def.recPart {
			return nil, nil, fmt.Errorf("bangdb: recursive reference to %s must be in the FROM clause of a recursive SELECT (not in a subquery)", def.name)
		}
		rows = def.work
	case !def.done:
		if err := x.computeCTE(def); err != nil {
			return nil, nil, err
		}
		rows = def.rows
	}
	t := &Table{Name: name, RowidCol: -1}
	for _, c := range def.cols {
		t.Columns = append(t.Columns, Column{Name: c, Type: value.Any})
	}
	if rows == nil {
		rows = [][]any{}
	}
	return t, rows, nil
}

// computeCTE runs the body of def.
func (x *execCtx) computeCTE(def *cteDef) error {
	x.fkDepth++
	ctes, outer, cur := x.ctes, x.outer, x.curSelect
	x.ctes, x.outer = def.env, nil // the body does not see the enclosing query's row
	defer func() {
		x.fkDepth--
		x.ctes, x.outer, x.curSelect = ctes, outer, cur
	}()
	if x.fkDepth > maxFKDepth {
		return fmt.Errorf("bangdb: WITH table %s is nested too deeply", def.name)
	}
	if def.recursive && selectRefersTo(def.sel, def.name) {
		return x.computeRecursive(def)
	}
	res, err := x.execSelect(def.sel)
	if err != nil {
		return err
	}
	if def.cols, err = def.columnNames(res.Columns); err != nil {
		return err
	}
	def.rows, def.done = res.Rows, true
	return nil
}

// errf is an error about the definition of def.
func (def *cteDef) errf(format string, args ...any) error {
	return fmt.Errorf("bangdb: WITH table %s: %s", def.name, fmt.Sprintf(format, args...))
}

// columnNames applies the declared column names to a body returning cols.
func (def *cteDef) columnNames(cols []string) ([]string, error) {
	if len(def.columns) == 0 {
		return cols, nil
	}
	if len(def.columns) != len(cols) {
		return nil, fmt.Errorf("bangdb: WITH table %s has %d column names but its SELECT returns %d columns", def.name, len(def.columns), len(cols))
	}
	return def.columns, nil
}

// computeRecursive computes a CTE whose body names itself.
func (x *execCtx) computeRecursive(def *cteDef) error {
	body := def.sel
	defer x.pushCTEs(body.With)() // a WITH inside the body
	parts := []*sql.Select{body}
	for _, op := range body.Compound {
		parts = append(parts, op.Select)
	}
	refers := func(i int) bool {
		p := parts[i]
		if i == 0 { // the head without the other parts and the WITH
			head := *body
			head.With, head.Compound, head.OrderBy, head.Limit, head.Offset = nil, nil, nil, nil, nil
			p = &head
		}
		return selectRefersTo(p, def.name)
	}
	first := 0 // the first recursive part
	for i := range parts {
		if refers(i) {
			first = i
			break
		}
	}
	if first == 0 {
		return def.errf("the recursive table needs a non-recursive SELECT first: SELECT ... UNION [ALL] SELECT ... FROM %s", def.name)
	}
	distinct := false
	for i := first; i < len(parts); i++ {
		if !refers(i) {
			return def.errf("a non-recursive SELECT cannot follow a recursive one")
		}
		switch body.Compound[i-1].Op {
		case "UNION":
			distinct = true
		case "UNION ALL":
		default:
			return def.errf("a recursive SELECT must be combined with UNION or UNION ALL, not %s", body.Compound[i-1].Op)
		}
	}

	// The copies of statements made here must not fill the plan cache.
	plans := x.plans
	x.plans = nil
	defer func() { x.plans = plans }()

	initial := *body
	initial.With, initial.Compound = nil, body.Compound[:first-1]
	initial.OrderBy, initial.Limit, initial.Offset = nil, nil, nil
	res, err := x.execSelect(&initial)
	if err != nil {
		return err
	}
	if def.cols, err = def.columnNames(res.Columns); err != nil {
		return err
	}

	limit, offset := int64(-1), int64(0)
	if body.Limit != nil {
		if limit, err = evalInt(body.Limit, x.args, "LIMIT"); err != nil {
			return err
		}
	}
	if body.Offset != nil {
		if offset, err = evalInt(body.Offset, x.args, "OFFSET"); err != nil {
			return err
		}
	}
	var order []compoundOrder
	for _, o := range body.OrderBy {
		col := -1
		switch e := o.Expr.(type) {
		case *sql.Literal:
			if n, ok := e.Value.(int64); ok && n >= 1 && int(n) <= len(def.cols) {
				col = int(n) - 1
			}
		case *sql.ColumnRef:
			for i, name := range def.cols {
				if e.Table == "" && strings.EqualFold(name, e.Name) {
					col = i
					break
				}
			}
		}
		if col < 0 {
			return def.errf("ORDER BY term %s does not match any column of the recursive table", sql.FormatExpr(o.Expr))
		}
		order = append(order, compoundOrder{col, o.Desc})
	}

	// Plan the recursive parts; each reads the CTE through one level, whose
	// rows are replaced before every run.
	def.running = true
	defer func() { def.running, def.recPart, def.work, def.levels = false, nil, nil, nil }()
	var runs []*selectRun
	for _, p := range parts[first:] {
		n := len(def.levels)
		def.recPart = p
		run, err := x.prepareSelect(p)
		if err != nil {
			return err
		}
		if len(run.p.cols) != len(def.cols) {
			return def.errf("the recursive SELECT returns %d columns, the initial one %d", len(run.p.cols), len(def.cols))
		}
		if len(def.levels) != n+1 {
			return def.errf("a recursive SELECT must name %s exactly once in its FROM clause", def.name)
		}
		runs = append(runs, run)
	}
	def.recPart = nil

	var seen map[string]bool
	if distinct {
		seen = map[string]bool{}
	}
	fresh := func(row []any) bool {
		if seen == nil {
			return true
		}
		k := rowKey(row)
		if seen[k] {
			return false
		}
		seen[k] = true
		return true
	}
	var out [][]any
	skipped := int64(0)
	// emit adds a row to the result; false: the LIMIT is reached.
	emit := func(row []any) bool {
		if limit >= 0 && int64(len(out)) >= limit {
			return false
		}
		if skipped < offset {
			skipped++
			return true
		}
		out = append(out, row)
		return limit < 0 || int64(len(out)) < limit
	}
	// step feeds rows to the recursive parts and passes on what they return.
	step := func(rows [][]any, add func(row []any)) error {
		for _, l := range def.levels {
			l.virtual = rows
			if l.acc != nil && l.acc.kind == accHash {
				l.acc.hash = nil // built from the previous rows
			}
		}
		for _, run := range runs {
			err := x.runSelect(run, func(vals []any) (bool, error) {
				if fresh(vals) {
					add(vals)
				}
				return true, nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	finish := func() error {
		def.rows, def.done = out, true
		return nil
	}

	var queue [][]any
	for _, row := range res.Rows {
		if fresh(row) {
			queue = append(queue, row)
		}
	}
	if limit == 0 {
		return finish()
	}
	if len(order) == 0 {
		// First in, first out: a whole generation at a time.
		for len(queue) > 0 {
			for _, row := range queue {
				if !emit(row) {
					return finish()
				}
			}
			var next [][]any
			if err := step(queue, func(row []any) { next = append(next, row) }); err != nil {
				return err
			}
			queue = next
		}
		return finish()
	}
	// Ordered: the smallest row of the queue goes next, one at a time.
	q := &rowQueue{order: order}
	for _, row := range queue {
		q.add(row)
	}
	for q.Len() > 0 {
		row := heap.Pop(q).(queuedRow).row
		if !emit(row) {
			break
		}
		if err := step([][]any{row}, q.add); err != nil {
			return err
		}
	}
	return finish()
}

// rowQueue is the queue of an ordered recursive CTE: a heap by the ORDER
// BY columns, then by arrival.
type rowQueue struct {
	order []compoundOrder
	rows  []queuedRow
	seq   int64
}

type queuedRow struct {
	row []any
	seq int64
}

func (q *rowQueue) add(row []any) {
	q.seq++
	heap.Push(q, queuedRow{row, q.seq})
}

func (q *rowQueue) Len() int      { return len(q.rows) }
func (q *rowQueue) Swap(i, j int) { q.rows[i], q.rows[j] = q.rows[j], q.rows[i] }
func (q *rowQueue) Push(v any)    { q.rows = append(q.rows, v.(queuedRow)) }

func (q *rowQueue) Pop() any {
	n := len(q.rows) - 1
	v := q.rows[n]
	q.rows[n] = queuedRow{}
	q.rows = q.rows[:n]
	return v
}

func (q *rowQueue) Less(i, j int) bool {
	a, b := q.rows[i], q.rows[j]
	for _, o := range q.order {
		if c := value.Compare(a.row[o.col], b.row[o.col]); c != 0 {
			return (c < 0) != o.desc
		}
	}
	return a.seq < b.seq
}

// selectRefersTo reports whether s names the table called name in a FROM
// clause, at any depth.
func selectRefersTo(s *sql.Select, name string) bool {
	if s == nil {
		return false
	}
	if s.With != nil {
		for _, c := range s.With.CTEs {
			if selectRefersTo(c.Select, name) {
				return true
			}
		}
		for _, c := range s.With.CTEs {
			if strings.EqualFold(c.Name, name) {
				return false // hidden by a CTE of the same name
			}
		}
	}
	ref := func(t *sql.TableRef) bool {
		if t.Sub != nil {
			return selectRefersTo(t.Sub, name)
		}
		return strings.EqualFold(t.Name, name)
	}
	if s.From != nil && ref(s.From) {
		return true
	}
	exprs := []sql.Expr{s.Where, s.Having, s.Limit, s.Offset}
	for i := range s.Joins {
		if ref(&s.Joins[i].Table) {
			return true
		}
		exprs = append(exprs, s.Joins[i].On)
	}
	for _, it := range s.Items {
		exprs = append(exprs, it.Expr)
	}
	exprs = append(exprs, s.GroupBy...)
	for _, o := range s.OrderBy {
		exprs = append(exprs, o.Expr)
	}
	for _, e := range exprs {
		found := false
		walk(e, func(n sql.Expr) {
			switch q := n.(type) {
			case *sql.Subquery:
				found = found || selectRefersTo(q.Select, name)
			case *sql.Exists:
				found = found || selectRefersTo(q.Select, name)
			case *sql.In:
				found = found || selectRefersTo(q.Sub, name)
			}
		})
		if found {
			return true
		}
	}
	for _, op := range s.Compound {
		if selectRefersTo(op.Select, name) {
			return true
		}
	}
	return false
}
