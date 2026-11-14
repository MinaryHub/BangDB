package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

// IntegrityCheck verifies the whole database on a consistent snapshot and
// returns the problems found (an empty list means the database is sound):
//
//   - every page's checksum, every B+tree's structure (see btree.Check);
//   - every page is used exactly once: by the header, a tree, an overflow
//     chain or the free list (no lost or doubly used pages);
//   - every row decodes and matches its table's columns;
//   - every index has exactly one entry per row with the row's value, and
//     UNIQUE indexes have no duplicates.
//
// It does not block readers or writers.
func (db *DB) IntegrityCheck(ctx context.Context) ([]string, error) {
	var problems []string
	_, err := db.read(ctx, func(x *execCtx) (Result, error) {
		problems = x.integrityCheck()
		return Result{}, x.ctx.Err()
	}, nil)
	return problems, err
}

func (x *execCtx) integrityCheck() []string {
	var problems []string
	add := func(format string, args ...any) {
		if len(problems) < 500 {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	n := x.v.PageCount()
	uses := make([]uint8, n)
	owner := make([]string, n)
	markFor := func(what string) func(uint32) {
		return func(pg uint32) {
			if pg >= n {
				add("%s references page %d beyond the end of the database (%d pages)", what, pg, n)
				return
			}
			if uses[pg] < 255 {
				uses[pg]++
			}
			if uses[pg] == 2 {
				add("page %d is used by both %s and %s", pg, owner[pg], what)
			}
			owner[pg] = what
		}
	}
	markFor("the header")(0)
	checkTree := func(what string, root uint32) btree.CheckResult {
		r := btree.Check(x.v, root, markFor(what))
		for _, p := range r.Problems {
			add("%s: %s", what, p)
		}
		return r
	}
	if root := x.v.Meta(metaCatalogRoot); root != 0 {
		checkTree("catalog", root)
	}
	if root := x.v.Meta(metaSequenceRoot); root != 0 {
		checkTree("autoincrement sequences", root)
	}

	names := x.cat.TableNames()
	sort.Strings(names)
	for _, name := range names {
		if x.ctx != nil && x.ctx.Err() != nil {
			return problems
		}
		t := x.cat.table(name)
		// A volatile kind's data is not in pages: only its root page
		// (the store's identity) is accounted for.
		structure := func(what string, root uint32) bool {
			if t.kind.Volatile() {
				markFor(what)(root)
				return true
			}
			return len(checkTree(what, root).Problems) == 0
		}
		if !structure("table "+t.Name, t.Root) {
			continue // row and index checks would only repeat the damage
		}
		rows := x.checkRows(t, add)
		for _, ix := range t.indexes {
			if structure("index "+ix.Name, ix.Root) {
				x.checkIndex(t, ix, rows, add)
			}
		}
	}

	for _, name := range x.cat.ClosureNames() {
		x.checkClosure(x.cat.closure(name), add)
	}

	// Free list.
	freeMark := markFor("the free list")
	pg, hops := x.v.FreeHead(), uint32(0)
	for pg != 0 {
		if hops++; hops > n {
			add("the free list has a cycle")
			break
		}
		if pg >= n {
			add("the free list references page %d beyond the end", pg)
			break
		}
		freeMark(pg)
		b, err := x.v.Read(pg)
		if err != nil {
			add("free page %d: %v", pg, err)
			break
		}
		pg = binary.BigEndian.Uint32(b[0:4])
	}
	lost := 0
	for i, u := range uses {
		if u == 0 {
			if lost < 20 {
				add("page %d is not used by anything (lost)", i)
			}
			lost++
		}
	}
	if lost > 20 {
		add("... %d lost pages in total", lost)
	}
	return problems
}

// checkRows decodes every row of t and returns the row count. Rows of the
// default kind are read from the B+tree directly, so that one undecodable
// row does not hide the others; other kinds are read through their store.
func (x *execCtx) checkRows(t *Table, add func(string, ...any)) int {
	count := 0
	check := func(rowid int64, row []any) {
		if len(row) > len(t.Columns) {
			add("table %s row %d has %d values for %d columns", t.Name, rowid, len(row), len(t.Columns))
			return
		}
		if t.RowidCol >= 0 && t.RowidCol < len(row) && row[t.RowidCol] != rowid {
			add("table %s row %d: primary key column holds %v", t.Name, rowid, row[t.RowidCol])
		}
		if len(t.ForeignKeys) > 0 {
			if err := x.checkParents(t, padRow(t, row), nil); err != nil {
				add("table %s row %d: %v", t.Name, rowid, err)
			}
		}
	}
	if t.Store != "" {
		err := x.rows(t).Scan(store.MinRowid, store.MaxRowid, false, func(rowid int64, row []any) (bool, error) {
			count++
			check(rowid, row)
			return true, nil
		})
		if err != nil {
			add("table %s: %v", t.Name, err)
		}
		return count
	}
	c := btree.Open(x.v, t.Root).Seek(nil)
	for ; c.Valid(); c.Next() {
		count++
		rowid := value.DecodeRowID(c.Key())
		data, err := c.Value()
		if err != nil {
			add("table %s row %d: %v", t.Name, rowid, err)
			continue
		}
		row, err := value.DecodeRow(data)
		if err != nil {
			add("table %s row %d: %v", t.Name, rowid, err)
			continue
		}
		check(rowid, row)
	}
	if err := c.Err(); err != nil {
		add("table %s: %v", t.Name, err)
	}
	return count
}

// checkIndex verifies that ix has exactly one correct entry per row.
func (x *execCtx) checkIndex(t *Table, ix *Index, rows int, add func(string, ...any)) {
	entries := 0
	var prevVal []byte
	c := x.index(ix).Seek(nil)
	for ; c.Valid(); c.Next() {
		entries++
		key := c.Key()
		if len(key) < 9 {
			add("index %s: malformed key", ix.Name)
			continue
		}
		valPart, rowid := key[:len(key)-8], value.DecodeRowID(key[len(key)-8:])
		row, ok, err := x.getRow(t, rowid)
		if err != nil || !ok {
			add("index %s points to missing row %d of %s", ix.Name, rowid, t.Name)
			continue
		}
		if ok, err := ix.covers(row); err != nil || !ok {
			add("index %s has an entry for row %d of %s, which its WHERE excludes", ix.Name, rowid, t.Name)
		}
		if want := ix.key(row); !bytes.Equal(want, valPart) {
			add("index %s entry for row %d does not match the row's %s", ix.Name, rowid, ix.columnList())
		}
		if isTruncated(valPart) {
			valPart = ix.fullKey(row) // compare the values themselves
		}
		if ix.Unique && prevVal != nil && bytes.Equal(prevVal, valPart) && !ix.hasNull(row) {
			var vals []string
			for i := range ix.cols {
				vals = append(vals, value.Format(ix.value(i, row)))
			}
			add("unique index %s has duplicate value %s", ix.Name, strings.Join(vals, ", "))
		}
		prevVal = append(prevVal[:0], valPart...)
	}
	if err := c.Err(); err != nil {
		add("index %s: %v", ix.Name, err)
	}
	if ix.pred != nil { // a partial index has entries for the covered rows only
		rows = 0
		err := x.scanAll(t, func(_ int64, row []any) (bool, error) {
			if ok, _ := ix.covers(row); ok {
				rows++
			}
			return true, nil
		})
		if err != nil {
			add("table %s: %v", t.Name, err)
			return
		}
	}
	if entries != rows {
		add("index %s has %d entries but table %s has %d rows", ix.Name, entries, t.Name, rows)
	}
}
