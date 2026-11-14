package engine

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// Closure tables (transitive closure of a hierarchy):
//
//	CREATE CLOSURE subclass_closure ON subclass (child, parent);
//
// creates a table subclass_closure (anc, des, depth) that always holds, for
// every node des and every node anc reachable from it by following
// child -> parent edges, the length of the shortest such path (depth), plus
// (des, des, 0) for every node that is a child in some row. The source may
// be a tree table (class (id, parent), parent NULL at the roots) or an edge
// table with several rows per child (multiple inheritance, a DAG). Questions
// about the hierarchy then become index lookups:
//
//	SELECT des FROM subclass_closure WHERE anc = ?   -- everything under a class
//	SELECT anc FROM subclass_closure WHERE des = ?   -- everything above it
//
// The closure table has the storage kind of its source (a memory source
// gets a memory closure) and is read-only for statements; changes to the source
// (INSERT, UPDATE of the child or parent column, DELETE, foreign key actions)
// are recorded per row and applied when the statement finishes: the changed
// children and everything below them get their ancestors recomputed, top
// down, from their parents' rows. An edge that would make a cycle fails the
// statement. While a statement runs (and in the bodies of its triggers) the
// closure still shows the state before it.

// Closure is a catalog entry.
type Closure struct {
	Name   string `json:"name"` // also the name of its table
	Source string `json:"source"`
	Child  string `json:"child"`
	Parent string `json:"parent"`
	// DesIndex is the index on (des, anc, depth) of the closure table;
	// ChildIndex the index on the source's child column created for the
	// closure ("" when the source had one already).
	DesIndex   string `json:"desIndex"`
	ChildIndex string `json:"childIndex,omitempty"`
}

func closureKey(name string) []byte { return []byte("h\x00" + strings.ToLower(name)) }

func (c *Catalog) closure(name string) *Closure { return c.closures[strings.ToLower(name)] }

// ClosureNames returns the closure names, sorted.
func (c *Catalog) ClosureNames() []string {
	var out []string
	for _, cl := range c.closures {
		out = append(out, cl.Name)
	}
	sort.Strings(out)
	return out
}

// DescribeClosure returns the CREATE CLOSURE statement of a closure.
func (c *Catalog) DescribeClosure(name string) string {
	cl := c.closure(name)
	if cl == nil {
		return ""
	}
	return fmt.Sprintf("CREATE CLOSURE %s ON %s (%s, %s);", sql.QuoteIdent(cl.Name),
		sql.QuoteIdent(cl.Source), sql.QuoteIdent(cl.Child), sql.QuoteIdent(cl.Parent))
}

// closuresOf returns the closures whose source is table t.
func (c *Catalog) closuresOf(t *Table) []*Closure {
	var out []*Closure
	for _, cl := range c.closures {
		if strings.EqualFold(cl.Source, t.Name) {
			out = append(out, cl)
		}
	}
	return out
}

// ---- DDL ----

func (x *execCtx) createClosure(st *sql.CreateClosure) error {
	if x.cat.closure(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("bangdb: closure %s already exists", st.Name)
	}
	if x.cat.table(st.Name) != nil || x.cat.view(st.Name) != nil {
		return fmt.Errorf("bangdb: there is already a table or view named %s", st.Name)
	}
	src := x.cat.table(st.Table)
	if src == nil {
		return fmt.Errorf("bangdb: no such table: %s", st.Table)
	}
	switch {
	case x.cat.closure(src.Name) != nil:
		return fmt.Errorf("bangdb: %s is a closure table; build the closure over its source instead", src.Name)
	case src.timeCol >= 0:
		return fmt.Errorf("bangdb: a %s table cannot be the source of a closure", src.Store)
	}
	ci, pi := src.ColIndex(st.Child), src.ColIndex(st.Parent)
	if ci < 0 {
		return fmt.Errorf("bangdb: no such column: %s.%s", src.Name, st.Child)
	}
	if pi < 0 {
		return fmt.Errorf("bangdb: no such column: %s.%s", src.Name, st.Parent)
	}
	if ci == pi {
		return fmt.Errorf("bangdb: the child and parent columns of a closure must differ")
	}
	cl := &Closure{Name: st.Name, Source: src.Name, Child: src.Columns[ci].Name, Parent: src.Columns[pi].Name}
	typ := src.Columns[ci].DeclType
	if typ == "" {
		typ = "ANY"
	}
	ct := &sql.CreateTable{
		Name: st.Name,
		Columns: []sql.ColumnDef{
			{Name: "anc", Type: typ, NotNull: true},
			{Name: "des", Type: typ, NotNull: true},
			{Name: "depth", Type: "INTEGER", NotNull: true},
		},
		PrimaryKey: []string{"anc", "des"},
		// the same kind as the source: a memory source gets a memory
		// closure, so both are empty (or restored) together after a restart
		Using:        src.Store,
		UsingOptions: src.StoreOptions,
	}
	y := x.closureCtx(nil)
	if err := y.createTable(ct); err != nil {
		return err
	}
	cl.DesIndex = st.Name + "_des"
	if err := y.createIndex(&sql.CreateIndex{Name: cl.DesIndex, Table: st.Name, Columns: []string{"des", "anc", "depth"}}); err != nil {
		return err
	}
	if src.RowidCol != ci && !hasLeadingIndex(src, ci) {
		cl.ChildIndex = st.Name + "_" + cl.Child
		if err := y.createIndex(&sql.CreateIndex{Name: cl.ChildIndex, Table: src.Name, Columns: []string{cl.Child}}); err != nil {
			return err
		}
	}
	x.cat = y.cat // the DDL above cloned the catalog
	x.mutableCatalog()
	x.cat.closures[strings.ToLower(cl.Name)] = cl
	if err := saveEntry(x.v, closureKey(cl.Name), cl); err != nil {
		return err
	}
	return x.refreshClosure(cl, nil)
}

// hasLeadingIndex reports whether t has an index whose first key part is
// column c.
func hasLeadingIndex(t *Table, c int) bool {
	for _, ix := range t.indexes {
		if len(ix.cols) > 0 && ix.cols[0] == c && ix.pred == nil && (len(ix.exprs) == 0 || ix.exprs[0] == nil) {
			return true
		}
	}
	return false
}

func (x *execCtx) dropClosure(st *sql.DropClosure) error {
	cl := x.cat.closure(st.Name)
	if cl == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("bangdb: no such closure: %s", st.Name)
	}
	y := x.closureCtx(nil)
	if cl.ChildIndex != "" && y.cat.index(cl.ChildIndex) != nil {
		if err := y.dropIndex(&sql.DropIndex{Name: cl.ChildIndex}); err != nil {
			return err
		}
	}
	if err := y.dropTable(&sql.DropTable{Name: cl.Name}); err != nil {
		return err
	}
	x.cat = y.cat
	x.mutableCatalog()
	delete(x.cat.closures, strings.ToLower(cl.Name))
	if x.tx != nil {
		delete(x.tx.closureDirty, strings.ToLower(cl.Name))
	}
	return deleteEntry(x.v, closureKey(cl.Name))
}

// closureGuard rejects schema changes that would break a closure.
func (x *execCtx) closureGuard(s sql.Stmt) error {
	if len(x.cat.closures) == 0 || x.closureWrite {
		return nil
	}
	owned := func(table string) error {
		if cl := x.cat.closure(table); cl != nil && x.cat.table(table) != nil {
			return fmt.Errorf("bangdb: %s is maintained by its closure; change %s instead (or DROP CLOSURE %s)", cl.Name, cl.Source, cl.Name)
		}
		return nil
	}
	used := func(table string) *Closure {
		t := x.cat.table(table)
		if t == nil {
			return nil
		}
		if cls := x.cat.closuresOf(t); len(cls) > 0 {
			return cls[0]
		}
		return nil
	}
	switch st := s.(type) {
	case *sql.Insert:
		return owned(st.Table)
	case *sql.Update:
		return owned(st.Table)
	case *sql.Delete:
		return owned(st.Table)
	case *sql.AlterTableAddColumn:
		return owned(st.Table)
	case *sql.AlterTableRename:
		return owned(st.Table)
	case *sql.AlterTableRenameColumn:
		return owned(st.Table)
	case *sql.AlterTableDropColumn:
		if err := owned(st.Table); err != nil {
			return err
		}
		if t := x.cat.table(st.Table); t != nil {
			for _, cl := range x.cat.closuresOf(t) {
				if strings.EqualFold(st.Column, cl.Child) || strings.EqualFold(st.Column, cl.Parent) {
					return fmt.Errorf("bangdb: column %s.%s is used by closure %s", t.Name, st.Column, cl.Name)
				}
			}
		}
	case *sql.DropTable:
		if err := owned(st.Name); err != nil {
			return err
		}
		if cl := used(st.Name); cl != nil {
			return fmt.Errorf("bangdb: table %s is the source of closure %s; DROP CLOSURE %s first", st.Name, cl.Name, cl.Name)
		}
	case *sql.DropIndex:
		for _, cl := range x.cat.closures {
			if strings.EqualFold(st.Name, cl.DesIndex) {
				return fmt.Errorf("bangdb: index %s belongs to closure %s", st.Name, cl.Name)
			}
		}
	}
	return nil
}

// closureRenamed follows ALTER TABLE renames of a source table or of its
// child or parent column.
func (x *execCtx) closureRenamed(s sql.Stmt) error {
	if len(x.cat.closures) == 0 {
		return nil
	}
	for key, cl := range x.cat.closures {
		n := *cl
		switch st := s.(type) {
		case *sql.AlterTableRename:
			if !strings.EqualFold(cl.Source, st.Table) {
				continue
			}
			n.Source = st.NewName
		case *sql.AlterTableRenameColumn:
			if !strings.EqualFold(cl.Source, st.Table) {
				continue
			}
			switch {
			case strings.EqualFold(cl.Child, st.Old):
				n.Child = st.New
			case strings.EqualFold(cl.Parent, st.Old):
				n.Parent = st.New
			default:
				continue
			}
		default:
			return nil
		}
		x.mutableCatalog()
		x.cat.closures[key] = &n
		if err := saveEntry(x.v, closureKey(n.Name), &n); err != nil {
			return err
		}
	}
	return nil
}

// ---- change tracking ----

// closureChanged records that row (of table t) was added or removed.
func (x *execCtx) closureChanged(t *Table, row []any) {
	if x.tx == nil || len(x.cat.closures) == 0 {
		return
	}
	for _, cl := range x.cat.closuresOf(t) {
		if c := t.ColIndex(cl.Child); c < len(row) && row[c] != nil {
			x.markClosure(cl, row[c])
		}
	}
}

// closureUpdated records a changed row if its child or parent changed.
func (x *execCtx) closureUpdated(t *Table, old, newRow []any) {
	if x.tx == nil || len(x.cat.closures) == 0 {
		return
	}
	old, newRow = padRow(t, old), padRow(t, newRow)
	for _, cl := range x.cat.closuresOf(t) {
		c, p := t.ColIndex(cl.Child), t.ColIndex(cl.Parent)
		if !keyChanged([]int{c, p}, old, newRow) {
			continue
		}
		for _, v := range []any{old[c], newRow[c]} {
			if v != nil {
				x.markClosure(cl, v)
			}
		}
	}
}

func (x *execCtx) markClosure(cl *Closure, node any) {
	if x.tx.closureDirty == nil {
		x.tx.closureDirty = map[string]map[string]any{}
	}
	name := strings.ToLower(cl.Name)
	m := x.tx.closureDirty[name]
	if m == nil {
		m = map[string]any{}
		x.tx.closureDirty[name] = m
	}
	m[nodeKey(node)] = node
}

func nodeKey(v any) string { return string(value.AppendKey(nil, v)) }

// flushClosures brings every closure whose source changed up to date. It
// runs when a statement (not a trigger body) finishes.
func (x *execCtx) flushClosures() error {
	if x.tx == nil || len(x.tx.closureDirty) == 0 || x.closureWrite {
		return nil
	}
	dirty := x.tx.closureDirty
	x.tx.closureDirty = nil
	names := make([]string, 0, len(dirty))
	for n := range dirty {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cl := x.cat.closure(n)
		if cl == nil {
			continue
		}
		if err := x.refreshClosure(cl, dirty[n]); err != nil {
			return err
		}
	}
	return nil
}

// ---- maintenance ----

// closureCtx returns a context for the internal statements of closure
// maintenance (allowed to write closure tables).
func (x *execCtx) closureCtx(args []any) *execCtx {
	y := *x
	y.args, y.outer, y.ctes, y.curSelect = args, nil, nil, nil
	y.closureWrite = true
	return &y
}

// lookup calls fn for the rows of t whose column col equals v, through the
// rowid or an index led by col when there is one.
func (x *execCtx) lookup(t *Table, col int, v any, fn func(rowid int64, row []any) error) error {
	if v == nil {
		return nil
	}
	if col == t.RowidCol {
		id, ok := v.(int64)
		if !ok {
			return nil
		}
		row, found, err := x.getRow(t, id)
		if err != nil || !found {
			return err
		}
		return fn(id, padRow(t, row))
	}
	for _, ix := range t.indexes {
		if len(ix.cols) == 0 || ix.cols[0] != col || ix.pred != nil || (len(ix.exprs) > 0 && ix.exprs[0] != nil) {
			continue
		}
		prefix := value.AppendKey(nil, foldValue(v, ix.colls[0]))
		if len(ix.cols) == 1 {
			prefix, _ = truncKey(prefix)
		} else {
			prefix, _ = groupKey(prefix)
		}
		var ids []int64
		c := x.index(ix).Seek(prefix)
		for ; c.Valid() && bytes.HasPrefix(c.Key(), prefix); c.Next() {
			if err := x.tick(); err != nil {
				return err
			}
			k := c.Key()
			ids = append(ids, value.DecodeRowID(k[len(k)-8:]))
		}
		if err := c.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			row, found, err := x.getRow(t, id)
			if err != nil {
				return err
			}
			row = padRow(t, row)
			if found && sameValues([]any{row[col]}, []any{v}) {
				if err := fn(id, row); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return x.scanAll(t, func(id int64, row []any) (bool, error) {
		row = padRow(t, row)
		if sameValues([]any{row[col]}, []any{v}) {
			return true, fn(id, row)
		}
		return true, nil
	})
}

// appendRow writes a row known to satisfy the table's constraints.
func (x *execCtx) appendRow(t *Table, row []any) error {
	if err := prepareRow(t, row); err != nil {
		return err
	}
	rowid, err := x.nextRowid(t)
	if err != nil {
		return err
	}
	x.tx.rowids[strings.ToLower(t.Name)] = rowid + 1
	return x.writeRow(t, rowid, row)
}

// ancestor is a proper ancestor (depth >= 1) of a node.
type ancestor struct {
	node  any
	depth int64
}

// refreshClosure recomputes the closure rows of the nodes in dirty and of
// everything below them; with dirty nil it builds the closure from scratch
// (its table is empty).
func (x *execCtx) refreshClosure(cl *Closure, dirty map[string]any) error {
	src, tab := x.cat.table(cl.Source), x.cat.table(cl.Name)
	if src == nil || tab == nil {
		return fmt.Errorf("bangdb: closure %s is broken: its tables are missing", cl.Name)
	}
	ci, pi := src.ColIndex(cl.Child), src.ColIndex(cl.Parent)
	const colAnc, colDes, colDepth = 0, 1, 2
	build := dirty == nil

	// The affected nodes: the changed children and everything below them
	// (as the closure, still of the previous state, knows it). Newly linked
	// nodes below are changed children themselves.
	var order []string
	affected := map[string]any{}
	add := func(v any) {
		k := nodeKey(v)
		if _, ok := affected[k]; !ok {
			affected[k] = v
			order = append(order, k)
		}
	}
	parents := map[string][]any{}
	present := map[string]bool{} // nodes that are the child of some row
	scanSource := func(keep func(k string) bool) error {
		return x.scanAll(src, func(_ int64, row []any) (bool, error) {
			if err := x.tick(); err != nil {
				return false, err
			}
			row = padRow(src, row)
			c := row[ci]
			if c == nil {
				return true, nil
			}
			k := nodeKey(c)
			if !keep(k) {
				return true, nil
			}
			if build {
				add(c)
			}
			present[k] = true
			if p := row[pi]; p != nil {
				parents[k] = append(parents[k], p)
			}
			return true, nil
		})
	}
	if build {
		if err := scanSource(func(string) bool { return true }); err != nil {
			return err
		}
	} else {
		keys := make([]string, 0, len(dirty))
		for k := range dirty {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(dirty[k])
			err := x.lookup(tab, colAnc, dirty[k], func(_ int64, row []any) error {
				add(row[colDes])
				return nil
			})
			if err != nil {
				return err
			}
		}
		if len(affected) > 2048 {
			// many nodes: one pass over the source beats a lookup per node
			if err := scanSource(func(k string) bool { _, ok := affected[k]; return ok }); err != nil {
				return err
			}
		} else {
			for _, k := range order {
				err := x.lookup(src, ci, affected[k], func(_ int64, row []any) error {
					present[k] = true
					if row[pi] != nil {
						parents[k] = append(parents[k], row[pi])
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
	}

	// Top-down over the affected nodes (Kahn): a node is computed once all of
	// its affected parents are.
	indeg := map[string]int{}
	kids := map[string][]string{}
	for _, k := range order {
		for _, p := range parents[k] {
			pk := nodeKey(p)
			if pk == k {
				return closureCycle(cl, affected[k])
			}
			if _, ok := affected[pk]; ok {
				indeg[k]++
				kids[pk] = append(kids[pk], k)
			}
		}
	}
	var queue []string
	for _, k := range order {
		if indeg[k] == 0 {
			queue = append(queue, k)
		}
	}
	computed := map[string][]ancestor{}
	outside := map[string][]ancestor{} // proper ancestors of unaffected parents
	ancestorsOf := func(pk string, p any) ([]ancestor, error) {
		if a, ok := computed[pk]; ok {
			return a, nil
		}
		if a, ok := outside[pk]; ok {
			return a, nil
		}
		var a []ancestor
		if !build {
			err := x.lookup(tab, colDes, p, func(_ int64, row []any) error {
				if d, _ := row[colDepth].(int64); d > 0 {
					a = append(a, ancestor{row[colAnc], d})
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		outside[pk] = a
		return a, nil
	}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if err := x.tick(); err != nil {
			return err
		}
		best := map[string]ancestor{}
		put := func(n any, d int64) {
			nk := nodeKey(n)
			if cur, ok := best[nk]; !ok || d < cur.depth {
				best[nk] = ancestor{n, d}
			}
		}
		for _, p := range parents[k] {
			put(p, 1)
			up, err := ancestorsOf(nodeKey(p), p)
			if err != nil {
				return err
			}
			for _, a := range up {
				put(a.node, a.depth+1)
			}
		}
		if _, self := best[k]; self {
			return closureCycle(cl, affected[k])
		}
		list := make([]ancestor, 0, len(best))
		for _, a := range best {
			list = append(list, a)
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].depth != list[j].depth {
				return list[i].depth < list[j].depth
			}
			return value.Compare(list[i].node, list[j].node) < 0
		})
		computed[k] = list
		for _, c := range kids[k] {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if len(computed) < len(order) {
		for _, k := range order {
			if _, ok := computed[k]; !ok {
				return closureCycle(cl, affected[k])
			}
		}
	}

	// Write: bring the rows of every affected node to the computed set,
	// touching only rows that differ (moving a subtree leaves the rows
	// between its own nodes alone).
	for _, k := range order {
		node := affected[k]
		want := make(map[string]int64, len(computed[k])+1)
		wantNode := make(map[string]any, len(computed[k])+1)
		if present[k] {
			want[k], wantNode[k] = 0, node
		}
		for _, a := range computed[k] {
			ak := nodeKey(a.node)
			want[ak], wantNode[ak] = a.depth, a.node
		}
		if !build {
			type old struct {
				id  int64
				row []any
			}
			var gone []old
			err := x.lookup(tab, colDes, node, func(id int64, row []any) error {
				ak := nodeKey(row[colAnc])
				if d, ok := want[ak]; ok && row[colDepth] == any(d) {
					delete(want, ak) // already right
					return nil
				}
				gone = append(gone, old{id, row})
				return nil
			})
			if err != nil {
				return err
			}
			for _, g := range gone {
				if err := x.deleteRow(tab, g.id, g.row); err != nil {
					return err
				}
			}
		}
		// insert in a stable order: self first, then by depth and value
		if d, ok := want[k]; ok {
			if err := x.appendRow(tab, []any{node, node, d}); err != nil {
				return err
			}
			delete(want, k)
		}
		for _, a := range computed[k] {
			ak := nodeKey(a.node)
			if d, ok := want[ak]; ok {
				if err := x.appendRow(tab, []any{wantNode[ak], node, d}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func closureCycle(cl *Closure, node any) error {
	return fmt.Errorf("bangdb: closure %s: the edges of %s.%s -> %s would make a cycle through %s",
		cl.Name, cl.Source, cl.Child, cl.Parent, sqlLiteral(node))
}

// checkClosure compares a closure table with the closure of its source,
// computed independently (a breadth-first search up from every node).
func (x *execCtx) checkClosure(cl *Closure, add func(string, ...any)) {
	src, tab := x.cat.table(cl.Source), x.cat.table(cl.Name)
	if src == nil || tab == nil {
		add("closure %s: its source %s or its table is missing", cl.Name, cl.Source)
		return
	}
	ci, pi := src.ColIndex(cl.Child), src.ColIndex(cl.Parent)
	if ci < 0 || pi < 0 {
		add("closure %s: column %s or %s is missing from %s", cl.Name, cl.Child, cl.Parent, cl.Source)
		return
	}
	parents := map[string][]string{}
	vals := map[string]any{}
	isChild := map[string]bool{}
	var nodes []string
	err := x.scanAll(src, func(_ int64, row []any) (bool, error) {
		row = padRow(src, row)
		if row[ci] == nil {
			return true, nil
		}
		k := nodeKey(row[ci])
		if !isChild[k] {
			isChild[k] = true
			nodes = append(nodes, k)
		}
		vals[k] = row[ci]
		if row[pi] != nil {
			pk := nodeKey(row[pi])
			vals[pk] = row[pi]
			parents[k] = append(parents[k], pk)
		}
		return true, nil
	})
	if err != nil {
		add("closure %s: reading %s: %v", cl.Name, cl.Source, err)
		return
	}
	want := map[string]int64{} // anc key + "\x00" + des key -> depth
	for _, n := range nodes {
		dist := map[string]int64{n: 0}
		queue := []string{n}
		for len(queue) > 0 && len(dist) <= len(vals) {
			cur := queue[0]
			queue = queue[1:]
			for _, p := range parents[cur] {
				if _, seen := dist[p]; !seen {
					dist[p] = dist[cur] + 1
					queue = append(queue, p)
				}
			}
		}
		for a, d := range dist {
			if a == n && d != 0 {
				add("closure %s: %s is its own ancestor (a cycle)", cl.Name, sqlLiteral(vals[n]))
			}
			want[a+"\x00"+n] = d
		}
	}
	bad := 0
	err = x.scanAll(tab, func(_ int64, row []any) (bool, error) {
		row = padRow(tab, row)
		key := nodeKey(row[0]) + "\x00" + nodeKey(row[1])
		d, ok := want[key]
		switch {
		case !ok:
			add("closure %s: row (%s, %s, %s) should not be there", cl.Name, sqlLiteral(row[0]), sqlLiteral(row[1]), sqlLiteral(row[2]))
			bad++
		case row[2] != any(d):
			add("closure %s: depth of (%s, %s) is %s, should be %d", cl.Name, sqlLiteral(row[0]), sqlLiteral(row[1]), sqlLiteral(row[2]), d)
			bad++
		}
		delete(want, key)
		return bad < 20, nil
	})
	if err != nil {
		add("closure %s: reading its table: %v", cl.Name, err)
		return
	}
	if bad < 20 && len(want) > 0 {
		add("closure %s: %d rows are missing", cl.Name, len(want))
	}
}
