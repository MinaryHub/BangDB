package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

// execCtx carries what a single statement needs to run.
type execCtx struct {
	v     *pager.View
	cat   *Catalog
	tx    *Tx // nil for read-only execution
	args  []any
	ctx   context.Context
	ticks uint32
	outer *outerCtx // set when planning a subquery

	fkDepth int          // nesting of foreign key actions and views
	spill   *spillConfig // nil: defaults

	// keepJoinOrder disables join reordering while a SELECT whose ORDER BY
	// and LIMIT may use the first table's order is planned.
	keepJoinOrder bool

	keyPart []byte // key part of the index entry being visited (covering scans)

	lastTable *Table // cache of rows()
	lastRoot  uint32
	lastRows  store.TableStore
	plans     *planCache // nil: no plan reuse
}

// tick is called once per row visited; every 256 rows it checks whether the
// statement was canceled or timed out.
func (x *execCtx) tick() error {
	x.ticks++
	if x.ticks&255 == 0 && x.ctx != nil {
		if err := x.ctx.Err(); err != nil {
			return interrupted(err)
		}
	}
	return nil
}

func (x *execCtx) exec(s sql.Stmt) (Result, error) {
	switch st := s.(type) {
	case *sql.Select:
		return x.execSelect(st)
	case *sql.Explain:
		if st.Analyze {
			return x.explainAnalyze(st.Stmt)
		}
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
	case *sql.AlterTableRename:
		return Result{}, x.renameTable(st)
	case *sql.AlterTableRenameColumn:
		return Result{}, x.renameColumn(st)
	case *sql.AlterTableDropColumn:
		return Result{}, x.dropColumn(st)
	case *sql.CreateView:
		return Result{}, x.createView(st)
	case *sql.DropView:
		return Result{}, x.dropView(st)
	case *sql.Analyze:
		return Result{}, x.analyze(st)
	}
	return Result{}, fmt.Errorf("bangdb: unsupported statement %T", s)
}

func (x *execCtx) table(name string) (*Table, error) {
	t := x.cat.table(name)
	if t == nil {
		if x.cat.view(name) != nil {
			return nil, fmt.Errorf("bangdb: cannot modify %s because it is a view", name)
		}
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
//
// Rows and index entries are reached only through package store, so tables
// can use other storage kinds than the B+tree.

// defaultStore is the storage kind of tables created without USING.
var defaultStore = ""

// SetDefaultStore sets the storage kind of tables created without USING
// ("" or "btree" = the B+tree row store). It is meant for tests that run a
// whole suite against another kind; it is not safe to call concurrently
// with CREATE TABLE.
func SetDefaultStore(name string) { defaultStore = name }

// rows returns the store holding t's rows in the statement's view. The
// last one is kept, as row fetches by rowid come in long runs.
func (x *execCtx) rows(t *Table) store.TableStore {
	if x.lastTable != t || x.lastRoot != t.Root {
		var s store.TableStore
		if t.storeCfg != nil {
			s = t.kind.(store.ConfigurableKind).OpenConfigured(x.v, t.Root, t.storeCfg)
		} else {
			s = t.kind.Open(x.v, t.Root)
		}
		x.lastTable, x.lastRoot, x.lastRows = t, t.Root, s
	}
	return x.lastRows
}

// index returns the store holding ix's entries.
func (x *execCtx) index(ix *Index) store.IndexStore { return ix.kind.OpenIndex(x.v, ix.Root) }

// timeColumn returns the column whose order is t's rowid order (a
// time-series table's time column), or -1.
func (t *Table) timeColumn() int {
	if t.storeCfg == nil {
		return -1
	}
	return t.timeCol
}

// padRow completes a stored row: rows written before ALTER TABLE ADD COLUMN
// are shorter.
func padRow(t *Table, row []any) []any {
	for len(row) < len(t.Columns) {
		row = append(row, t.Columns[len(row)].defConst)
	}
	return row
}

func (x *execCtx) decodeRow(t *Table, data []byte) ([]any, error) {
	row, err := value.DecodeRow(data)
	if err != nil {
		return nil, err
	}
	return padRow(t, row), nil
}

func (x *execCtx) getRow(t *Table, rowid int64) ([]any, bool, error) {
	row, ok, err := x.rows(t).Get(rowid)
	if err != nil || !ok {
		return nil, false, err
	}
	return padRow(t, row), true, nil
}

// scanRows visits t's rows with lo <= rowid <= hi in rowid order (desc:
// descending), checking for cancellation.
func (x *execCtx) scanRows(t *Table, lo, hi int64, desc bool, fn func(rowid int64, row []any) (bool, error)) error {
	return x.rows(t).Scan(lo, hi, desc, func(rowid int64, row []any) (bool, error) {
		if err := x.tick(); err != nil {
			return false, err
		}
		return fn(rowid, padRow(t, row))
	})
}

// scanAll visits every row of t in rowid order.
func (x *execCtx) scanAll(t *Table, fn func(rowid int64, row []any) (bool, error)) error {
	return x.scanRows(t, store.MinRowid, store.MaxRowid, false, fn)
}

func indexEntry(ix *Index, row []any, rowid int64) ([]byte, error) {
	return append(ix.key(row), value.EncodeRowID(rowid)...), nil
}

// findKey returns a row other than self whose values in ix equal those of
// row (row must have every index column set).
func (x *execCtx) findKey(t *Table, ix *Index, row []any, self int64) (int64, bool, error) {
	full := ix.fullKey(row)
	prefix, trunc := truncKey(full)
	c := x.index(ix).Seek(prefix)
	for ; c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
		id := value.DecodeRowID(c.Key()[len(c.Key())-8:])
		if id == self {
			continue
		}
		if trunc {
			other, ok, err := x.getRow(t, id)
			if err != nil {
				return 0, false, err
			}
			if !ok || !bytes.Equal(ix.fullKey(other), full) {
				continue // shares only the truncated prefix
			}
		}
		return id, true, nil
	}
	return 0, false, c.Err()
}

// checkUnique fails if another row (not self) has the same values in ix.
func (x *execCtx) checkUnique(t *Table, ix *Index, row []any, self int64) error {
	if !ix.Unique || ix.hasNull(row) {
		return nil
	}
	if _, found, err := x.findKey(t, ix, row, self); err != nil || !found {
		return err
	}
	return constraintErr("UNIQUE constraint failed: %s.%s", t.Name, strings.Join(ix.Columns, ", "+t.Name+"."))
}

func (x *execCtx) nextRowid(t *Table) (int64, error) {
	key := strings.ToLower(t.Name)
	if id, ok := x.tx.rowids[key]; ok {
		return id, nil
	}
	last, ok, err := x.rows(t).Last()
	if err != nil {
		return 0, err
	}
	high := int64(0)
	if ok {
		high = last
	}
	if t.autoIncrement() {
		// AUTOINCREMENT never reuses ids, even after the largest row was
		// deleted: honour the recorded high-water mark.
		if hw, err := x.sequence(t); err != nil {
			return 0, err
		} else if hw > high {
			high = hw
		}
	}
	if high == math.MaxInt64 {
		return 0, errors.New("bangdb: rowid space exhausted")
	}
	next := max(high+1, 1)
	x.tx.rowids[key] = next
	return next, nil
}

// metaSequenceRoot is the header meta slot of the AUTOINCREMENT high-water
// mark tree (table name -> largest rowid ever issued).
const metaSequenceRoot = 1

func (t *Table) autoIncrement() bool {
	return t.RowidCol >= 0 && t.Columns[t.RowidCol].AutoIncrement
}

func (x *execCtx) sequence(t *Table) (int64, error) {
	root := x.v.Meta(metaSequenceRoot)
	if root == 0 {
		return 0, nil
	}
	b, ok, err := btree.Open(x.v, root).Get([]byte(strings.ToLower(t.Name)))
	if err != nil || !ok {
		return 0, err
	}
	return value.DecodeRowID(b), nil
}

// recordSequence raises the AUTOINCREMENT high-water mark of t to id.
func (x *execCtx) recordSequence(t *Table, id int64) error {
	if !t.autoIncrement() {
		return nil
	}
	if hw, err := x.sequence(t); err != nil || hw >= id {
		return err
	}
	root := x.v.Meta(metaSequenceRoot)
	if root == 0 {
		var err error
		if root, err = btree.Create(x.v); err != nil {
			return err
		}
		x.v.SetMeta(metaSequenceRoot, root)
	}
	return btree.Open(x.v, root).Put([]byte(strings.ToLower(t.Name)), value.EncodeRowID(id))
}

func (x *execCtx) dropSequence(t *Table) error {
	root := x.v.Meta(metaSequenceRoot)
	if root == 0 {
		return nil
	}
	_, err := btree.Open(x.v, root).Delete([]byte(strings.ToLower(t.Name)))
	return err
}

func (x *execCtx) noteRowid(t *Table, id int64) {
	key := strings.ToLower(t.Name)
	if next, ok := x.tx.rowids[key]; ok && id >= next && id < math.MaxInt64 {
		x.tx.rowids[key] = id + 1
	}
}

// prepareRow coerces values to column types and checks NOT NULL and CHECK
// constraints.
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
	return checkRow(t, row)
}

// insertRow writes a new row (already prepared) and returns its rowid.
func (x *execCtx) insertRow(t *Table, row []any) (int64, error) {
	var rowid int64
	rows := x.rows(t)
	if a, ok := rows.(store.RowidAssigner); ok {
		var err error
		if rowid, err = a.AssignRowid(row); err != nil {
			return 0, err
		}
	} else if t.RowidCol >= 0 && row[t.RowidCol] != nil {
		rowid = row[t.RowidCol].(int64)
		_, exists, err := rows.Get(rowid)
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
		if err := x.checkUnique(t, ix, row, rowid); err != nil {
			return 0, err
		}
	}
	if len(t.ForeignKeys) > 0 {
		if err := x.checkParents(t, row, nil); err != nil {
			return 0, err
		}
	}
	for _, ix := range t.indexes {
		k, err := indexEntry(ix, row, rowid)
		if err != nil {
			return 0, err
		}
		if err := x.index(ix).Put(k); err != nil {
			return 0, err
		}
	}
	return rowid, rows.Put(rowid, row)
}

func (x *execCtx) deleteRow(t *Table, rowid int64, row []any) error {
	for _, ix := range t.indexes {
		k, err := indexEntry(ix, row, rowid)
		if err != nil {
			return err
		}
		if _, err := x.index(ix).Delete(k); err != nil {
			return err
		}
	}
	_, err := x.rows(t).Delete(rowid)
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
	if x.cat.view(st.Name) != nil {
		return fmt.Errorf("bangdb: there is already a view named %s", st.Name)
	}
	if strings.HasPrefix(strings.ToLower(st.Name), "bangdb_") {
		return fmt.Errorf("bangdb: table names starting with bangdb_ are reserved")
	}
	t := &Table{Name: st.Name, RowidCol: -1}
	var pkCols []int
	for _, cd := range st.Columns {
		if t.ColIndex(cd.Name) >= 0 {
			return fmt.Errorf("bangdb: duplicate column name: %s", cd.Name)
		}
		col, err := columnFromDef(cd)
		if err != nil {
			return err
		}
		if col.PrimaryKey {
			pkCols = append(pkCols, len(t.Columns))
		}
		t.Columns = append(t.Columns, col)
	}
	if len(t.Columns) == 0 {
		return fmt.Errorf("bangdb: table %s must have at least one column", st.Name)
	}
	resolve := func(names []string) ([]int, []string, error) {
		var idx []int
		var canon []string
		for _, n := range names {
			i := t.ColIndex(n)
			if i < 0 {
				return nil, nil, fmt.Errorf("bangdb: no such column %s in table %s", n, st.Name)
			}
			for _, j := range idx {
				if j == i {
					return nil, nil, fmt.Errorf("bangdb: column %s listed twice", n)
				}
			}
			idx = append(idx, i)
			canon = append(canon, t.Columns[i].Name)
		}
		return idx, canon, nil
	}
	if st.PrimaryKey != nil {
		if len(pkCols) > 0 {
			return fmt.Errorf("bangdb: table %s has more than one primary key", st.Name)
		}
		var err error
		if pkCols, _, err = resolve(st.PrimaryKey); err != nil {
			return err
		}
		for _, i := range pkCols {
			t.Columns[i].PrimaryKey = true
		}
	}
	if len(pkCols) > 1 && len(st.PrimaryKey) == 0 {
		return fmt.Errorf("bangdb: table %s has more than one primary key", st.Name)
	}
	for _, i := range pkCols {
		if t.Columns[i].AutoIncrement && len(pkCols) > 1 {
			return fmt.Errorf("bangdb: AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
		}
	}
	if len(pkCols) == 1 {
		c := t.Columns[pkCols[0]]
		if c.Type == value.Integer && strings.Contains(c.DeclType, "INT") {
			t.RowidCol = pkCols[0]
		} else if c.AutoIncrement {
			return fmt.Errorf("bangdb: AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
		}
	}
	type autoIdx struct {
		cols []string
		pk   bool
	}
	var autos []autoIdx
	if len(pkCols) > 1 {
		for _, i := range pkCols {
			t.PrimaryKey = append(t.PrimaryKey, t.Columns[i].Name)
		}
		autos = append(autos, autoIdx{t.PrimaryKey, true})
	}
	for i, c := range t.Columns {
		if i != t.RowidCol && (c.Unique || c.PrimaryKey && len(pkCols) == 1) {
			autos = append(autos, autoIdx{[]string{c.Name}, c.PrimaryKey})
		}
	}
	for _, u := range st.Uniques {
		_, canon, err := resolve(u)
		if err != nil {
			return err
		}
		autos = append(autos, autoIdx{cols: canon})
	}
	checks := append([]sql.Expr(nil), st.Checks...)
	for _, cd := range st.Columns {
		if cd.Check != nil {
			checks = append(checks, cd.Check)
		}
	}
	for _, e := range checks {
		if err := validateCheck(e); err != nil {
			return fmt.Errorf("bangdb: %w", err)
		}
		src := sql.FormatExpr(e)
		if _, err := compileCheck(t, src); err != nil {
			return fmt.Errorf("bangdb: CHECK constraint: %w", err)
		}
		t.Checks = append(t.Checks, src)
	}

	t.Store = st.Using
	if t.Store == "" {
		t.Store = defaultStore
	}
	if t.Store == "btree" {
		t.Store = "" // the default kind's catalog name
	}
	kind, err := store.Lookup(t.Store)
	if err != nil {
		return err
	}
	t.kind = kind
	t.StoreOptions = st.UsingOptions
	if err := t.configure(); err != nil {
		return err
	}
	root, err := kind.Create(x.v)
	if err != nil {
		return err
	}
	t.Root = root
	x.mutableCatalog()
	x.cat.tables[strings.ToLower(t.Name)] = t
	if err := saveEntry(x.v, tableKey(t.Name), t); err != nil {
		return err
	}
	for _, a := range autos {
		name := fmt.Sprintf("bangdb_autoindex_%s_%s", t.Name, strings.Join(a.cols, "_"))
		if a.pk {
			name = fmt.Sprintf("bangdb_autoindex_%s_pk", t.Name)
		}
		if x.cat.index(name) != nil {
			continue // e.g. UNIQUE (a) next to a UNIQUE column a
		}
		if err := x.addIndex(&Index{Name: name, Table: t.Name, Columns: a.cols, Unique: true, Auto: true}); err != nil {
			return err
		}
	}
	x.cat.link()
	if len(st.ForeignKeys) > 0 || hasReferences(st) {
		if err := x.buildForeignKeys(t, st); err != nil {
			return err
		}
		if err := saveEntry(x.v, tableKey(t.Name), t); err != nil {
			return err
		}
		x.cat.link()
	}
	return nil
}

func hasReferences(st *sql.CreateTable) bool {
	for _, cd := range st.Columns {
		if cd.References != nil {
			return true
		}
	}
	return false
}

func columnFromDef(cd sql.ColumnDef) (Column, error) {
	col := Column{Name: cd.Name, Type: value.ParseType(cd.Type), DeclType: cd.Type,
		NotNull: cd.NotNull, PrimaryKey: cd.PrimaryKey, Unique: cd.Unique, AutoIncrement: cd.AutoIncrement}
	if cd.Collate != "BINARY" {
		col.Collate = cd.Collate
	}
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
		if x.cat.view(st.Name) != nil {
			return fmt.Errorf("bangdb: %s is a view (use DROP VIEW)", st.Name)
		}
		return fmt.Errorf("bangdb: no such table: %s", st.Name)
	}
	if refs := x.cat.referencingTables(t); len(refs) > 0 {
		return fmt.Errorf("bangdb: cannot drop table %s: it is referenced by a foreign key of %s", t.Name, strings.Join(refs, ", "))
	}
	x.mutableCatalog()
	t = x.cat.table(st.Name)
	for _, ix := range t.indexes {
		if err := ix.kind.DropIndex(x.v, ix.Root); err != nil {
			return err
		}
		if err := deleteEntry(x.v, indexKey(ix.Name)); err != nil {
			return err
		}
		delete(x.cat.indexes, strings.ToLower(ix.Name))
	}
	if err := t.kind.Drop(x.v, t.Root); err != nil {
		return err
	}
	if err := deleteEntry(x.v, tableKey(t.Name)); err != nil {
		return err
	}
	delete(x.cat.tables, strings.ToLower(t.Name))
	delete(x.tx.rowids, strings.ToLower(t.Name))
	if err := x.dropSequence(t); err != nil {
		return err
	}
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
	var cols []string
	for _, name := range st.Columns {
		i := t.ColIndex(name)
		if i < 0 {
			return fmt.Errorf("bangdb: no such column: %s.%s", t.Name, name)
		}
		for _, c := range cols {
			if strings.EqualFold(c, name) {
				return fmt.Errorf("bangdb: column %s listed twice in index %s", name, st.Name)
			}
		}
		cols = append(cols, t.Columns[i].Name)
	}
	x.mutableCatalog()
	if err := x.addIndex(&Index{Name: st.Name, Table: t.Name, Columns: cols, Unique: st.Unique}); err != nil {
		return err
	}
	x.cat.link()
	return nil
}

// addIndex creates the index tree, fills it from existing rows and stores it
// in the catalog. The catalog must already be mutable.
func (x *execCtx) addIndex(ix *Index) error {
	t := x.cat.table(ix.Table)
	ix.resolve(t)
	root, err := t.kind.CreateIndex(x.v)
	if err != nil {
		return err
	}
	ix.Root = root
	entries := x.index(ix)
	err = x.scanAll(t, func(rowid int64, row []any) (bool, error) {
		if err := x.checkUnique(t, ix, row, rowid); err != nil {
			return false, err
		}
		k, err := indexEntry(ix, row, rowid)
		if err != nil {
			return false, err
		}
		return true, entries.Put(k)
	})
	if err != nil {
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
	if err := ix.kind.DropIndex(x.v, ix.Root); err != nil {
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
	if st.Column.Check != nil || st.Column.References != nil {
		return fmt.Errorf("bangdb: cannot add a column with a CHECK or REFERENCES constraint")
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
	if err := t.configure(); err != nil {
		return err
	}
	x.cat.link()
	return saveEntry(x.v, tableKey(t.Name), t)
}

// saveTable rewrites the catalog entries of t and its indexes.
func (x *execCtx) saveTable(t *Table) error {
	if err := saveEntry(x.v, tableKey(t.Name), t); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		if err := saveEntry(x.v, indexKey(ix.Name), ix); err != nil {
			return err
		}
	}
	return nil
}

func (x *execCtx) renameTable(st *sql.AlterTableRename) error {
	t, err := x.table(st.Table)
	if err != nil {
		return err
	}
	if x.cat.table(st.NewName) != nil || x.cat.view(st.NewName) != nil {
		return fmt.Errorf("bangdb: there is already a table or view named %s", st.NewName)
	}
	if strings.HasPrefix(strings.ToLower(st.NewName), "bangdb_") {
		return fmt.Errorf("bangdb: table names starting with bangdb_ are reserved")
	}
	x.mutableCatalog()
	t = x.cat.table(st.Table)
	old := t.Name
	if err := deleteEntry(x.v, tableKey(old)); err != nil {
		return err
	}
	delete(x.cat.tables, strings.ToLower(old))
	t.Name = st.NewName
	x.cat.tables[strings.ToLower(t.Name)] = t
	oldPrefix := strings.ToLower("bangdb_autoindex_" + old + "_")
	for _, ix := range t.indexes {
		ix.Table = t.Name
		if ix.Auto && strings.HasPrefix(strings.ToLower(ix.Name), oldPrefix) {
			// Keep automatic index names tied to the table name, so that a
			// new table with the old name can create its own.
			if err := deleteEntry(x.v, indexKey(ix.Name)); err != nil {
				return err
			}
			delete(x.cat.indexes, strings.ToLower(ix.Name))
			ix.Name = "bangdb_autoindex_" + t.Name + "_" + ix.Name[len(oldPrefix):]
			x.cat.indexes[strings.ToLower(ix.Name)] = ix
		}
	}
	// Move the AUTOINCREMENT high-water mark.
	if hw, err := x.sequence(&Table{Name: old, RowidCol: t.RowidCol, Columns: t.Columns}); err != nil {
		return err
	} else if hw > 0 {
		if err := x.dropSequence(&Table{Name: old}); err != nil {
			return err
		}
		if err := x.recordSequence(t, hw); err != nil {
			return err
		}
	}
	if id, ok := x.tx.rowids[strings.ToLower(old)]; ok {
		delete(x.tx.rowids, strings.ToLower(old))
		x.tx.rowids[strings.ToLower(t.Name)] = id
	}
	// Foreign keys of other tables (and self references) follow the rename.
	for _, ref := range t.referencedBy {
		ref.fk.RefTable = t.Name
		if ref.child != t {
			if err := saveEntry(x.v, tableKey(ref.child.Name), ref.child); err != nil {
				return err
			}
		}
	}
	x.cat.link()
	return x.saveTable(t)
}

func (x *execCtx) renameColumn(st *sql.AlterTableRenameColumn) error {
	t, err := x.table(st.Table)
	if err != nil {
		return err
	}
	i := t.ColIndex(st.Old)
	if i < 0 {
		return fmt.Errorf("bangdb: no such column: %s.%s", t.Name, st.Old)
	}
	if j := t.ColIndex(st.New); j >= 0 && j != i {
		return fmt.Errorf("bangdb: duplicate column name: %s", st.New)
	}
	x.mutableCatalog()
	t = x.cat.table(st.Table)
	old := t.Columns[i].Name
	t.Columns[i].Name = st.New
	for k, c := range t.PrimaryKey {
		if strings.EqualFold(c, old) {
			t.PrimaryKey[k] = st.New
		}
	}
	for _, ix := range t.indexes {
		for k, c := range ix.Columns {
			if strings.EqualFold(c, old) {
				ix.Columns[k] = st.New
			}
		}
	}
	for k := range t.ForeignKeys {
		renameIn(t.ForeignKeys[k].Columns, old, st.New)
	}
	renameInChecks(t, old, st.New)
	for k, v := range t.StoreOptions { // e.g. a time-series table's time_column
		if strings.EqualFold(v, old) {
			t.StoreOptions[k] = st.New
		}
	}
	if err := t.configure(); err != nil {
		return err
	}
	for _, ref := range t.referencedBy {
		renameIn(ref.fk.RefColumns, old, st.New)
		if ref.child != t {
			if err := saveEntry(x.v, tableKey(ref.child.Name), ref.child); err != nil {
				return err
			}
		}
	}
	x.cat.link()
	return x.saveTable(t)
}

func (x *execCtx) dropColumn(st *sql.AlterTableDropColumn) error {
	t, err := x.table(st.Table)
	if err != nil {
		return err
	}
	i := t.ColIndex(st.Column)
	if i < 0 {
		return fmt.Errorf("bangdb: no such column: %s.%s", t.Name, st.Column)
	}
	if len(t.Columns) == 1 {
		return fmt.Errorf("bangdb: cannot drop the only column of %s", t.Name)
	}
	if t.Columns[i].PrimaryKey {
		return fmt.Errorf("bangdb: cannot drop PRIMARY KEY column %s", t.Columns[i].Name)
	}
	if why := columnInUse(t, i); why != "" {
		return fmt.Errorf("bangdb: cannot drop column %s: %s", t.Columns[i].Name, why)
	}
	if t.storeCfg != nil { // the storage kind must accept the new columns
		tmp := *t
		tmp.Columns = append(t.Columns[:i:i], t.Columns[i+1:]...)
		if err := tmp.configure(); err != nil {
			return fmt.Errorf("bangdb: cannot drop column %s: %w", t.Columns[i].Name, err)
		}
	}
	for _, ix := range t.indexes {
		for _, c := range ix.cols {
			if c == i {
				if ix.Auto {
					return fmt.Errorf("bangdb: cannot drop UNIQUE column %s", t.Columns[i].Name)
				}
				return fmt.Errorf("bangdb: cannot drop column %s: it is used by index %s (drop the index first)", t.Columns[i].Name, ix.Name)
			}
		}
	}
	// Rewrite every row without the column (rows are collected first).
	type rec struct {
		rowid int64
		row   []any
	}
	var rows []rec
	err = x.scanAll(t, func(rowid int64, row []any) (bool, error) {
		rows = append(rows, rec{rowid, append(row[:i:i], row[i+1:]...)})
		return true, nil
	})
	if err != nil {
		return err
	}
	stored := x.rows(t)
	for _, r := range rows {
		if err := stored.Put(r.rowid, r.row); err != nil {
			return err
		}
	}
	x.mutableCatalog()
	t = x.cat.table(st.Table)
	t.Columns = append(t.Columns[:i:i], t.Columns[i+1:]...)
	if t.RowidCol > i {
		t.RowidCol--
	}
	if err := t.configure(); err != nil { // the time column may have moved
		return err
	}
	x.cat.link()
	return x.saveTable(t)
}
