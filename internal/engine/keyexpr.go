package engine

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// This file implements expressions stored with a table: the key parts of
// expression indexes (CREATE INDEX ... ON t (lower(email))) and generated
// columns (c TEXT AS (lower(email))).
//
// Such an expression uses only the columns of its row and deterministic
// functions, so its value changes only when the row does. It is kept in the
// catalog as SQL text with the table's own column names; a query condition
// uses an expression index when one side of it, written the same way, is
// the indexed expression.
//
// The type of an index expression decides how its values are keyed. Where
// it is known (lower(x) is text, length(x) an integer, CAST(x AS t) is t)
// the part behaves like a column of that type. Otherwise the values are
// keyed by their own type, with whole numbers as integers, and the index
// serves equality and IN lookups but not ranges or ORDER BY.

// typeExprAny is the key type of an index expression of unknown type.
const typeExprAny value.Type = "\x00EXPR"

// keyExpr is an index expression of a table.
type keyExpr struct {
	text string
	typ  value.Type
	fn   evalFn
}

// keyExprID is the pseudo column number of the k-th index expression of a
// table (column numbers are >= 0 and the rowid is -1).
func keyExprID(k int) int { return -2 - k }

// keyExpr returns the table's index expression with the given text and its
// pseudo column number, compiling it on first use.
func (t *Table) keyExpr(text string) (*keyExpr, int) {
	for k, ke := range t.keyExprs {
		if ke.text == text {
			return ke, keyExprID(k)
		}
	}
	ke := &keyExpr{text: text, typ: typeExprAny}
	e, err := sql.ParseExpr(text)
	if err == nil {
		ke.fn, err = t.compileStored(e)
	}
	if err != nil {
		err := fmt.Errorf("bangdb: index expression %s of %s: %w", text, t.Name, err)
		ke.fn = func(*env) (any, error) { return nil, err }
	} else {
		ke.typ = exprType(e, t)
	}
	t.keyExprs = append(t.keyExprs, ke)
	return ke, keyExprID(len(t.keyExprs) - 1)
}

// compileStored compiles an expression over the columns of t (a row of
// len(t.Columns) values).
func (t *Table) compileStored(e sql.Expr) (evalFn, error) {
	if err := validateStored(e); err != nil {
		return nil, err
	}
	sc := &scope{}
	for _, c := range t.Columns {
		sc.cols = append(sc.cols, scopeCol{table: strings.ToLower(t.Name), name: strings.ToLower(c.Name), coll: c.Collate})
	}
	return (&compiler{sc: sc}).compile(e)
}

// nondeterministic lists the functions whose result does not depend on
// their arguments alone.
var nondeterministic = map[string]bool{"CURRENT_TIMESTAMP": true, "CURRENT_DATE": true, "CURRENT_TIME": true, "NOW": true,
	"RANDOM": true, "RAISE": true, "LAST_INSERT_ROWID": true, "CHANGES": true}

var dateFuncs = map[string]bool{"DATE": true, "TIME": true, "DATETIME": true, "JULIANDAY": true, "UNIXEPOCH": true, "STRFTIME": true}

// validateStored checks that e may be stored with a table: no subqueries,
// aggregates, window functions or parameters, and nothing that depends on
// the time it is evaluated.
func validateStored(e sql.Expr) error {
	if hasAggregate(e) || containsSubquery(e) {
		return errors.New("subqueries and aggregate functions are not allowed here")
	}
	var err error
	walk(e, func(n sql.Expr) {
		switch x := n.(type) {
		case *sql.Param:
			err = errors.New("parameters are not allowed here")
		case *sql.Func:
			switch {
			case x.Over != nil:
				err = errors.New("window functions are not allowed here")
			case nondeterministic[x.Name]:
				err = fmt.Errorf("%s is not deterministic", x.Name)
			case dateFuncs[x.Name]:
				timeArgs := len(x.Args)
				if x.Name == "STRFTIME" {
					timeArgs--
				}
				if timeArgs <= 0 {
					err = fmt.Errorf("%s() of the current time is not deterministic", x.Name)
				}
				for _, a := range x.Args {
					if lit, ok := a.(*sql.Literal); ok {
						switch s, _ := lit.Value.(string); strings.ToLower(strings.TrimSpace(s)) {
						case "now", "localtime", "utc":
							err = fmt.Errorf("%s(... '%s') is not deterministic", x.Name, s)
						}
					}
				}
			}
		}
	})
	return err
}

// storedText renders e, an expression over the columns of t, in the form
// kept in the catalog: column references without table name, spelled as
// the columns are declared.
func (t *Table) storedText(e sql.Expr) (string, error) {
	var err error
	text := sql.FormatExprCols(e, func(r *sql.ColumnRef) string {
		i := t.ColIndex(r.Name)
		if i < 0 || r.Table != "" && !strings.EqualFold(r.Table, t.Name) {
			if err == nil {
				err = fmt.Errorf("no such column: %s", sql.FormatExpr(r))
			}
			return sql.FormatExpr(r)
		}
		return sql.QuoteIdent(t.Columns[i].Name)
	})
	return text, err
}

// keyExprOf returns the pseudo column of the index expression of level l's
// table that e is, if any.
func keyExprOf(e sql.Expr, sc *scope, l *level, li int) (int, bool) {
	if len(l.t.keyExprs) == 0 {
		return 0, false
	}
	switch e.(type) {
	case *sql.ColumnRef, *sql.Literal, *sql.Param:
		return 0, false
	}
	ok := true
	walk(e, func(n sql.Expr) {
		switch n.(type) {
		case *sql.Subquery, *sql.Exists, *sql.Param:
			ok = false
		}
	})
	if !ok {
		return 0, false
	}
	text := sql.FormatExprCols(e, func(r *sql.ColumnRef) string {
		i, err := sc.resolve(r.Table, r.Name)
		if err != nil || sc.cols[i].level != li || i-l.offset >= len(l.t.Columns) {
			ok = false
			return ""
		}
		return sql.QuoteIdent(l.t.Columns[i-l.offset].Name)
	})
	if !ok {
		return 0, false
	}
	for k, ke := range l.t.keyExprs {
		if ke.text == text {
			return keyExprID(k), true
		}
	}
	return 0, false
}

// exprType infers the type of the values of e (an expression over the
// columns of t), or typeExprAny.
func exprType(e sql.Expr, t *Table) value.Type {
	switch x := e.(type) {
	case *sql.ColumnRef:
		if i := t.ColIndex(x.Name); i >= 0 && t.Columns[i].Type != value.Any && t.Columns[i].Collate == "" {
			return t.Columns[i].Type
		}
	case *sql.Collate:
		return exprType(x.X, t)
	case *sql.Literal:
		switch x.Value.(type) {
		case int64:
			return value.Integer
		case float64:
			return value.Real
		case string:
			return value.Text
		case []byte:
			return value.Blob
		}
	case *sql.IsNull, *sql.In, *sql.Between, *sql.Like:
		return value.Integer
	case *sql.Unary:
		if x.Op == "NOT" {
			return value.Integer
		}
	case *sql.Binary:
		switch x.Op {
		case "=", "!=", "<", "<=", ">", ">=", "AND", "OR":
			return value.Integer
		case "||":
			return value.Text
		}
	case *sql.Func:
		switch x.Name {
		case "CAST":
			if name, ok := x.Args[1].(*sql.Literal).Value.(string); ok {
				if typ := value.ParseType(name); typ != value.Any {
					return typ
				}
			}
		case "LOWER", "UPPER", "TRIM", "LTRIM", "RTRIM", "REPLACE", "SUBSTR", "SUBSTRING", "TYPEOF",
			"DATE", "TIME", "DATETIME", "STRFTIME", "HEX":
			return value.Text
		case "LENGTH", "INSTR", "UNIXEPOCH", "UNICODE":
			return value.Integer
		case "JULIANDAY":
			return value.Real
		}
	}
	return typeExprAny
}

// normKeyValue is the form in which a value of an index expression of
// unknown type is keyed: numbers that compare equal get the same key.
func normKeyValue(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && x >= math.MinInt64 && x < math.MaxInt64 {
			return int64(x)
		}
	case value.Decimal:
		if d, err := x.Rescale(0); err == nil && d.Cmp(x) == 0 {
			return d.Unscaled
		}
		return x.Float64()
	}
	return v
}

// eval computes the expression for a row of t.
func (ke *keyExpr) eval(t *Table, row []any) (any, error) {
	if len(row) < len(t.Columns) {
		row = padRow(t, append([]any(nil), row...))
	}
	v, err := ke.fn(&env{row: row})
	if err != nil || v == nil {
		return nil, err
	}
	if ke.typ == typeExprAny {
		return normKeyValue(v), nil
	}
	return value.Coerce(v, ke.typ)
}

// hasExpr reports whether ix has an expression among its key parts.
func (ix *Index) hasExpr() bool {
	for _, e := range ix.Exprs {
		if e != "" {
			return true
		}
	}
	return false
}

// value returns the value of key part i for row. An expression that fails
// yields NULL (rows are checked with checkExprs before they are stored).
func (ix *Index) value(i int, row []any) any {
	if c := ix.cols[i]; c >= 0 {
		return row[c]
	}
	if ke := ix.exprs[i]; ke != nil {
		v, _ := ke.eval(ix.table, row)
		return v
	}
	return nil
}

// checkExprs fails if an index expression cannot be computed for row.
func (ix *Index) checkExprs(row []any) error {
	for _, ke := range ix.exprs {
		if ke == nil {
			continue
		}
		if _, err := ke.eval(ix.table, row); err != nil {
			if strings.HasPrefix(err.Error(), "bangdb: index expression") {
				return err
			}
			return fmt.Errorf("bangdb: index %s: expression %s: %w", ix.Name, ke.text, err)
		}
	}
	return nil
}

// keyPartList renders the key parts of ix as in CREATE INDEX.
func (ix *Index) keyPartList() string {
	parts := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		if i < len(ix.Exprs) && ix.Exprs[i] != "" {
			parts[i] = ix.Exprs[i]
		} else {
			parts[i] = sql.QuoteIdent(c)
		}
	}
	return strings.Join(parts, ", ")
}

// ---- generated columns ----

// compileGenerated compiles the expressions of t's generated columns.
func (t *Table) compileGenerated() {
	t.hasGen = false
	for i := range t.Columns {
		c := &t.Columns[i]
		c.genFn = nil
		if c.Generated == "" {
			continue
		}
		t.hasGen = true
		e, err := sql.ParseExpr(c.Generated)
		if err == nil {
			err = t.checkGenerated(i, e)
		}
		if err == nil {
			c.genFn, err = t.compileStored(e)
		}
		if err != nil {
			err := fmt.Errorf("bangdb: generated column %s.%s: %w", t.Name, c.Name, err)
			c.genFn = func(*env) (any, error) { return nil, err }
		}
	}
}

// checkGenerated checks the columns used by the expression of generated
// column i: they must be ordinary columns of the table.
func (t *Table) checkGenerated(i int, e sql.Expr) error {
	var err error
	walk(e, func(n sql.Expr) {
		r, ok := n.(*sql.ColumnRef)
		if !ok || err != nil {
			return
		}
		switch j := t.ColIndex(r.Name); {
		case j < 0 || r.Table != "" && !strings.EqualFold(r.Table, t.Name):
			err = fmt.Errorf("no such column: %s", sql.FormatExpr(r))
		case j == i:
			err = errors.New("the expression refers to the column itself")
		case t.Columns[j].Generated != "":
			err = fmt.Errorf("the expression refers to generated column %s", t.Columns[j].Name)
		}
	})
	return err
}

// generate computes the generated columns of row (len(t.Columns) values).
func (t *Table) generate(row []any) error {
	e := &env{row: row}
	for i := range t.Columns {
		if f := t.Columns[i].genFn; f != nil {
			v, err := f(e)
			if err != nil {
				return err
			}
			row[i] = v
		}
	}
	return nil
}

// storedColumns returns the positions of the columns a statement may
// assign: all but the generated ones.
func (t *Table) storedColumns() []int {
	out := make([]int, 0, len(t.Columns))
	for i, c := range t.Columns {
		if c.Generated == "" {
			out = append(out, i)
		}
	}
	return out
}
