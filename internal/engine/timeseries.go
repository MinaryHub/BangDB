package engine

import (
	"strings"

	"github.com/minaryhub/bangdb/internal/sql"
	"github.com/minaryhub/bangdb/internal/store"
)

// deleteByTime runs DELETE on a time-series table as a range deletion
// (whole compressed blocks are dropped) when the WHERE clause only bounds
// the time column. It reports false when the statement needs the general
// row-by-row path.
func (x *execCtx) deleteByTime(t *Table, where sql.Expr) (int64, bool, error) {
	tc := t.timeColumn()
	rd, ok := x.rows(t).(store.RangeDeleter)
	if tc < 0 || !ok || len(t.indexes) > 0 || len(t.referencedBy) > 0 {
		return 0, false, nil
	}
	var lo, hi any
	var loInc, hiInc, hasLo, hasHi bool
	e := &env{args: x.args}
	c := &compiler{x: x}
	for _, cj := range conjuncts(where) {
		b, ok := cj.(*sql.Binary)
		if !ok {
			return 0, false, nil
		}
		op, other := b.Op, b.R
		if !isTableColumn(b.L, t, tc) {
			if !isTableColumn(b.R, t, tc) {
				return 0, false, nil
			}
			op, other = flipOp[b.Op], b.L
		}
		if op == "" || op == "!=" || refsColumns(other) || containsSubquery(other) {
			return 0, false, nil
		}
		f, err := c.compile(other)
		if err != nil {
			return 0, false, nil // not a constant expression: general path
		}
		v, err := f(e)
		if err != nil {
			return 0, false, err
		}
		if v == nil {
			return 0, true, nil // a comparison with NULL matches nothing
		}
		switch op {
		case "=":
			if hasLo || hasHi {
				return 0, false, nil
			}
			lo, hi, loInc, hiInc, hasLo, hasHi = v, v, true, true, true, true
		case ">", ">=":
			if hasLo {
				return 0, false, nil
			}
			lo, loInc, hasLo = v, op == ">=", true
		case "<", "<=":
			if hasHi {
				return 0, false, nil
			}
			hi, hiInc, hasHi = v, op == "<=", true
		default:
			return 0, false, nil
		}
	}
	from, to, ok := x.rows(t).(store.TimeKeyer).RowidRange(lo, loInc, hi, hiInc)
	if !ok {
		return 0, false, nil
	}
	if from > to {
		return 0, true, nil
	}
	n, err := rd.DeleteRange(from, to)
	return n, true, err
}

// isTableColumn reports whether e is a reference to column col of t.
func isTableColumn(e sql.Expr, t *Table, col int) bool {
	r, ok := e.(*sql.ColumnRef)
	return ok && (r.Table == "" || strings.EqualFold(r.Table, t.Name)) && strings.EqualFold(r.Name, t.Columns[col].Name)
}

func refsColumns(e sql.Expr) bool {
	found := false
	walk(e, func(n sql.Expr) {
		if _, ok := n.(*sql.ColumnRef); ok {
			found = true
		}
	})
	return found
}
