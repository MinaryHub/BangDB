package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// execCtx carries what a single statement needs to run.
type execCtx struct {
	v    *pager.View
	cat  *Catalog
	tx   *Tx // nil for read-only execution
	args []any
}

func (x *execCtx) exec(s sql.Stmt) (Result, error) {
	switch st := s.(type) {
	case *sql.Select:
		return x.execSelect(st)
	case *sql.Explain:
		return x.explain(st.Stmt)
	}
	if x.tx == nil {
		return Result{}, errors.New("bangdb: write statement outside of a write transaction")
	}
	switch st := s.(type) {
	case *sql.Insert:
		return x.execInsert(st)
	case *sql.Update:
		return x.execUpdate(st)
	case *sql.Delete:
		return x.execDelete(st)
	case *sql.CreateTable:
		return Result{}, x.createTable(st)
	case *sql.DropTable:
		return Result{}, x.dropTable(st)
	case *sql.CreateIndex:
		return Result{}, x.createIndex(st)
	case *sql.DropIndex:
		return Result{}, x.dropIndex(st)
	case *sql.AlterTableAddColumn:
		return Result{}, x.addColumn(st)
	}
	return Result{}, fmt.Errorf("bangdb: unsupported statement %T", s)
}

func (x *execCtx) table(name string) (*Table, error) {
	t := x.cat.table(name)
	if t == nil {
		return nil, fmt.Errorf("bangdb: no such table: %s", name)
	}
	return t, nil
}

// constraintError matches ErrConstraint with errors.Is.
type constraintError struct{ msg string }

func (e *constraintError) Error() string        { return "bangdb: " + e.msg }
func (e *constraintError) Is(target error) bool { return target == ErrConstraint }

func constraintErr(format string, args ...any) error {
	return &constraintError{fmt.Sprintf(format, args...)}
}

// ---- row storage helpers ----

func (x *execCtx) decodeRow(t *Table, data []byte) ([]any, error) {
	row, err := value.DecodeRow(data)
	if err != nil {
		return nil, err
	}
	// Rows written before ALTER TABLE ADD COLUMN are shorter.
	for len(row) < len(t.Columns) {
		row = append(row, t.Columns[len(row)].defConst)
	}
	return row, nil
}

func (x *execCtx) getRow(t *Table, rowid int64) ([]any, bool, error) {
	data, ok, err := btree.Open(x.v, t.Root).Get(value.EncodeRowID(rowid))
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := x.decodeRow(t, data)
	return row, err == nil, err
}

func indexEntry(val any, rowid int64) ([]byte, error) {
	k := value.AppendKey(nil, val)
	k = append(k, value.EncodeRowID(rowid)...)
	if len(k) > btree.MaxKey {
		return nil, fmt.Errorf("bangdb: value too large to be indexed (max %d bytes)", btree.MaxKey-16)
	}
	return k, nil
}

// checkUnique fails if another row (not self) already has val in ix.
func (x *execCtx) checkUnique(t *Table, ix *Index, val any, self int64) error {
	if !ix.Unique || val == nil {
		return nil
	}
	prefix := value.AppendKey(nil, val)
	for c := btree.Open(x.v, ix.Root).Seek(prefix); c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
		if value.DecodeRowID(c.Key()[len(prefix):]) != self {
			return constraintErr("UNIQUE constraint failed: %s.%s", t.Name, ix.Column)
		}
	}
	return nil
}

func (x *execCtx) nextRowid(t *Table) (int64, error) {
	key := strings.ToLower(t.Name)
	if id, ok := x.tx.rowids[key]; ok {
		return id, nil
	}
	last, ok, err := btree.Open(x.v, t.Root).Last()
	if err != nil {
		return 0, err
	}
	next := int64(1)
	if ok {
		lastID := value.DecodeRowID(last)
		if lastID == math.MaxInt64 {
			return 0, errors.New("bangdb: rowid space exhausted")
		}
		next = max(lastID+1, 1)
	}
	x.tx.rowids[key] = next
	return next, nil
}

func (x *execCtx) noteRowid(t *Table, id int64) {
	key := strings.ToLower(t.Name)
	if next, ok := x.tx.rowids[key]; ok && id >= next && id < math.MaxInt64 {
		x.tx.rowids[key] = id + 1
	}
}

// prepareRow coerces values to column types and checks NOT NULL.
func prepareRow(t *Table, row []any) error {
	for i, c := range t.Columns {
		v, err := value.Coerce(row[i], c.Type)
		if err != nil {
			return fmt.Errorf("bangdb: column %s.%s: %w", t.Name, c.Name, err)
		}
		row[i] = v
		if v == nil && (c.NotNull || (c.PrimaryKey && i != t.RowidCol)) {
			return constraintErr("NOT NULL constraint failed: %s.%s", t.Name, c.Name)
		}
	}
	return nil
}

// insertRow writes a new row (already prepared) and returns its rowid.
func (x *execCtx) insertRow(t *Table, row []any) (int64, error) {
	var rowid int64
	tree := btree.Open(x.v, t.Root)
	if t.RowidCol >= 0 && row[t.RowidCol] != nil {
		rowid = row[t.RowidCol].(int64)
		exists, err := tree.Has(value.EncodeRowID(rowid))
		if err != nil {
			return 0, err
		}
		if exists {
			return 0, constraintErr("UNIQUE constraint failed: %s.%s", t.Name, t.Columns[t.RowidCol].Name)
		}
		x.noteRowid(t, rowid)
	} else {
		var err error
		if rowid, err = x.nextRowid(t); err != nil {
			return 0, err
		}
		x.tx.rowids[strings.ToLower(t.Name)] = rowid + 1
		if t.RowidCol >= 0 {
			row[t.RowidCol] = rowid
		}
	}
	for _, ix := range t.indexes {
		if err := x.checkUnique(t, ix, row[ix.col], rowid); err != nil {
			return 0, err
		}
	}
	for _, ix := range t.indexes {
		k, err := indexEntry(row[ix.col], rowid)
		if err != nil {
			return 0, err
		}
		if err := btree.Open(x.v, ix.Root).Put(k, nil); err != nil {
			return 0, err
		}
	}
	return rowid, tree.Put(value.EncodeRowID(rowid), value.EncodeRow(row))
}

func (x *execCtx) deleteRow(t *Table, rowid int64, row []any) error {
	for _, ix := range t.indexes {
		k, err := indexEntry(row[ix.col], rowid)
		if err != nil {
			return err
		}
		if _, err := btree.Open(x.v, ix.Root).Delete(k); err != nil {
			return err
		}
	}
	_, err := btree.Open(x.v, t.Root).Delete(value.EncodeRowID(rowid))
	return err
}

// ---- DDL ----

func (x *execCtx) mutableCatalog() {
	if x.tx != nil && x.cat == x.tx.cat {
		x.cat = x.cat.clone()
	}
}

func (x *execCtx) createTable(st *sql.CreateTable) error {
	if x.cat.table(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("bangdb: table %s already exists", st.Name)
	}
	if strings.HasPrefix(strings.ToLower(st.Name), "bangdb_") {
		return fmt.Errorf("bangdb: table names starting with bangdb_ are reserved")
	}
	t := &Table{Name: st.Name, RowidCol: -1}
	pkCount := 0
	for _, cd := range st.Columns {
		if t.ColIndex(cd.Name) >= 0 {
			return fmt.Errorf("bangdb: duplicate column name: %s", cd.Name)
		}
		col, err := columnFromDef(cd)
		if err != nil {
			return err
		}
		if col.PrimaryKey {
			pkCount++
			if col.Type == value.Integer && strings.Contains(col.DeclType, "INT") {
				t.RowidCol = len(t.Columns)
			} else if col.AutoIncrement {
				return fmt.Errorf("bangdb: AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
			}
		}
		t.Columns = append(t.Columns, col)
	}
	if pkCount > 1 {
		return fmt.Errorf("bangdb: table %s has more than one primary key", st.Name)
	}
	if len(t.Columns) == 0 {
		return fmt.Errorf("bangdb: table %s must have at least one column", st.Name)
	}
	root, err := btree.Create(x.v)
	if err != nil {
		return err
	}
	t.Root = root
	x.mutableCatalog()
	x.cat.tables[strings.ToLower(t.Name)] = t
	if err := saveEntry(x.v, tableKey(t.Name), t); err != nil {
		return err
	}
	for i, c := range t.Columns {
		if i == t.RowidCol || !(c.Unique || c.PrimaryKey) {
			continue
		}
		name := fmt.Sprintf("bangdb_autoindex_%s_%s", t.Name, c.Name)
		if err := x.addIndex(&Index{Name: name, Table: t.Name, Column: c.Name, Unique: true, Auto: true}); err != nil {
			return err
		}
	}
	x.cat.link()
	return nil
}

func columnFromDef(cd sql.ColumnDef) (Column, error) {
	col := Column{Name: cd.Name, Type: value.ParseType(cd.Type), DeclType: cd.Type,
		NotNull: cd.NotNull, PrimaryKey: cd.PrimaryKey, Unique: cd.Unique, AutoIncrement: cd.AutoIncrement}
	if cd.AutoIncrement && !cd.PrimaryKey {
		return col, fmt.Errorf("bangdb: AUTOINCREMENT requires PRIMARY KEY")
	}
	if cd.Default != nil {
		col.DefaultSQL = sql.FormatExpr(cd.Default)
		if err := col.prepareDefault(); err != nil {
			return col, err
		}
		if _, err := (&compiler{}).compile(col.defExpr); err != nil {
			return col, fmt.Errorf("bangdb: invalid default for column %s: %w", cd.Name, err)
		}
		if v, ok := constValue(cd.Default); ok {
			if _, err := value.Coerce(v, col.Type); err != nil {
				return col, fmt.Errorf("bangdb: default for column %s: %w", cd.Name, err)
			}
		}
	}
	return col, nil
}

func (x *execCtx) dropTable(st *sql.DropTable) error {
	t := x.cat.table(st.Name)
	if t == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("bangdb: no such table: %s", st.Name)
	}
	x.mutableCatalog()
	t = x.cat.table(st.Name)
	for _, ix := range t.indexes {
		if err := btree.Drop(x.v, ix.Root); err != nil {
			return err
		}
		if err := deleteEntry(x.v, indexKey(ix.Name)); err != nil {
			return err
		}
		delete(x.cat.indexes, strings.ToLower(ix.Name))
	}
	if err := btree.Drop(x.v, t.Root); err != nil {
		return err
	}
	if err := deleteEntry(x.v, tableKey(t.Name)); err != nil {
		return err
	}
	delete(x.cat.tables, strings.ToLower(t.Name))
	delete(x.tx.rowids, strings.ToLower(t.Name))
	x.cat.link()
	return nil
}

func (x *execCtx) createIndex(st *sql.CreateIndex) error {
	if x.cat.index(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("bangdb: index %s already exists", st.Name)
	}
	t, err := x.table(st.Table)
	if err != nil {
		return err
	}
	col := t.ColIndex(st.Column)
	if col < 0 {
		return fmt.Errorf("bangdb: no such column: %s.%s", t.Name, st.Column)
	}
	x.mutableCatalog()
	if err := x.addIndex(&Index{Name: st.Name, Table: t.Name, Column: t.Columns[col].Name, Unique: st.Unique}); err != nil {
		return err
	}
	x.cat.link()
	return nil
}

// addIndex creates the index tree, fills it from existing rows and stores it
// in the catalog. The catalog must already be mutable.
func (x *execCtx) addIndex(ix *Index) error {
	t := x.cat.table(ix.Table)
	ix.col = t.ColIndex(ix.Column)
	root, err := btree.Create(x.v)
	if err != nil {
		return err
	}
	ix.Root = root
	tree := btree.Open(x.v, root)
	c := btree.Open(x.v, t.Root).Seek(nil)
	for ; c.Valid(); c.Next() {
		data, err := c.Value()
		if err != nil {
			return err
		}
		row, err := x.decodeRow(t, data)
		if err != nil {
			return err
		}
		rowid := value.DecodeRowID(c.Key())
		if err := x.checkUnique(t, ix, row[ix.col], rowid); err != nil {
			return err
		}
		k, err := indexEntry(row[ix.col], rowid)
		if err != nil {
			return err
		}
		if err := tree.Put(k, nil); err != nil {
			return err
		}
	}
	if err := c.Err(); err != nil {
		return err
	}
	x.cat.indexes[strings.ToLower(ix.Name)] = ix
	return saveEntry(x.v, indexKey(ix.Name), ix)
}

func (x *execCtx) dropIndex(st *sql.DropIndex) error {
	ix := x.cat.index(st.Name)
	if ix == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("bangdb: no such index: %s", st.Name)
	}
	if ix.Auto {
		return fmt.Errorf("bangdb: index %s is associated with a UNIQUE or PRIMARY KEY constraint and cannot be dropped", ix.Name)
	}
	x.mutableCatalog()
	if err := btree.Drop(x.v, ix.Root); err != nil {
		return err
	}
	delete(x.cat.indexes, strings.ToLower(ix.Name))
	x.cat.link()
	return deleteEntry(x.v, indexKey(ix.Name))
}

func (x *execCtx) addColumn(st *sql.AlterTableAddColumn) error {
	t, err := x.table(st.Table)
	if err != nil {
		return err
	}
	if t.ColIndex(st.Column.Name) >= 0 {
		return fmt.Errorf("bangdb: duplicate column name: %s", st.Column.Name)
	}
	if st.Column.PrimaryKey || st.Column.Unique {
		return fmt.Errorf("bangdb: cannot add a PRIMARY KEY or UNIQUE column")
	}
	col, err := columnFromDef(st.Column)
	if err != nil {
		return err
	}
	if col.defExpr != nil && col.defConst == nil {
		if _, isNull := col.defExpr.(*sql.Literal); !isNull {
			return fmt.Errorf("bangdb: cannot add a column with non-constant default")
		}
	}
	if col.NotNull && col.defConst == nil {
		return fmt.Errorf("bangdb: cannot add a NOT NULL column with default value NULL")
	}
	x.mutableCatalog()
	t = x.cat.table(st.Table)
	t.Columns = append(t.Columns, col)
	x.cat.link()
	return saveEntry(x.v, tableKey(t.Name), t)
}
