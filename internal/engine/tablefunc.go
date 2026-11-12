package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/knoxxr/bangdb/internal/value"
)

// Table-valued functions: FROM name(args). Their rows are computed from the
// arguments, which may use the columns of the tables to their left, so
//
//	SELECT u.id, j.value FROM users u, json_each(u.tags) j
//
// yields one row per element of each user's tags.
//
//	json_each(json [, path])   the members of an array or object (one level)
//	json_tree(json [, path])   the element and everything below it
//	generate_series(start, stop [, step])   the integers from start to stop
//
// json_each and json_tree have the columns of SQLite: key, value, type,
// atom, id, parent, fullkey and path.

type tableFunc struct {
	name             string
	cols             []string
	minArgs, maxArgs int
	// rows passes the function's rows for args to emit, which returns false
	// to stop.
	rows func(args []any, emit func(row []any) (bool, error)) error
}

var jsonEachCols = []string{"key", "value", "type", "atom", "id", "parent", "fullkey", "path"}

var tableFuncs = map[string]*tableFunc{
	"json_each": {name: "json_each", cols: jsonEachCols, minArgs: 1, maxArgs: 2,
		rows: func(args []any, emit func([]any) (bool, error)) error { return jsonRows(args, false, emit) }},
	"json_tree": {name: "json_tree", cols: jsonEachCols, minArgs: 1, maxArgs: 2,
		rows: func(args []any, emit func([]any) (bool, error)) error { return jsonRows(args, true, emit) }},
	"generate_series": {name: "generate_series", cols: []string{"value"}, minArgs: 2, maxArgs: 3, rows: seriesRows},
}

func lookupTableFunc(name string, nargs int) (*tableFunc, error) {
	fn := tableFuncs[strings.ToLower(name)]
	if fn == nil {
		return nil, fmt.Errorf("bangdb: no such table-valued function: %s", name)
	}
	if nargs < fn.minArgs || nargs > fn.maxArgs {
		return nil, fmt.Errorf("bangdb: wrong number of arguments to %s()", fn.name)
	}
	return fn, nil
}

// table describes the function's rows as a table called name.
func (fn *tableFunc) table(name string) *Table {
	t := &Table{Name: name, RowidCol: -1}
	for _, c := range fn.cols {
		t.Columns = append(t.Columns, Column{Name: c, Type: value.Any})
	}
	return t
}

// jsonRows implements json_each (tree = false) and json_tree.
func jsonRows(args []any, tree bool, emit func([]any) (bool, error)) error {
	doc, ok, err := jsonDoc(args[0])
	if !ok {
		return err // NULL: no rows
	}
	// Walk to the element named by the path, keeping its place in the document.
	full, parentPath := "$", "$"
	var key any
	if len(args) > 1 {
		if args[1] == nil {
			return nil
		}
		steps, err := parsePath(args[1])
		if err != nil {
			return err
		}
		for _, st := range steps {
			parentPath = full
			switch x := doc.(type) {
			case *jsonObj:
				i := -1
				if st.isKey {
					i = x.index(st.key)
				}
				if i < 0 {
					return nil
				}
				key, doc = st.key, x.vals[i]
				full += jsonKeyPath(st.key)
			case []any:
				i := st.arrayIndex(len(x))
				if st.isKey || i < 0 || i >= int64(len(x)) {
					return nil
				}
				key, doc = i, x[i]
				full += "[" + strconv.FormatInt(i, 10) + "]"
			default:
				return nil
			}
		}
	}
	nextID := int64(0)
	stopped := false
	// visit emits the row of v and, for json_tree, of everything in it.
	var visit func(key any, v any, parent any, fullkey, path string, descend bool) error
	visit = func(key any, v any, parent any, fullkey, path string, descend bool) error {
		id := nextID
		nextID++
		val := fromJSON(v)
		var atom any
		switch v.(type) {
		case []any, *jsonObj:
		default:
			atom = val
		}
		cont, err := emit([]any{key, val, jsonTypeName(v), atom, id, parent, fullkey, path})
		if err != nil || !cont {
			stopped = true
			return err
		}
		if !descend {
			return nil
		}
		return children(v, fullkey, func(k any, c any, ck string) error {
			if stopped {
				return nil
			}
			return visit(k, c, id, ck, fullkey, true)
		})
	}
	if tree {
		return visit(key, doc, nil, full, parentPath, true)
	}
	switch doc.(type) {
	case []any, *jsonObj:
		nextID = 1
		return children(doc, full, func(k any, c any, ck string) error {
			if stopped {
				return nil
			}
			return visit(k, c, nil, ck, full, false)
		})
	}
	return visit(key, doc, nil, full, parentPath, false)
}

// children calls fn for each member of a JSON array or object with its key
// and its path, given the container's path.
func children(v any, path string, fn func(key any, child any, childPath string) error) error {
	switch x := v.(type) {
	case []any:
		for i, c := range x {
			if err := fn(int64(i), c, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case *jsonObj:
		for i, k := range x.keys {
			if err := fn(k, x.vals[i], path+jsonKeyPath(k)); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonKeyPath is the path step of an object key: .key, or ."key" when the
// key is not a plain word.
func jsonKeyPath(k string) string {
	plain := k != ""
	for i, r := range k {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			plain = false
			break
		}
	}
	if plain {
		return "." + k
	}
	return `."` + k + `"`
}

// seriesRows implements generate_series(start, stop [, step]).
func seriesRows(args []any, emit func([]any) (bool, error)) error {
	nums := []int64{0, 0, 1}
	for i, a := range args {
		if a == nil {
			return nil
		}
		n, ok := keyValue(a, value.Integer)
		if !ok {
			return fmt.Errorf("bangdb: generate_series() takes integers, not %s", value.Format(a))
		}
		nums[i] = n.(int64)
	}
	start, stop, step := nums[0], nums[1], nums[2]
	if step == 0 {
		step = 1
	}
	for v := start; step > 0 && v <= stop || step < 0 && v >= stop; v += step {
		if cont, err := emit([]any{v}); err != nil || !cont {
			return err
		}
		if step > 0 && v > math.MaxInt64-step || step < 0 && v < math.MinInt64-step {
			break // the next value does not fit
		}
	}
	return nil
}
