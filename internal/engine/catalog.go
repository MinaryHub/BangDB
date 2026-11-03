package engine

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

// metaCatalogRoot is the header meta slot holding the catalog tree root.
const metaCatalogRoot = 0

// Column describes a table column.
type Column struct {
	Name          string     `json:"name"`
	Type          value.Type `json:"type"`
	DeclType      string     `json:"decl,omitempty"`
	NotNull       bool       `json:"notNull,omitempty"`
	PrimaryKey    bool       `json:"pk,omitempty"`
	Unique        bool       `json:"unique,omitempty"`
	AutoIncrement bool       `json:"autoinc,omitempty"`
	Collate       string     `json:"collate,omitempty"` // NOCASE, RTRIM or "" (binary)
	DefaultSQL    string     `json:"default,omitempty"`

	defExpr  sql.Expr // parsed DefaultSQL
	defConst any      // value used to pad rows written before ADD COLUMN
}

// Table describes a table. Rows are stored in a B+tree keyed by rowid.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	Root    uint32   `json:"root"`
	// Store names the storage kind of the rows ("" = the B+tree row
	// store; see package store).
	Store string `json:"store,omitempty"`
	// StoreOptions are the kind's options (USING kind WITH (...)).
	StoreOptions map[string]string `json:"storeOptions,omitempty"`
	RowidCol     int               `json:"rowidCol"` // INTEGER PRIMARY KEY column, or -1
	// PrimaryKey lists the primary key columns when the key is composite
	// (single-column keys are flagged on the column).
	PrimaryKey []string `json:"primaryKey,omitempty"`
	// Checks are CHECK constraints as SQL expressions.
	Checks      []string     `json:"checks,omitempty"`
	ForeignKeys []ForeignKey `json:"foreignKeys,omitempty"`

	kind         store.Kind // resolved Store
	storeCfg     any        // configuration of a store.ConfigurableKind
	timeCol      int        // time column of a time-keyed kind, or -1
	indexes      []*Index
	checkFns     []evalFn // compiled Checks
	referencedBy []fkRef  // foreign keys of other tables pointing here
}

// Index is a secondary index on one or more columns. Entries are keyed by
// key(v1) || key(v2) || ... || rowid and have empty values; the value keys
// are prefix-free, so any leading subset of the columns can be searched.
type Index struct {
	Name    string   `json:"name"`
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Column  string   `json:"column,omitempty"` // legacy single-column form
	Unique  bool     `json:"unique,omitempty"`
	Auto    bool     `json:"auto,omitempty"` // created for UNIQUE / PRIMARY KEY
	Root    uint32   `json:"root"`
	// Stats (from ANALYZE): Stats[k] is the average number of entries
	// sharing the values of the first k+1 indexed columns.
	Stats []float64 `json:"stats,omitempty"`
	// Where makes a partial index: only rows for which it is true have
	// entries.
	Where string `json:"where,omitempty"`

	pred  evalFn // compiled Where
	defs  []any  // the table's column defaults, for rows shorter than it
	cols  []int
	colls []string   // collation per column: keys of collated text are folded
	kind  store.Kind // the table's storage kind, which also stores its indexes
}

// key returns the index key part for row (without the rowid), truncated if
// it is long (see truncKey).
func (ix *Index) key(row []any) []byte {
	k, _ := truncKey(ix.fullKey(row))
	return k
}

// fullKey is the untruncated key part for row.
func (ix *Index) fullKey(row []any) []byte {
	var k []byte
	for i, c := range ix.cols {
		k = value.AppendKey(k, foldValue(row[c], ix.colls[i]))
	}
	return k
}

// keyLimit is the longest index key part stored as is. A longer key part is
// stored as its first keyLimit bytes, a 0x00 byte and an 8-byte hash of the
// whole part; a stored part longer than keyLimit marks such a key.
//
// Keys sharing their first keyLimit bytes form a group: groups keep the
// order of the values (the cut is monotonic) and are contiguous, but inside
// a group entries are ordered by hash. So an equality search on the whole
// key finds its entries by hash (and compares the rows' values, as hashes
// can collide), a search on part of the key or a range uses the group
// prefix (groupKey) and lets the level's filters recheck, and ordered scans
// sort each group by value (keyOrder).
const keyLimit = btree.MaxKey - 8 - 9

// truncKey returns the stored form of a whole key part.
func truncKey(k []byte) ([]byte, bool) {
	if len(k) <= keyLimit {
		return k, false
	}
	h := fnv.New64a()
	h.Write(k)
	return h.Sum(append(k[:keyLimit:keyLimit], 0)), true
}

// groupKey returns the search prefix for a partial key or a range bound: a
// cut key stands for its whole group.
func groupKey(k []byte) ([]byte, bool) {
	if len(k) <= keyLimit {
		return k, false
	}
	return append(k[:keyLimit:keyLimit], 0), true
}

func isTruncated(part []byte) bool { return len(part) > keyLimit }

// truncGroup returns the group prefix of a stored (truncated) key part.
func truncGroup(part []byte) []byte { return part[:keyLimit+1] }

// hasNull reports whether any indexed value of row is NULL (such rows never
// conflict in a UNIQUE index).
func (ix *Index) hasNull(row []any) bool {
	for _, c := range ix.cols {
		if row[c] == nil {
			return true
		}
	}
	return false
}

func (ix *Index) columnList() string { return quoteIdents(ix.Columns) }

func quoteIdents(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = sql.QuoteIdent(n)
	}
	return strings.Join(q, ", ")
}

// spec describes t to its storage kind.
func (t *Table) spec() store.TableSpec {
	s := store.TableSpec{RowidColumn: t.RowidCol, Options: t.StoreOptions}
	for _, c := range t.Columns {
		s.Columns = append(s.Columns, store.ColumnSpec{Name: c.Name, Type: string(c.Type), NotNull: c.NotNull})
	}
	return s
}

// configure validates t for its storage kind (after it was created or its
// columns changed) and keeps the kind's configuration.
func (t *Table) configure() error {
	t.storeCfg, t.timeCol = nil, -1
	ck, ok := t.kind.(store.ConfigurableKind)
	if !ok {
		if t.kind.Volatile() {
			return checkVolatileOptions(t)
		}
		if len(t.StoreOptions) > 0 {
			return fmt.Errorf("bangdb: storage kind %s takes no options", kindName(t.Store))
		}
		return nil
	}
	cfg, err := ck.Configure(t.spec())
	if err != nil {
		return err
	}
	t.storeCfg = cfg
	// The time column is part of the configuration: no view is needed.
	if tk, ok := ck.OpenConfigured(nil, 0, cfg).(store.TimeKeyer); ok {
		t.timeCol = tk.TimeColumn()
	}
	return nil
}

func kindName(s string) string {
	if s == "" {
		return "btree"
	}
	return s
}

// ColIndex returns the column position by name (case-insensitive), or -1.
func (t *Table) ColIndex(name string) int {
	for i, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// Catalog is the in-memory schema. A committed catalog is immutable; write
// transactions clone it before applying DDL.
type Catalog struct {
	tables   map[string]*Table
	indexes  map[string]*Index
	views    map[string]*View
	triggers map[string]*Trigger
}

func newCatalog() *Catalog {
	return &Catalog{tables: map[string]*Table{}, indexes: map[string]*Index{}, views: map[string]*View{}, triggers: map[string]*Trigger{}}
}

// ForeignKey makes Columns of a table refer to RefColumns (a primary key or
// UNIQUE constraint) of RefTable.
type ForeignKey struct {
	Columns    []string `json:"columns"`
	RefTable   string   `json:"refTable"`
	RefColumns []string `json:"refColumns"`
	OnDelete   string   `json:"onDelete,omitempty"` // "", NO ACTION, RESTRICT, CASCADE, SET NULL
	OnUpdate   string   `json:"onUpdate,omitempty"`

	cols []int
}

type fkRef struct {
	child *Table
	fk    *ForeignKey
}

// View is a named SELECT.
type View struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns,omitempty"`
	SQL     string   `json:"sql"`

	sel *sql.Select
}

func (c *Catalog) view(name string) *View { return c.views[strings.ToLower(name)] }

// ViewNames returns the view names in the catalog.
func (c *Catalog) ViewNames() []string {
	var out []string
	for _, v := range c.views {
		out = append(out, v.Name)
	}
	return out
}

func viewKey(name string) []byte { return []byte("v\x00" + strings.ToLower(name)) }

func (v *View) prepare() error {
	stmts, _, err := sql.Parse(v.SQL)
	if err != nil {
		return fmt.Errorf("bangdb: view %s: %w", v.Name, err)
	}
	sel, ok := stmts[0].(*sql.Select)
	if len(stmts) != 1 || !ok {
		return fmt.Errorf("bangdb: view %s is not a single SELECT", v.Name)
	}
	v.sel = sel
	return nil
}

func (c *Catalog) table(name string) *Table { return c.tables[strings.ToLower(name)] }
func (c *Catalog) index(name string) *Index { return c.indexes[strings.ToLower(name)] }

// Table returns the table called name (case-insensitive), or nil. The
// table must not be modified.
func (c *Catalog) Table(name string) *Table { return c.table(name) }

// Indexes returns the table's indexes, including those created for UNIQUE
// and PRIMARY KEY constraints.
func (t *Table) Indexes() []*Index { return append([]*Index(nil), t.indexes...) }

// TableNames returns the table names in the catalog.
func (c *Catalog) TableNames() []string {
	var out []string
	for _, t := range c.tables {
		out = append(out, t.Name)
	}
	return out
}

func (c *Catalog) clone() *Catalog {
	n := newCatalog()
	for k, t := range c.tables {
		nt := *t
		nt.Columns = append([]Column(nil), t.Columns...)
		nt.PrimaryKey = append([]string(nil), t.PrimaryKey...)
		if t.StoreOptions != nil {
			nt.StoreOptions = map[string]string{}
			for k, v := range t.StoreOptions {
				nt.StoreOptions[k] = v
			}
		}
		nt.Checks = append([]string(nil), t.Checks...)
		nt.ForeignKeys = nil
		for _, fk := range t.ForeignKeys {
			fk.Columns = append([]string(nil), fk.Columns...)
			fk.RefColumns = append([]string(nil), fk.RefColumns...)
			fk.cols = nil
			nt.ForeignKeys = append(nt.ForeignKeys, fk)
		}
		nt.indexes, nt.checkFns, nt.referencedBy = nil, nil, nil
		n.tables[k] = &nt
	}
	for k, tr := range c.triggers {
		n.triggers[k] = tr // immutable once created
	}
	for k, v := range c.views {
		nv := *v
		n.views[k] = &nv
	}
	for k, ix := range c.indexes {
		ni := *ix
		ni.Columns = append([]string(nil), ix.Columns...)
		ni.cols, ni.colls = nil, nil
		n.indexes[k] = &ni
	}
	n.link()
	return n
}

// link rebuilds the derived table->index pointers.
func (c *Catalog) link() {
	for _, t := range c.tables {
		t.indexes = t.indexes[:0]
		t.referencedBy = t.referencedBy[:0]
	}
	for _, t := range c.tables {
		for i := range t.ForeignKeys {
			fk := &t.ForeignKeys[i]
			fk.cols = fk.cols[:0]
			for _, name := range fk.Columns {
				fk.cols = append(fk.cols, t.ColIndex(name))
			}
			if p := c.table(fk.RefTable); p != nil {
				p.referencedBy = append(p.referencedBy, fkRef{t, fk})
			}
		}
		t.compileChecks()
	}
	for _, ix := range c.indexes {
		t := c.table(ix.Table)
		if t == nil {
			continue
		}
		ix.resolve(t)
		t.indexes = append(t.indexes, ix)
	}
}

func (ix *Index) resolve(t *Table) {
	if len(ix.Columns) == 0 && ix.Column != "" {
		ix.Columns, ix.Column = []string{ix.Column}, ""
	}
	ix.kind = t.kind
	ix.defs, ix.pred = nil, nil
	if ix.Where != "" {
		for _, c := range t.Columns {
			ix.defs = append(ix.defs, c.defConst)
		}
		f, err := compileCheck(t, ix.Where)
		if err != nil {
			err := fmt.Errorf("bangdb: WHERE of index %s: %w", ix.Name, err)
			f = func(*env) (any, error) { return nil, err }
		}
		ix.pred = f
	}
	ix.cols, ix.colls = ix.cols[:0], ix.colls[:0]
	for _, c := range ix.Columns {
		i := t.ColIndex(c)
		ix.cols = append(ix.cols, i)
		coll := ""
		if i >= 0 {
			coll = t.Columns[i].Collate
		}
		ix.colls = append(ix.colls, coll)
	}
}

func tableKey(name string) []byte { return []byte("t\x00" + strings.ToLower(name)) }
func indexKey(name string) []byte { return []byte("i\x00" + strings.ToLower(name)) }

func loadCatalog(v *pager.View) (*Catalog, error) {
	c := newCatalog()
	root := v.Meta(metaCatalogRoot)
	if root == 0 {
		return c, nil
	}
	tr := btree.Open(v, root)
	cur := tr.Seek(nil)
	for ; cur.Valid(); cur.Next() {
		k := cur.Key()
		data, err := cur.Value()
		if err != nil {
			return nil, err
		}
		switch k[0] {
		case 't':
			t := &Table{}
			if err := json.Unmarshal(data, t); err != nil {
				return nil, fmt.Errorf("bangdb: corrupt catalog entry %q: %w", k, err)
			}
			for i := range t.Columns {
				if err := t.Columns[i].prepareDefault(); err != nil {
					return nil, err
				}
			}
			kind, err := store.Lookup(t.Store)
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", t.Name, err)
			}
			t.kind = kind
			if err := t.configure(); err != nil {
				return nil, fmt.Errorf("table %s: %w", t.Name, err)
			}
			c.tables[strings.ToLower(t.Name)] = t
		case 'i':
			ix := &Index{}
			if err := json.Unmarshal(data, ix); err != nil {
				return nil, fmt.Errorf("bangdb: corrupt catalog entry %q: %w", k, err)
			}
			c.indexes[strings.ToLower(ix.Name)] = ix
		case 'v':
			v := &View{}
			if err := json.Unmarshal(data, v); err != nil {
				return nil, fmt.Errorf("bangdb: corrupt catalog entry %q: %w", k, err)
			}
			if err := v.prepare(); err != nil {
				return nil, err
			}
			c.views[strings.ToLower(v.Name)] = v
		case 'g':
			tr := &Trigger{}
			if err := json.Unmarshal(data, tr); err != nil {
				return nil, fmt.Errorf("bangdb: corrupt catalog entry %q: %w", k, err)
			}
			if err := tr.prepare(); err != nil {
				return nil, err
			}
			c.triggers[strings.ToLower(tr.Name)] = tr
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	c.link()
	return c, nil
}

func (col *Column) prepareDefault() error {
	col.defExpr, col.defConst = nil, nil
	if col.DefaultSQL == "" {
		return nil
	}
	e, err := sql.ParseExpr(col.DefaultSQL)
	if err != nil {
		return fmt.Errorf("bangdb: bad default for column %s: %w", col.Name, err)
	}
	col.defExpr = e
	if v, ok := constValue(e); ok {
		if v, err = value.Coerce(v, col.Type); err == nil {
			col.defConst = v
		}
	}
	return nil
}

// constValue evaluates literal-only expressions such as 1, -2.5 or 'x'.
func constValue(e sql.Expr) (any, bool) {
	switch x := e.(type) {
	case *sql.Literal:
		return x.Value, true
	case *sql.Unary:
		v, ok := constValue(x.X)
		if !ok || x.Op != "-" {
			return nil, false
		}
		switch n := v.(type) {
		case int64:
			return -n, true
		case float64:
			return -n, true
		}
	}
	return nil, false
}

func catalogTree(v *pager.View) (*btree.Tree, error) {
	root := v.Meta(metaCatalogRoot)
	if root == 0 {
		var err error
		if root, err = btree.Create(v); err != nil {
			return nil, err
		}
		v.SetMeta(metaCatalogRoot, root)
	}
	return btree.Open(v, root), nil
}

func saveEntry(v *pager.View, key []byte, obj any) error {
	tr, err := catalogTree(v)
	if err != nil {
		return err
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return tr.Put(key, data)
}

func deleteEntry(v *pager.View, key []byte) error {
	tr, err := catalogTree(v)
	if err != nil {
		return err
	}
	_, err = tr.Delete(key)
	return err
}

// Describe renders a table (and its user-created indexes) as SQL.
func (c *Catalog) Describe(name string) string {
	t := c.table(name)
	if t == nil {
		return ""
	}
	out := t.createSQL()
	for _, ix := range t.indexes {
		if !ix.Auto {
			out += "\n" + ix.createSQL()
		}
	}
	return out
}

// createSQL is the CREATE TABLE statement of t (without its own indexes).
func (t *Table) createSQL() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE %s (\n", sql.QuoteIdent(t.Name))
	for i, col := range t.Columns {
		fmt.Fprintf(&sb, "  %s", sql.QuoteIdent(col.Name))
		if col.DeclType != "" {
			sb.WriteString(" " + col.DeclType)
		}
		if col.PrimaryKey && len(t.PrimaryKey) == 0 {
			sb.WriteString(" PRIMARY KEY")
		}
		if col.AutoIncrement {
			sb.WriteString(" AUTOINCREMENT")
		}
		if col.NotNull {
			sb.WriteString(" NOT NULL")
		}
		if col.Unique {
			sb.WriteString(" UNIQUE")
		}
		if col.DefaultSQL != "" {
			sb.WriteString(" DEFAULT " + col.DefaultSQL)
		}
		if col.Collate != "" {
			sb.WriteString(" COLLATE " + col.Collate)
		}
		if i < len(t.Columns)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	var constraints []string
	if len(t.PrimaryKey) > 0 {
		constraints = append(constraints, "PRIMARY KEY ("+quoteIdents(t.PrimaryKey)+")")
	}
	for _, ix := range t.indexes {
		if ix.Auto && len(ix.Columns) > 1 && !strings.HasSuffix(ix.Name, "_pk") {
			constraints = append(constraints, "UNIQUE ("+ix.columnList()+")")
		}
	}
	for _, chk := range t.Checks {
		constraints = append(constraints, "CHECK ("+chk+")")
	}
	for _, fk := range t.ForeignKeys {
		s := fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s)", quoteIdents(fk.Columns), sql.QuoteIdent(fk.RefTable), quoteIdents(fk.RefColumns))
		if fk.OnDelete != "" {
			s += " ON DELETE " + fk.OnDelete
		}
		if fk.OnUpdate != "" {
			s += " ON UPDATE " + fk.OnUpdate
		}
		constraints = append(constraints, s)
	}
	if len(constraints) > 0 {
		out := strings.TrimSuffix(sb.String(), "\n")
		sb.Reset()
		sb.WriteString(out + ",\n  " + strings.Join(constraints, ",\n  ") + "\n")
	}
	sb.WriteString(")")
	if t.Store != "" {
		sb.WriteString(" USING " + t.Store)
	}
	if len(t.StoreOptions) > 0 {
		var opts []string
		for k, v := range t.StoreOptions {
			if _, err := strconv.ParseFloat(v, 64); err != nil && !isIdent(v) {
				v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
			}
			opts = append(opts, k+" = "+v)
		}
		sort.Strings(opts)
		sb.WriteString(" WITH (" + strings.Join(opts, ", ") + ")")
	}
	sb.WriteString(";")
	return sb.String()
}

// createSQL is the CREATE INDEX statement of ix.
func (ix *Index) createSQL() string {
	u := ""
	if ix.Unique {
		u = "UNIQUE "
	}
	where := ""
	if ix.Where != "" {
		where = " WHERE " + ix.Where
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)%s;", u, sql.QuoteIdent(ix.Name), sql.QuoteIdent(ix.Table), ix.columnList(), where)
}

func isIdent(s string) bool {
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}
