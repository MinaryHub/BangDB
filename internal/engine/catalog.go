package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
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
	DefaultSQL    string     `json:"default,omitempty"`

	defExpr  sql.Expr // parsed DefaultSQL
	defConst any      // value used to pad rows written before ADD COLUMN
}

// Table describes a table. Rows are stored in a B+tree keyed by rowid.
type Table struct {
	Name     string   `json:"name"`
	Columns  []Column `json:"columns"`
	Root     uint32   `json:"root"`
	RowidCol int      `json:"rowidCol"` // INTEGER PRIMARY KEY column, or -1

	indexes []*Index
}

// Index is a secondary index on a single column. Entries are keyed by
// key(value) || rowid and have empty values.
type Index struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Column string `json:"column"`
	Unique bool   `json:"unique,omitempty"`
	Auto   bool   `json:"auto,omitempty"` // created for UNIQUE / PRIMARY KEY
	Root   uint32 `json:"root"`

	col int
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
	tables  map[string]*Table
	indexes map[string]*Index
}

func newCatalog() *Catalog {
	return &Catalog{tables: map[string]*Table{}, indexes: map[string]*Index{}}
}

func (c *Catalog) table(name string) *Table { return c.tables[strings.ToLower(name)] }
func (c *Catalog) index(name string) *Index { return c.indexes[strings.ToLower(name)] }

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
		nt.indexes = nil
		n.tables[k] = &nt
	}
	for k, ix := range c.indexes {
		ni := *ix
		n.indexes[k] = &ni
	}
	n.link()
	return n
}

// link rebuilds the derived table->index pointers.
func (c *Catalog) link() {
	for _, t := range c.tables {
		t.indexes = t.indexes[:0]
	}
	for _, ix := range c.indexes {
		t := c.table(ix.Table)
		if t == nil {
			continue
		}
		ix.col = t.ColIndex(ix.Column)
		t.indexes = append(t.indexes, ix)
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
			c.tables[strings.ToLower(t.Name)] = t
		case 'i':
			ix := &Index{}
			if err := json.Unmarshal(data, ix); err != nil {
				return nil, fmt.Errorf("bangdb: corrupt catalog entry %q: %w", k, err)
			}
			c.indexes[strings.ToLower(ix.Name)] = ix
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
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE %s (\n", t.Name)
	for i, col := range t.Columns {
		fmt.Fprintf(&sb, "  %s", col.Name)
		if col.DeclType != "" {
			sb.WriteString(" " + col.DeclType)
		}
		if col.PrimaryKey {
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
		if i < len(t.Columns)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString(");")
	for _, ix := range t.indexes {
		if ix.Auto {
			continue
		}
		u := ""
		if ix.Unique {
			u = "UNIQUE "
		}
		fmt.Fprintf(&sb, "\nCREATE %sINDEX %s ON %s (%s);", u, ix.Name, t.Name, ix.Column)
	}
	return sb.String()
}
