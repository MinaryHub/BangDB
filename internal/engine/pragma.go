package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// metaUserVersion is the header meta slot of PRAGMA user_version.
const metaUserVersion = 3

// pragma runs PRAGMA statements, mostly as in SQLite:
//
//	user_version [= n]    schema version kept for the application (migrations)
//	table_list            tables and views with their storage kind
//	table_info(t)         columns of t
//	index_list(t)         indexes of t
//	index_info(i)         columns of index i
//	schema [(name)]       the CREATE statements of everything (or of one table
//	                      with its indexes and triggers, or one view)
//	integrity_check       "ok" or the problems found
//	foreign_keys [= on]   always on (1)
//	journal_mode [= wal]  always "wal"
//	page_count, page_size
func (x *execCtx) pragma(st *sql.Pragma) (Result, error) {
	one := func(name string, v any) (Result, error) {
		return Result{Columns: []string{name}, Rows: [][]any{{v}}}, nil
	}
	arg := func() (any, error) {
		if st.Value == nil {
			return nil, fmt.Errorf("bangdb: PRAGMA %s needs an argument", st.Name)
		}
		f, err := (&compiler{}).compile(st.Value)
		if err != nil {
			return nil, err
		}
		return f(&env{args: x.args})
	}
	setting := st.Value != nil && !st.Call
	switch st.Name {
	case "user_version":
		if !setting {
			return one("user_version", int64(int32(x.v.Meta(metaUserVersion))))
		}
		v, err := arg()
		if err != nil {
			return Result{}, err
		}
		n, ok := v.(int64)
		if !ok || n < -1<<31 || n >= 1<<31 {
			return Result{}, fmt.Errorf("bangdb: user_version must be a 32-bit integer")
		}
		x.v.SetMeta(metaUserVersion, uint32(int32(n)))
		return Result{}, nil
	case "foreign_keys":
		if setting {
			v, err := arg()
			if err != nil {
				return Result{}, err
			}
			if on, _ := pragmaBool(v); !on {
				return Result{}, fmt.Errorf("bangdb: foreign keys are always enforced and cannot be turned off")
			}
		}
		return one("foreign_keys", int64(1))
	case "journal_mode":
		return one("journal_mode", "wal") // the only mode; setting another is ignored, as SQLite reports
	case "page_size":
		return one("page_size", int64(pager.PageSize))
	case "page_count":
		return one("page_count", int64(x.v.PageCount()))
	case "integrity_check":
		problems := x.integrityCheck()
		res := Result{Columns: []string{"integrity_check"}}
		if len(problems) == 0 {
			res.Rows = [][]any{{"ok"}}
		}
		for _, p := range problems {
			res.Rows = append(res.Rows, []any{p})
		}
		return res, nil
	case "schema":
		want := ""
		if st.Value != nil {
			v, err := arg()
			if err != nil {
				return Result{}, err
			}
			want, _ = v.(string)
		}
		res := Result{Columns: []string{"type", "name", "tbl_name", "sql"}}
		add := func(typ, name, table, sql string) {
			if want == "" || strings.EqualFold(want, name) || strings.EqualFold(want, table) {
				res.Rows = append(res.Rows, []any{typ, name, table, sql})
			}
		}
		tables := x.cat.TableNames()
		sort.Strings(tables)
		for _, n := range tables {
			t := x.cat.table(n)
			cl := x.cat.closure(n)
			if cl != nil {
				add("closure", t.Name, cl.Source, x.cat.DescribeClosure(n))
			} else {
				add("table", t.Name, t.Name, t.createSQL())
			}
			ixs := append([]*Index(nil), t.indexes...)
			sort.Slice(ixs, func(i, j int) bool { return ixs[i].Name < ixs[j].Name })
			for _, ix := range ixs {
				if !ix.Auto && (cl == nil || !strings.EqualFold(ix.Name, cl.DesIndex)) {
					add("index", ix.Name, t.Name, ix.createSQL())
				}
			}
		}
		views := x.cat.ViewNames()
		sort.Strings(views)
		for _, n := range views {
			add("view", x.cat.view(n).Name, x.cat.view(n).Name, x.cat.DescribeView(n))
		}
		for _, n := range x.cat.TriggerNames() {
			add("trigger", n, x.cat.trigger(n).Table, x.cat.DescribeTrigger(n))
		}
		if want != "" && len(res.Rows) == 0 {
			return Result{}, fmt.Errorf("bangdb: no such table or view: %s", want)
		}
		return res, nil
	case "table_list":
		res := Result{Columns: []string{"name", "type", "kind", "ncol"}}
		for _, n := range x.cat.TableNames() {
			t := x.cat.table(n)
			res.Rows = append(res.Rows, []any{t.Name, "table", kindName(t.Store), int64(len(t.Columns))})
		}
		for _, n := range x.cat.ViewNames() {
			v := x.cat.view(n)
			res.Rows = append(res.Rows, []any{v.Name, "view", nil, nil})
		}
		sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i][0].(string) < res.Rows[j][0].(string) })
		return res, nil
	case "table_info", "index_list":
		v, err := arg()
		if err != nil {
			return Result{}, err
		}
		name, _ := v.(string)
		t := x.cat.table(name)
		if t == nil {
			if st.Name == "table_info" && x.cat.view(name) != nil {
				return x.viewInfo(name)
			}
			return Result{}, fmt.Errorf("bangdb: no such table: %v", v)
		}
		if st.Name == "table_info" {
			return tableInfo(t), nil
		}
		res := Result{Columns: []string{"seq", "name", "unique", "origin", "partial"}}
		ixs := append([]*Index(nil), t.indexes...)
		sort.Slice(ixs, func(i, j int) bool { return ixs[i].Name < ixs[j].Name })
		for i, ix := range ixs {
			origin := "c"
			if ix.Auto {
				origin = "u"
				if strings.HasSuffix(ix.Name, "_pk") {
					origin = "pk"
				}
			}
			res.Rows = append(res.Rows, []any{int64(i), ix.Name, boolInt(ix.Unique), origin, int64(0)})
		}
		return res, nil
	case "index_info":
		v, err := arg()
		if err != nil {
			return Result{}, err
		}
		name, _ := v.(string)
		ix := x.cat.index(name)
		if ix == nil {
			return Result{}, fmt.Errorf("bangdb: no such index: %v", v)
		}
		t := x.cat.table(ix.Table)
		res := Result{Columns: []string{"seqno", "cid", "name"}}
		for i, c := range ix.cols {
			if c < 0 { // an expression, as SQLite reports it
				res.Rows = append(res.Rows, []any{int64(i), int64(-2), nil})
				continue
			}
			res.Rows = append(res.Rows, []any{int64(i), int64(c), t.Columns[c].Name})
		}
		return res, nil
	}
	return Result{}, fmt.Errorf("bangdb: unknown pragma %s", st.Name)
}

func tableInfo(t *Table) Result {
	res := Result{Columns: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}}
	pk := t.primaryKey()
	for i, c := range t.Columns {
		pos := int64(0)
		for k, name := range pk {
			if strings.EqualFold(name, c.Name) {
				pos = int64(k + 1)
			}
		}
		typ := c.DeclType
		if typ == "" && c.Type != value.Any {
			typ = string(c.Type)
		}
		var dflt any
		if c.DefaultSQL != "" {
			dflt = c.DefaultSQL
		}
		res.Rows = append(res.Rows, []any{int64(i), c.Name, typ, boolInt(c.NotNull), dflt, pos})
	}
	return res
}

func (x *execCtx) viewInfo(name string) (Result, error) {
	t, _, _, err := x.viewTable(x.cat.view(name), name)
	if err != nil {
		return Result{}, err
	}
	return tableInfo(t), nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func pragmaBool(v any) (bool, bool) {
	switch x := v.(type) {
	case int64:
		return x != 0, true
	case string:
		switch strings.ToLower(x) {
		case "on", "true", "yes", "1":
			return true, true
		case "off", "false", "no", "0":
			return false, true
		}
	}
	return false, false
}
