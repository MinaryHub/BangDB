package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// This file implements CHECK constraints, foreign keys and views.
//
// Foreign keys are always enforced and checked immediately (per row). A
// child row whose key columns are all non-NULL must have a parent row with
// the same values in the referenced columns, which must be the parent's
// primary key or carry a UNIQUE constraint. ON DELETE / ON UPDATE support
// NO ACTION (the default), RESTRICT, CASCADE and SET NULL.

// maxFKDepth bounds cascading actions (and nested views).
const maxFKDepth = 64

var errFK = &constraintError{"FOREIGN KEY constraint failed"}

// ---- CHECK ----

func (t *Table) compileChecks() {
	t.checkFns = t.checkFns[:0]
	for _, src := range t.Checks {
		f, err := compileCheck(t, src)
		if err != nil {
			err := fmt.Errorf("bangdb: CHECK constraint of %s: %w", t.Name, err)
			f = func(*env) (any, error) { return nil, err }
		}
		t.checkFns = append(t.checkFns, f)
	}
}

func compileCheck(t *Table, src string) (evalFn, error) {
	e, err := sql.ParseExpr(src)
	if err != nil {
		return nil, err
	}
	if err := validateCheck(e); err != nil {
		return nil, err
	}
	return (&compiler{sc: &scope{cols: tableScope(t, t.Name, 0, false)}}).compile(e)
}

func validateCheck(e sql.Expr) error {
	if hasAggregate(e) || containsSubquery(e) {
		return errors.New("subqueries and aggregate functions are not allowed in CHECK constraints")
	}
	var err error
	walk(e, func(n sql.Expr) {
		if _, ok := n.(*sql.Param); ok {
			err = errors.New("parameters are not allowed in CHECK constraints")
		}
	})
	return err
}

// checkRow evaluates the CHECK constraints of t; NULL counts as passing.
func checkRow(t *Table, row []any) error {
	if len(t.checkFns) == 0 {
		return nil
	}
	e := &env{row: append(row[:len(row):len(row)], nil)} // hidden rowid unknown
	for i, f := range t.checkFns {
		v, err := f(e)
		if err != nil {
			return err
		}
		if v == nil {
			continue
		}
		if ok, _ := value.Truthy(v); !ok {
			return constraintErr("CHECK constraint failed: %s", t.Checks[i])
		}
	}
	return nil
}

// ---- foreign key definition ----

// buildForeignKeys validates the FOREIGN KEY clauses of a new table t (whose
// indexes already exist) and stores them in t.
func (x *execCtx) buildForeignKeys(t *Table, st *sql.CreateTable) error {
	defs := append([]sql.ForeignKey(nil), st.ForeignKeys...)
	for _, cd := range st.Columns {
		if cd.References != nil {
			fk := *cd.References
			fk.Columns = []string{cd.Name}
			defs = append(defs, fk)
		}
	}
	for _, d := range defs {
		fk := ForeignKey{RefTable: d.RefTable, OnDelete: d.OnDelete, OnUpdate: d.OnUpdate}
		for _, name := range d.Columns {
			i := t.ColIndex(name)
			if i < 0 {
				return fmt.Errorf("bangdb: unknown column %s in foreign key definition", name)
			}
			fk.Columns = append(fk.Columns, t.Columns[i].Name)
		}
		p := t
		if !strings.EqualFold(d.RefTable, t.Name) {
			if p = x.cat.table(d.RefTable); p == nil {
				return &unknownRefError{d.RefTable}
			}
		}
		if p.kind.Volatile() && !t.kind.Volatile() {
			// After a restart the parent rows would be gone.
			return fmt.Errorf("bangdb: table %s cannot reference %s: a durable table cannot reference a memory table", t.Name, p.Name)
		}
		fk.RefTable = p.Name
		refs := d.RefColumns
		if len(refs) == 0 {
			if refs = p.primaryKey(); refs == nil {
				return fmt.Errorf("bangdb: foreign key references table %s, which has no PRIMARY KEY", p.Name)
			}
		}
		for _, name := range refs {
			i := p.ColIndex(name)
			if i < 0 {
				return fmt.Errorf("bangdb: foreign key references unknown column %s.%s", p.Name, name)
			}
			fk.RefColumns = append(fk.RefColumns, p.Columns[i].Name)
		}
		if len(fk.RefColumns) != len(fk.Columns) {
			return fmt.Errorf("bangdb: foreign key on %s has %d columns but references %d", t.Name, len(fk.Columns), len(fk.RefColumns))
		}
		if !p.isKey(fk.RefColumns) {
			return fmt.Errorf("bangdb: foreign key references %s(%s), which is not a PRIMARY KEY or UNIQUE", p.Name, strings.Join(fk.RefColumns, ", "))
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
	}
	return nil
}

// primaryKey returns the primary key column names, or nil.
func (t *Table) primaryKey() []string {
	if len(t.PrimaryKey) > 0 {
		return t.PrimaryKey
	}
	for _, c := range t.Columns {
		if c.PrimaryKey {
			return []string{c.Name}
		}
	}
	return nil
}

// isKey reports whether cols are the rowid column or a UNIQUE index of t.
func (t *Table) isKey(cols []string) bool {
	return t.RowidCol >= 0 && sameColumns(cols, []string{t.Columns[t.RowidCol].Name}) || t.uniqueIndex(cols) != nil
}

func (t *Table) uniqueIndex(cols []string) *Index {
	for _, ix := range t.indexes {
		if ix.Unique && ix.Where == "" && !ix.hasExpr() && sameColumns(cols, ix.Columns) {
			return ix
		}
	}
	return nil
}

// ---- enforcement: child side ----

// checkParents verifies that every foreign key of row (in t) has a parent.
// For an update, old is the previous row and only changed keys are checked.
func (x *execCtx) checkParents(t *Table, row, old []any) error {
	if x.tx != nil && x.tx.deferFK {
		return nil // checked at the end of an import
	}
	for i := range t.ForeignKeys {
		fk := &t.ForeignKeys[i]
		if old != nil && !keyChanged(fk.cols, old, row) {
			continue
		}
		vals, ok := fkValues(row, fk.cols)
		if !ok {
			continue // MATCH SIMPLE: a NULL column disables the check
		}
		p := x.cat.table(fk.RefTable)
		if p == nil {
			return errFK
		}
		if p == t && sameValues(vals, fkValues2(row, t, fk.RefColumns)) {
			continue
		}
		found, err := x.parentExists(p, fk.RefColumns, vals)
		if err != nil {
			return err
		}
		if !found {
			return errFK
		}
	}
	return nil
}

func fkValues(row []any, cols []int) ([]any, bool) {
	vals := make([]any, len(cols))
	for i, c := range cols {
		if row[c] == nil {
			return nil, false
		}
		vals[i] = row[c]
	}
	return vals, true
}

// fkValues2 extracts the named columns of row in t.
func fkValues2(row []any, t *Table, names []string) []any {
	vals := make([]any, len(names))
	for i, n := range names {
		vals[i] = row[t.ColIndex(n)]
	}
	return vals
}

func sameValues(a, b []any) bool {
	for i := range a {
		if a[i] == nil || b[i] == nil || !bytes.Equal(value.AppendKey(nil, a[i]), value.AppendKey(nil, b[i])) {
			return false
		}
	}
	return true
}

func (x *execCtx) parentExists(p *Table, cols []string, vals []any) (bool, error) {
	tmp := make([]any, len(p.Columns))
	for i, n := range cols {
		c := p.ColIndex(n)
		v, err := value.Coerce(vals[i], p.Columns[c].Type)
		if err != nil {
			return false, nil
		}
		tmp[c] = v
	}
	if p.RowidCol >= 0 && len(cols) == 1 && strings.EqualFold(cols[0], p.Columns[p.RowidCol].Name) {
		id, ok := tmp[p.RowidCol].(int64)
		if !ok {
			return false, nil
		}
		_, found, err := x.getRow(p, id)
		return found, err
	}
	ix := p.uniqueIndex(cols)
	if ix == nil {
		return false, errFK
	}
	_, found, err := x.findKey(p, ix, tmp, math.MinInt64)
	return found, err
}

// ---- enforcement: parent side ----

// children returns the rows of child whose foreign key fk holds vals.
func (x *execCtx) children(child *Table, fk *ForeignKey, vals []any) ([]target, error) {
	var out []target
	add := func(rowid int64) error {
		row, ok, err := x.getRow(child, rowid)
		if err == nil && ok {
			// Recheck: a truncated index key or a collation can match
			// other values.
			if cv, ok := fkValues(row, fk.cols); ok && sameValues(cv, vals) {
				out = append(out, target{rowid, row})
			}
		}
		return err
	}
	// An index whose leading columns are the foreign key columns.
	for _, ix := range child.indexes {
		if ix.Where != "" || ix.hasExpr() || len(ix.cols) < len(fk.cols) || !sameColumns(ix.Columns[:len(fk.cols)], fk.Columns) {
			continue
		}
		var prefix []byte
		for i, c := range ix.cols[:len(fk.cols)] {
			for j, fc := range fk.cols {
				if fc == c {
					prefix = value.AppendKey(prefix, foldValue(vals[j], ix.colls[i]))
				}
			}
		}
		if len(fk.cols) == len(ix.cols) {
			prefix, _ = truncKey(prefix)
		} else {
			prefix, _ = groupKey(prefix)
		}
		var ids []int64
		c := x.index(ix).Seek(prefix)
		for ; c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
			if err := x.tick(); err != nil {
				return nil, err
			}
			k := c.Key()
			ids = append(ids, value.DecodeRowID(k[len(k)-8:]))
		}
		if err := c.Err(); err != nil {
			return nil, err
		}
		for _, id := range ids {
			if err := add(id); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	// No usable index: scan the child table.
	err := x.scanAll(child, func(rowid int64, row []any) (bool, error) {
		if cv, ok := fkValues(row, fk.cols); ok && sameValues(cv, vals) {
			out = append(out, target{rowid, row})
		}
		return true, nil
	})
	return out, err
}

// removeRow deletes a row and applies the ON DELETE actions of foreign keys
// referencing it.
func (x *execCtx) removeRow(t *Table, rowid int64, row []any) error {
	if err := x.deleteRow(t, rowid, row); err != nil {
		return err
	}
	x.closureChanged(t, row)
	if len(t.referencedBy) == 0 {
		return nil
	}
	x.fkDepth++
	defer func() { x.fkDepth-- }()
	if x.fkDepth > maxFKDepth {
		return errors.New("bangdb: too many levels of foreign key actions")
	}
	for _, ref := range t.referencedBy {
		vals, ok := fkValues(row, refCols(t, ref.fk))
		if !ok {
			continue
		}
		kids, err := x.children(ref.child, ref.fk, vals)
		if err != nil {
			return err
		}
		for _, k := range kids {
			switch ref.fk.OnDelete {
			case "CASCADE":
				// A cascade may already have removed or changed the row.
				cur, ok, err := x.getRow(ref.child, k.rowid)
				if err != nil {
					return err
				}
				if ok {
					if err := x.removeRow(ref.child, k.rowid, cur); err != nil {
						return err
					}
				}
			case "SET NULL":
				if err := x.setNull(ref.child, ref.fk, k); err != nil {
					return err
				}
			default:
				return errFK
			}
		}
	}
	return nil
}

// parentUpdated applies the ON UPDATE actions after row oldID of t changed
// from old to newRow.
func (x *execCtx) parentUpdated(t *Table, old, newRow []any) error {
	if len(t.referencedBy) == 0 {
		return nil
	}
	x.fkDepth++
	defer func() { x.fkDepth-- }()
	if x.fkDepth > maxFKDepth {
		return errors.New("bangdb: too many levels of foreign key actions")
	}
	for _, ref := range t.referencedBy {
		pcols := refCols(t, ref.fk)
		vals, ok := fkValues(old, pcols)
		if !ok {
			continue
		}
		nv, _ := fkValues(newRow, pcols)
		if nv != nil && sameValues(vals, nv) {
			continue // referenced key unchanged
		}
		kids, err := x.children(ref.child, ref.fk, vals)
		if err != nil {
			return err
		}
		for _, k := range kids {
			switch ref.fk.OnUpdate {
			case "CASCADE":
				upd := append([]any(nil), k.row...)
				for i, c := range ref.fk.cols {
					upd[c] = newRow[pcols[i]]
				}
				if _, err := x.updateRow(ref.child, k.rowid, k.row, upd); err != nil {
					return err
				}
			case "SET NULL":
				if err := x.setNull(ref.child, ref.fk, k); err != nil {
					return err
				}
			default:
				return errFK
			}
		}
	}
	return nil
}

func (x *execCtx) setNull(child *Table, fk *ForeignKey, k target) error {
	cur, ok, err := x.getRow(child, k.rowid)
	if err != nil || !ok {
		return err
	}
	upd := append([]any(nil), cur...)
	for _, c := range fk.cols {
		upd[c] = nil
	}
	_, err = x.updateRow(child, k.rowid, cur, upd)
	return err
}

// refCols returns the positions in parent t of the columns fk references.
func refCols(t *Table, fk *ForeignKey) []int {
	out := make([]int, len(fk.RefColumns))
	for i, n := range fk.RefColumns {
		out[i] = t.ColIndex(n)
	}
	return out
}

func keyChanged(cols []int, old, newRow []any) bool {
	for _, c := range cols {
		if !bytes.Equal(value.AppendKey(nil, old[c]), value.AppendKey(nil, newRow[c])) {
			return true
		}
	}
	return false
}

// ---- DDL helpers ----

// referencingTables lists other tables with a foreign key to t.
func (c *Catalog) referencingTables(t *Table) []string {
	var out []string
	for _, ref := range t.referencedBy {
		if ref.child != t {
			out = append(out, ref.child.Name)
		}
	}
	return out
}

// columnInUse explains why column i of t cannot be dropped, or returns "".
func columnInUse(t *Table, i int) string {
	name := t.Columns[i].Name
	for _, fk := range t.ForeignKeys {
		for _, c := range fk.cols {
			if c == i {
				return "it is part of a foreign key"
			}
		}
	}
	for _, ref := range t.referencedBy {
		for _, n := range ref.fk.RefColumns {
			if strings.EqualFold(n, name) {
				return "it is referenced by a foreign key of " + ref.child.Name
			}
		}
	}
	for _, src := range t.Checks {
		if e, err := sql.ParseExpr(src); err == nil && refersTo(e, name) {
			return "it is used by a CHECK constraint"
		}
	}
	for _, ix := range t.indexes {
		if e, err := sql.ParseExpr(ix.Where); ix.Where != "" && err == nil && refersTo(e, name) {
			return "it is used by the WHERE of index " + ix.Name
		}
		for _, src := range ix.Exprs {
			if e, err := sql.ParseExpr(src); src != "" && err == nil && refersTo(e, name) {
				return "it is used by an expression of index " + ix.Name + " (drop the index first)"
			}
		}
	}
	for _, c := range t.Columns {
		if e, err := sql.ParseExpr(c.Generated); c.Generated != "" && err == nil && refersTo(e, name) {
			return "it is used by generated column " + c.Name
		}
	}
	return ""
}

func refersTo(e sql.Expr, col string) bool {
	found := false
	walk(e, func(n sql.Expr) {
		if r, ok := n.(*sql.ColumnRef); ok && strings.EqualFold(r.Name, col) {
			found = true
		}
	})
	return found
}

// renameInChecks rewrites column references in the CHECK constraints of t.
func renameInChecks(t *Table, old, new string) {
	for i, src := range t.Checks {
		t.Checks[i] = renameInExpr(src, old, new)
	}
}

// renameInExpr rewrites column references in a stored SQL expression.
func renameInExpr(src, old, new string) string {
	e, err := sql.ParseExpr(src)
	if err != nil {
		return src
	}
	walk(e, func(n sql.Expr) {
		if r, ok := n.(*sql.ColumnRef); ok && strings.EqualFold(r.Name, old) {
			r.Name = new
		}
	})
	return sql.FormatExpr(e)
}

func renameIn(names []string, old, new string) {
	for i, n := range names {
		if strings.EqualFold(n, old) {
			names[i] = new
		}
	}
}

// ---- views ----

func (x *execCtx) createView(st *sql.CreateView) error {
	if x.cat.view(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("bangdb: view %s already exists", st.Name)
	}
	if x.cat.table(st.Name) != nil {
		return fmt.Errorf("bangdb: there is already a table named %s", st.Name)
	}
	if strings.HasPrefix(strings.ToLower(st.Name), "bangdb_") {
		return fmt.Errorf("bangdb: names starting with bangdb_ are reserved")
	}
	if _, n, err := sql.Parse(st.SQL); err != nil {
		return err
	} else if n > 0 {
		return errors.New("bangdb: parameters are not allowed in views")
	}
	v := &View{Name: st.Name, Columns: st.Columns, SQL: st.SQL}
	if err := v.prepare(); err != nil {
		return err
	}
	r, err := x.prepareSelect(v.sel)
	if err != nil {
		return err
	}
	if len(v.Columns) > 0 && len(v.Columns) != len(r.p.cols) {
		return fmt.Errorf("bangdb: view %s has %d column names but the SELECT returns %d columns", v.Name, len(v.Columns), len(r.p.cols))
	}
	x.mutableCatalog()
	x.cat.views[strings.ToLower(v.Name)] = v
	return saveEntry(x.v, viewKey(v.Name), v)
}

func (x *execCtx) dropView(st *sql.DropView) error {
	v := x.cat.view(st.Name)
	if v == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("bangdb: no such view: %s", st.Name)
	}
	x.mutableCatalog()
	delete(x.cat.views, strings.ToLower(v.Name))
	return deleteEntry(x.v, viewKey(v.Name))
}

// viewTable runs view v and returns it as a virtual table.
func (x *execCtx) viewTable(v *View, alias string, filters ...derivedFilter) (*Table, [][]any, []derivedFilter, error) {
	x.fkDepth++
	defer func() { x.fkDepth-- }()
	if x.fkDepth > maxFKDepth {
		return nil, nil, nil, fmt.Errorf("bangdb: view %s is nested too deeply (or refers to itself)", v.Name)
	}
	outer, ctes := x.outer, x.ctes
	x.outer, x.ctes = nil, nil // a view never sees the enclosing query
	res, pushed, err := x.execPushed(v.sel, v.Columns, filters)
	x.outer, x.ctes = outer, ctes
	if err != nil {
		return nil, nil, nil, fmt.Errorf("bangdb: view %s: %w", v.Name, err)
	}
	t := &Table{Name: alias, RowidCol: -1}
	names := res.Columns
	if len(v.Columns) > 0 {
		if len(v.Columns) != len(names) {
			return nil, nil, nil, fmt.Errorf("bangdb: view %s has %d column names but the SELECT returns %d columns", v.Name, len(v.Columns), len(names))
		}
		names = v.Columns
	}
	for _, c := range names {
		t.Columns = append(t.Columns, Column{Name: c, Type: value.Any})
	}
	rows := res.Rows
	if rows == nil {
		rows = [][]any{}
	}
	return t, rows, pushed, nil
}

// DescribeView renders a view as SQL, or "" if there is none.
func (c *Catalog) DescribeView(name string) string {
	v := c.view(name)
	if v == nil {
		return ""
	}
	cols := ""
	if len(v.Columns) > 0 {
		cols = " (" + quoteIdents(v.Columns) + ")"
	}
	return fmt.Sprintf("CREATE VIEW %s%s AS %s;", sql.QuoteIdent(v.Name), cols, v.SQL)
}

// unknownRefError is returned when a foreign key references a table that
// does not exist (yet).
type unknownRefError struct{ table string }

func (e *unknownRefError) Error() string {
	return "bangdb: foreign key references unknown table " + e.table
}
