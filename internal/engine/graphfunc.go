package engine

import (
	"fmt"
	"strings"

	"github.com/knoxxr/bangdb/internal/value"
)

// Graph traversal table-valued functions over an edge table (or view):
//
//	graph_walk(edges, from_col, to_col, start [, max_depth [, direction]])
//	    one row (node, depth, parent) per node reachable from start, with
//	    its shortest distance and its predecessor on one shortest path
//	    (start itself: depth 0, parent NULL), in breadth-first order
//	graph_path(edges, from_col, to_col, start, goal [, max_depth [, direction]])
//	    the nodes (step, node) of one shortest path from start to goal,
//	    no rows if goal is not reachable
//
// edges names a table or a view, from_col and to_col two of its columns;
// an edge leads from the from_col value of a row to its to_col value.
// direction is 'out' (follow edges, the default), 'in' (follow them
// backwards) or 'both'; max_depth NULL or absent means no limit. Every node
// is visited once, so cycles are fine. Edges with a condition (one
// predicate of a triple table) are a view:
//
//	CREATE VIEW part_of AS SELECT s, o FROM triple WHERE p = 7;
//	SELECT node, depth FROM graph_walk('part_of', 's', 'o', 1234, 5);
//
// A table is read through its rowid or an index led by the column that is
// looked up (from_col for 'out', to_col for 'in'); a view, or a table
// without such an index, is read once per statement into memory.

var graphWalkCols = []string{"node", "depth", "parent"}
var graphPathCols = []string{"step", "node"}

func init() {
	tableFuncs["graph_walk"] = &tableFunc{name: "graph_walk", cols: graphWalkCols, minArgs: 4, maxArgs: 6, xrows: graphWalkRows}
	tableFuncs["graph_path"] = &tableFunc{name: "graph_path", cols: graphPathCols, minArgs: 5, maxArgs: 7, xrows: graphPathRows}
}

// graphEdges is an edge set being traversed.
type graphEdges struct {
	t        *Table
	from, to int
	// in memory (a view, or a table without a usable index for some
	// direction): neighbours by node key
	out, in map[string][]any
}

// graphSource resolves the edge set named by the first three arguments.
func (x *execCtx) graphSource(fn string, args []any) (*graphEdges, error) {
	name, ok1 := args[0].(string)
	fromCol, ok2 := args[1].(string)
	toCol, ok3 := args[2].(string)
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("bangdb: %s(): the table and column names must be text", fn)
	}
	key := strings.ToLower(name + "\x00" + fromCol + "\x00" + toCol)
	if g := x.graphs[key]; g != nil {
		return g, nil
	}
	var g *graphEdges
	if t := x.cat.table(name); t != nil {
		from, to := t.ColIndex(fromCol), t.ColIndex(toCol)
		if from < 0 || to < 0 {
			return nil, fmt.Errorf("bangdb: %s(): no such column: %s.%s", fn, t.Name, map[bool]string{true: fromCol, false: toCol}[from < 0])
		}
		g = &graphEdges{t: t, from: from, to: to}
	} else if v := x.cat.view(name); v != nil {
		vt, rows, _, err := x.viewTable(v, v.Name)
		if err != nil {
			return nil, err
		}
		from, to := vt.ColIndex(fromCol), vt.ColIndex(toCol)
		if from < 0 || to < 0 {
			return nil, fmt.Errorf("bangdb: %s(): no such column: %s.%s", fn, v.Name, map[bool]string{true: fromCol, false: toCol}[from < 0])
		}
		g = &graphEdges{from: from, to: to}
		g.load(rows)
	} else {
		return nil, fmt.Errorf("bangdb: %s(): no such table or view: %s", fn, name)
	}
	if x.graphs == nil {
		x.graphs = map[string]*graphEdges{}
	}
	x.graphs[key] = g
	return g, nil
}

// load fills the in-memory adjacency from edge rows.
func (g *graphEdges) load(rows [][]any) {
	g.out, g.in = map[string][]any{}, map[string][]any{}
	for _, r := range rows {
		a, b := r[g.from], r[g.to]
		if a == nil || b == nil {
			continue
		}
		g.out[nodeKey(a)] = append(g.out[nodeKey(a)], b)
		g.in[nodeKey(b)] = append(g.in[nodeKey(b)], a)
	}
}

// indexed reports whether the table can be searched by column c.
func indexed(t *Table, c int) bool {
	if c == t.RowidCol {
		return true
	}
	for _, ix := range t.indexes {
		if len(ix.cols) > 0 && ix.cols[0] == c && ix.pred == nil && (len(ix.exprs) == 0 || ix.exprs[0] == nil) {
			return true
		}
	}
	return false
}

// neighbours returns the nodes one edge away from node.
func (x *execCtx) neighbours(g *graphEdges, node any, dir string) ([]any, error) {
	var out []any
	step := func(by, other int, mem func() map[string][]any) error {
		if g.t == nil || !indexed(g.t, by) {
			if g.out == nil { // read the table once
				var rows [][]any
				err := x.scanAll(g.t, func(_ int64, row []any) (bool, error) {
					if err := x.tick(); err != nil {
						return false, err
					}
					rows = append(rows, padRow(g.t, row))
					return true, nil
				})
				if err != nil {
					return err
				}
				g.load(rows)
			}
			out = append(out, mem()[nodeKey(node)]...)
			return nil
		}
		return x.lookup(g.t, by, node, func(_ int64, row []any) error {
			if v := row[other]; v != nil {
				out = append(out, v)
			}
			return nil
		})
	}
	if dir == "out" || dir == "both" {
		if err := step(g.from, g.to, func() map[string][]any { return g.out }); err != nil {
			return nil, err
		}
	}
	if dir == "in" || dir == "both" {
		if err := step(g.to, g.from, func() map[string][]any { return g.in }); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// walkOptions reads [max_depth [, direction]] starting at args[i].
func walkOptions(fn string, args []any, i int) (int64, string, error) {
	maxDepth, dir := int64(-1), "out"
	if len(args) > i && args[i] != nil {
		n, ok := keyValue(args[i], value.Integer)
		if !ok || n.(int64) < 0 {
			return 0, "", fmt.Errorf("bangdb: %s(): max_depth must be a non-negative integer", fn)
		}
		maxDepth = n.(int64)
	}
	if len(args) > i+1 && args[i+1] != nil {
		s, _ := args[i+1].(string)
		dir = strings.ToLower(s)
		if dir != "out" && dir != "in" && dir != "both" {
			return 0, "", fmt.Errorf("bangdb: %s(): direction must be 'out', 'in' or 'both'", fn)
		}
	}
	return maxDepth, dir, nil
}

// bfs visits the nodes reachable from start breadth first; visit gets each
// node with its depth and predecessor and returns false to stop.
func (x *execCtx) bfs(g *graphEdges, start any, maxDepth int64, dir string, visit func(node any, depth int64, parent any) (bool, error)) error {
	type item struct {
		node  any
		depth int64
	}
	seen := map[string]bool{nodeKey(start): true}
	if cont, err := visit(start, 0, nil); err != nil || !cont {
		return err
	}
	queue := []item{{start, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if maxDepth >= 0 && cur.depth >= maxDepth {
			continue
		}
		next, err := x.neighbours(g, cur.node, dir)
		if err != nil {
			return err
		}
		for _, n := range next {
			if err := x.tick(); err != nil {
				return err
			}
			k := nodeKey(n)
			if seen[k] {
				continue
			}
			seen[k] = true
			if cont, err := visit(n, cur.depth+1, cur.node); err != nil || !cont {
				return err
			}
			queue = append(queue, item{n, cur.depth + 1})
		}
	}
	return nil
}

func graphWalkRows(x *execCtx, args []any, emit func([]any) (bool, error)) error {
	g, err := x.graphSource("graph_walk", args)
	if err != nil {
		return err
	}
	maxDepth, dir, err := walkOptions("graph_walk", args, 4)
	if err != nil || args[3] == nil {
		return err
	}
	return x.bfs(g, args[3], maxDepth, dir, func(node any, depth int64, parent any) (bool, error) {
		return emit([]any{node, depth, parent})
	})
}

func graphPathRows(x *execCtx, args []any, emit func([]any) (bool, error)) error {
	g, err := x.graphSource("graph_path", args)
	if err != nil {
		return err
	}
	maxDepth, dir, err := walkOptions("graph_path", args, 5)
	if err != nil || args[3] == nil || args[4] == nil {
		return err
	}
	goal := nodeKey(args[4])
	prev := map[string]any{} // node key -> predecessor
	vals := map[string]any{}
	found := false
	err = x.bfs(g, args[3], maxDepth, dir, func(node any, _ int64, parent any) (bool, error) {
		k := nodeKey(node)
		vals[k] = node
		if parent != nil {
			prev[k] = parent
		}
		if k == goal {
			found = true
			return false, nil
		}
		return true, nil
	})
	if err != nil || !found {
		return err
	}
	var path []any
	for k := goal; ; {
		path = append(path, vals[k])
		p, ok := prev[k]
		if !ok {
			break
		}
		k = nodeKey(p)
	}
	for i := len(path) - 1; i >= 0; i-- {
		if cont, err := emit([]any{int64(len(path) - 1 - i), path[i]}); err != nil || !cont {
			return err
		}
	}
	return nil
}
