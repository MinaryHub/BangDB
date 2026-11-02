package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// Triggers: CREATE TRIGGER name {BEFORE|AFTER} {INSERT|UPDATE [OF cols]|
// DELETE} ON table [FOR EACH ROW] [WHEN cond] BEGIN stmt; ... END.
//
// They run for every row an INSERT, UPDATE or DELETE statement (including
// UPSERT's update) changes, inside the statement: a failing trigger fails
// the statement. NEW.col and OLD.col name the row after and before the
// change (NEW for INSERT and UPDATE, OLD for UPDATE and DELETE).
// RAISE(ABORT|FAIL|ROLLBACK, 'message') fails the statement;
// RAISE(IGNORE) in a BEFORE trigger skips the row. Rows changed by foreign
// key actions (CASCADE, SET NULL) do not fire triggers.

// Trigger is a catalog entry.
type Trigger struct {
	Name  string `json:"name"`
	Table string `json:"table"`
	SQL   string `json:"sql"` // the CREATE TRIGGER statement

	def *sql.CreateTrigger
}

func triggerKey(name string) []byte { return []byte("g\x00" + strings.ToLower(name)) }

func (tr *Trigger) prepare() error {
	stmts, n, err := sql.Parse(tr.SQL)
	if err != nil {
		return fmt.Errorf("bangdb: trigger %s: %w", tr.Name, err)
	}
	if n > 0 {
		return errors.New("bangdb: parameters are not allowed in triggers")
	}
	def, ok := stmts[0].(*sql.CreateTrigger)
	if !ok || len(stmts) != 1 {
		return fmt.Errorf("bangdb: trigger %s: not a CREATE TRIGGER statement", tr.Name)
	}
	tr.def = def
	return nil
}

func (c *Catalog) trigger(name string) *Trigger { return c.triggers[strings.ToLower(name)] }

// TriggerNames returns the names of all triggers.
func (c *Catalog) TriggerNames() []string {
	var out []string
	for _, tr := range c.triggers {
		out = append(out, tr.Name)
	}
	sort.Strings(out)
	return out
}

// DescribeTrigger returns the CREATE TRIGGER statement of a trigger.
func (c *Catalog) DescribeTrigger(name string) string {
	if tr := c.trigger(name); tr != nil {
		return tr.SQL + ";"
	}
	return ""
}

// tableTriggers returns t's triggers for one timing and event, by name.
func (c *Catalog) tableTriggers(t *Table, timing, event string) []*Trigger {
	var out []*Trigger
	for _, tr := range c.triggers {
		if strings.EqualFold(tr.Table, t.Name) && tr.def.Timing == timing && tr.def.Event == event {
			out = append(out, tr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (x *execCtx) createTrigger(st *sql.CreateTrigger) error {
	if x.cat.trigger(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("bangdb: trigger %s already exists", st.Name)
	}
	t := x.cat.table(st.Table)
	if t == nil {
		if x.cat.view(st.Table) != nil {
			return fmt.Errorf("bangdb: cannot create a trigger on view %s (INSTEAD OF triggers are not supported)", st.Table)
		}
		return fmt.Errorf("bangdb: no such table: %s", st.Table)
	}
	for _, c := range st.Columns {
		if t.ColIndex(c) < 0 {
			return fmt.Errorf("bangdb: no such column: %s.%s", t.Name, c)
		}
	}
	tr := &Trigger{Name: st.Name, Table: t.Name, SQL: st.SQL}
	if err := tr.prepare(); err != nil {
		return err
	}
	x.mutableCatalog()
	x.cat.triggers[strings.ToLower(tr.Name)] = tr
	return saveEntry(x.v, triggerKey(tr.Name), tr)
}

func (x *execCtx) dropTrigger(st *sql.DropTrigger) error {
	tr := x.cat.trigger(st.Name)
	if tr == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("bangdb: no such trigger: %s", st.Name)
	}
	x.mutableCatalog()
	delete(x.cat.triggers, strings.ToLower(tr.Name))
	return deleteEntry(x.v, triggerKey(tr.Name))
}

// dropTableTriggers removes the triggers of a dropped table.
func (x *execCtx) dropTableTriggers(t *Table) error {
	for key, tr := range x.cat.triggers {
		if strings.EqualFold(tr.Table, t.Name) {
			delete(x.cat.triggers, key)
			if err := deleteEntry(x.v, triggerKey(tr.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// errIgnoreRow is RAISE(IGNORE): skip the current row.
var errIgnoreRow = errors.New("bangdb: RAISE(IGNORE) outside a trigger")

// raiseError is RAISE(ABORT|FAIL|ROLLBACK, message).
type raiseError struct{ msg string }

func (e *raiseError) Error() string { return "bangdb: " + e.msg }

const maxTriggerDepth = 32

// fireTriggers runs t's triggers for an event on one row. old and new are
// the row before and after the change (nil when there is none); set lists
// the columns an UPDATE assigns (for UPDATE OF). It returns errIgnoreRow
// when a BEFORE trigger raised IGNORE.
func (x *execCtx) fireTriggers(t *Table, timing, event string, oldID int64, old []any, newID int64, newRow []any, set []int) error {
	if len(x.cat.triggers) == 0 {
		return nil
	}
	triggers := x.cat.tableTriggers(t, timing, event)
	if len(triggers) == 0 {
		return nil
	}
	if x.triggerDepth >= maxTriggerDepth {
		return errors.New("bangdb: too many levels of trigger recursion")
	}
	// NEW and OLD are an enclosing "row" of the trigger's statements.
	n := len(t.Columns) + 1
	sc := &scope{cols: append(tableScope(t, "old", 0, true), tableScope(t, "new", 1, true)...)}
	cur := &env{row: make([]any, 2*n), args: x.args}
	if old != nil {
		copy(cur.row, padRow(t, old))
		cur.row[n-1] = oldID
	}
	if newRow != nil {
		copy(cur.row[n:], padRow(t, newRow))
		cur.row[2*n-1] = newID
	}
	outer := &outerCtx{sc: sc, cur: cur}
	for _, tr := range triggers {
		def := tr.def
		if len(def.Columns) > 0 && set != nil {
			hit := false
			for _, name := range def.Columns {
				for _, c := range set {
					hit = hit || strings.EqualFold(t.Columns[c].Name, name)
				}
			}
			if !hit {
				continue
			}
		}
		y := *x // the trigger's statements run in this statement's transaction
		y.outer, y.triggerDepth, y.args = outer, x.triggerDepth+1, nil
		y.lastTable, y.plans = nil, nil
		if def.When != nil {
			c := &compiler{x: &y, sc: &scope{outer: outer}}
			f, err := c.compile(def.When)
			if err != nil {
				return fmt.Errorf("bangdb: trigger %s: %w", tr.Name, err)
			}
			v, err := f(&env{})
			if err != nil {
				return triggerErr(tr, err)
			}
			if ok, _ := value.Truthy(v); !ok {
				continue
			}
		}
		for _, s := range def.Body {
			if _, err := y.exec(s); err != nil {
				return triggerErr(tr, err)
			}
		}
	}
	return nil
}

// afterTriggers fires AFTER triggers; RAISE(IGNORE) there ends the
// trigger without effect on the row, which is already written.
func (x *execCtx) afterTriggers(t *Table, event string, oldID int64, old []any, newID int64, newRow []any, set []int) error {
	err := x.fireTriggers(t, "AFTER", event, oldID, old, newID, newRow, set)
	if errors.Is(err, errIgnoreRow) {
		return nil
	}
	return err
}

func (x *execCtx) hasTriggers(t *Table) bool {
	for _, tr := range x.cat.triggers {
		if strings.EqualFold(tr.Table, t.Name) {
			return true
		}
	}
	return false
}

func triggerErr(tr *Trigger, err error) error {
	var re *raiseError
	if errors.Is(err, errIgnoreRow) || errors.As(err, &re) {
		return err
	}
	return fmt.Errorf("bangdb: trigger %s: %w", tr.Name, err)
}

// compileRaise compiles RAISE(IGNORE) and RAISE(ABORT|FAIL|ROLLBACK, msg).
func (c *compiler) compileRaise(f *sql.Func) (evalFn, error) {
	kind := ""
	if len(f.Args) > 0 {
		if r, ok := f.Args[0].(*sql.ColumnRef); ok && r.Table == "" {
			kind = strings.ToUpper(r.Name)
		}
	}
	switch kind {
	case "IGNORE":
		if len(f.Args) != 1 {
			return nil, errors.New("bangdb: RAISE(IGNORE) takes no message")
		}
		return func(*env) (any, error) { return nil, errIgnoreRow }, nil
	case "ABORT", "FAIL", "ROLLBACK":
		if len(f.Args) != 2 {
			return nil, fmt.Errorf("bangdb: RAISE(%s, message) needs a message", kind)
		}
		msg, err := c.compile(f.Args[1])
		if err != nil {
			return nil, err
		}
		return func(e *env) (any, error) {
			m, err := msg(e)
			if err != nil {
				return nil, err
			}
			return nil, &raiseError{value.Format(m)}
		}, nil
	}
	return nil, errors.New("bangdb: RAISE needs IGNORE, ABORT, FAIL or ROLLBACK")
}
