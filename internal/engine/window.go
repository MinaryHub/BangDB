package engine

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// Window functions: f(...) OVER (PARTITION BY ... ORDER BY ... frame).
//
// They are computed after WHERE, GROUP BY and HAVING: the rows the query
// produces are collected with the values the windows need, each window
// sorts them into partitions and computes its values, and the select list
// and ORDER BY are then evaluated with those values (e.wins). The query is
// therefore always buffered.

// windowArgs gives the argument counts (min, max) of the functions that
// exist only as window functions.
var windowArgs = map[string][2]int{
	"ROW_NUMBER": {0, 0}, "RANK": {0, 0}, "DENSE_RANK": {0, 0}, "PERCENT_RANK": {0, 0}, "CUME_DIST": {0, 0},
	"NTILE": {1, 1}, "LAG": {1, 3}, "LEAD": {1, 3}, "FIRST_VALUE": {1, 1}, "LAST_VALUE": {1, 1}, "NTH_VALUE": {2, 2},
}

type winSpec struct {
	name      string
	args      []evalFn
	partition []evalFn
	order     []winOrder
	frame     *winFrame // nil: the default frame
	agg       *aggSpec  // aggregate used as a window function
}

type winOrder struct {
	fn   evalFn
	desc bool
}

type winFrame struct {
	rows       bool
	start, end int    // sql.UnboundedPreceding ...
	startN     evalFn // offsets of PRECEDING / FOLLOWING bounds
	endN       evalFn
}

// winRow is a collected row: its environment and the window inputs.
type winRow struct {
	row, aggs []any
	args      [][]any  // per window: argument values
	part      [][]byte // per window: partition key
	ord       [][]any  // per window: order values
}

// hasWindow reports whether e calls a window function.
func hasWindow(e sql.Expr) bool {
	found := false
	walk(e, func(x sql.Expr) {
		if f, ok := x.(*sql.Func); ok && f.Over != nil {
			found = true
		}
	})
	return found
}

func (c *compiler) compileWindow(f *sql.Func) (evalFn, error) {
	if c.wins == nil {
		return nil, fmt.Errorf("bangdb: misuse of window function %s()", f.Name)
	}
	spec := &winSpec{name: f.Name}
	if aggregateFuncs[f.Name] {
		if len(f.Args) > 1 {
			return nil, fmt.Errorf("bangdb: %s() takes one argument", f.Name)
		}
		spec.agg = &aggSpec{name: f.Name, distinct: f.Distinct}
		if !f.Star {
			if len(f.Args) == 0 {
				return nil, fmt.Errorf("bangdb: %s() requires an argument", f.Name)
			}
			// The aggregate reads the argument value collected for the row.
			spec.agg.arg = func(e *env) (any, error) { return e.row[0], nil }
		}
	} else if !isWindowOnly(f.Name) {
		return nil, fmt.Errorf("bangdb: %s() is not a window function", f.Name)
	}
	if f.Distinct && spec.agg == nil {
		return nil, fmt.Errorf("bangdb: DISTINCT is not allowed in %s()", f.Name)
	}
	n := windowArgs[f.Name]
	if spec.agg == nil && (len(f.Args) < n[0] || len(f.Args) > n[1]) {
		return nil, fmt.Errorf("bangdb: wrong number of arguments to %s()", f.Name)
	}
	// Window inputs are evaluated in the query's row context: arguments may
	// use aggregates of a grouped query, but not other window functions.
	inner := *c
	inner.wins = nil
	for _, a := range f.Args {
		fn, err := inner.compile(a)
		if err != nil {
			return nil, err
		}
		spec.args = append(spec.args, fn)
	}
	for _, p := range f.Over.PartitionBy {
		fn, err := inner.compile(p)
		if err != nil {
			return nil, err
		}
		spec.partition = append(spec.partition, withCollation(fn, inner.collationOf(p)))
	}
	for _, o := range f.Over.OrderBy {
		fn, err := inner.compile(o.Expr)
		if err != nil {
			return nil, err
		}
		spec.order = append(spec.order, winOrder{withCollation(fn, inner.collationOf(o.Expr)), o.Desc})
	}
	if fr := f.Over.Frame; fr != nil {
		w := &winFrame{rows: fr.Rows, start: fr.Start.Kind, end: fr.End.Kind}
		for _, b := range []struct {
			bound sql.FrameBound
			fn    *evalFn
		}{{fr.Start, &w.startN}, {fr.End, &w.endN}} {
			if b.bound.N == nil {
				continue
			}
			if !fr.Rows {
				return nil, fmt.Errorf("bangdb: RANGE frames with an offset are not supported (use ROWS)")
			}
			fn, err := (&compiler{}).compile(b.bound.N)
			if err != nil {
				return nil, fmt.Errorf("bangdb: frame offsets must be constant: %w", err)
			}
			*b.fn = fn
		}
		spec.frame = w
	}
	idx := len(*c.wins)
	*c.wins = append(*c.wins, spec)
	return func(e *env) (any, error) { return e.wins[idx], nil }, nil
}

// collect returns the window inputs of the current row.
func collectWindowRow(specs []*winSpec, e *env) (winRow, error) {
	r := winRow{row: append([]any(nil), e.row...), aggs: append([]any(nil), e.aggs...)}
	for _, s := range specs {
		args := make([]any, len(s.args))
		for i, f := range s.args {
			v, err := f(e)
			if err != nil {
				return r, err
			}
			args[i] = v
		}
		var part []byte
		for _, f := range s.partition {
			v, err := f(e)
			if err != nil {
				return r, err
			}
			part = value.AppendKey(part, v)
		}
		ord := make([]any, len(s.order))
		for i, o := range s.order {
			v, err := o.fn(e)
			if err != nil {
				return r, err
			}
			ord[i] = v
		}
		r.args, r.part, r.ord = append(r.args, args), append(r.part, part), append(r.ord, ord)
	}
	return r, nil
}

// computeWindows returns, per row, the values of every window function.
func computeWindows(specs []*winSpec, rows []winRow, args []any) ([][]any, error) {
	out := make([][]any, len(rows))
	for i := range out {
		out[i] = make([]any, len(specs))
	}
	for si, s := range specs {
		if err := s.compute(si, rows, out, args); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *winSpec) compute(si int, rows []winRow, out [][]any, args []any) error {
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	cmpOrder := func(a, b int) int {
		for k, o := range s.order {
			if c := value.Compare(rows[a].ord[si][k], rows[b].ord[si][k]); c != 0 {
				if o.desc {
					return -c
				}
				return c
			}
		}
		return 0
	}
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := idx[i], idx[j]
		if c := bytes.Compare(rows[a].part[si], rows[b].part[si]); c != 0 {
			return c < 0
		}
		return cmpOrder(a, b) < 0
	})
	// Frame offsets (constant expressions).
	var startN, endN int64
	if f := s.frame; f != nil {
		for _, b := range []struct {
			fn  evalFn
			dst *int64
		}{{f.startN, &startN}, {f.endN, &endN}} {
			if b.fn == nil {
				continue
			}
			v, err := b.fn(&env{args: args})
			if err != nil {
				return err
			}
			n, ok := v.(int64)
			if !ok || n < 0 {
				return fmt.Errorf("bangdb: frame offsets must be non-negative integers")
			}
			*b.dst = n
		}
	}
	for ps := 0; ps < len(idx); {
		pe := ps + 1
		for pe < len(idx) && bytes.Equal(rows[idx[pe]].part[si], rows[idx[ps]].part[si]) {
			pe++
		}
		if err := s.computePartition(si, rows, idx[ps:pe], out, cmpOrder, startN, endN); err != nil {
			return err
		}
		ps = pe
	}
	return nil
}

// computePartition computes the window over the rows of one partition (in window
// order).
func (s *winSpec) computePartition(si int, rows []winRow, part []int, out [][]any, cmpOrder func(a, b int) int, startN, endN int64) error {
	n := len(part)
	// Peer groups: rows with equal ORDER BY values.
	peerStart := make([]int, n)
	peerEnd := make([]int, n)
	for i := 0; i < n; {
		j := i + 1
		for j < n && cmpOrder(part[i], part[j]) == 0 {
			j++
		}
		for k := i; k < j; k++ {
			peerStart[k], peerEnd[k] = i, j-1
		}
		i = j
	}
	frame := func(k int) (int, int) {
		f := s.frame
		if f == nil {
			if len(s.order) == 0 {
				return 0, n - 1
			}
			return 0, peerEnd[k]
		}
		bound := func(kind int, off int64, start bool) int {
			switch kind {
			case sql.UnboundedPreceding:
				return 0
			case sql.UnboundedFollowing:
				return n - 1
			case sql.CurrentRow:
				if f.rows {
					return k
				}
				if start {
					return peerStart[k]
				}
				return peerEnd[k]
			case sql.Preceding:
				return int(max(int64(k)-off, -1))
			default: // Following
				return int(min(int64(k)+off, int64(n)))
			}
		}
		return bound(f.start, startN, true), bound(f.end, endN, false)
	}
	arg := func(k, i int) any {
		a := rows[part[k]].args[si]
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	set := func(k int, v any) { out[part[k]][si] = v }

	switch s.name {
	case "ROW_NUMBER":
		for k := 0; k < n; k++ {
			set(k, int64(k+1))
		}
	case "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST":
		dense := int64(0)
		for k := 0; k < n; k++ {
			if peerStart[k] == k {
				dense++
			}
			rank := int64(peerStart[k] + 1)
			switch s.name {
			case "RANK":
				set(k, rank)
			case "DENSE_RANK":
				set(k, dense)
			case "PERCENT_RANK":
				if n == 1 {
					set(k, 0.0)
				} else {
					set(k, float64(rank-1)/float64(n-1))
				}
			default:
				set(k, float64(peerEnd[k]+1)/float64(n))
			}
		}
	case "NTILE":
		for k := 0; k < n; k++ {
			b, ok := arg(k, 0).(int64)
			if !ok || b <= 0 {
				return fmt.Errorf("bangdb: argument of ntile must be a positive integer")
			}
			// Groups of size n/b, the first n%b of them one larger.
			size, extra := int64(n)/b, int64(n)%b
			pos := int64(k)
			var tile int64
			if size == 0 {
				tile = pos + 1
			} else if pos < extra*(size+1) {
				tile = pos/(size+1) + 1
			} else {
				tile = extra + (pos-extra*(size+1))/size + 1
			}
			set(k, tile)
		}
	case "LAG", "LEAD":
		for k := 0; k < n; k++ {
			off := int64(1)
			if a := arg(k, 1); a != nil {
				o, ok := a.(int64)
				if !ok || o < 0 {
					return fmt.Errorf("bangdb: the offset of %s must be a non-negative integer", s.name)
				}
				off = o
			}
			j := int64(k) - off
			if s.name == "LEAD" {
				j = int64(k) + off
			}
			if j >= 0 && j < int64(n) {
				set(k, arg(int(j), 0))
			} else {
				set(k, arg(k, 2))
			}
		}
	case "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE":
		for k := 0; k < n; k++ {
			a, b := frame(k)
			a, b = max(a, 0), min(b, n-1)
			if a > b {
				set(k, nil)
				continue
			}
			switch s.name {
			case "FIRST_VALUE":
				set(k, arg(a, 0))
			case "LAST_VALUE":
				set(k, arg(b, 0))
			default:
				nth, ok := arg(k, 1).(int64)
				if !ok || nth <= 0 {
					return fmt.Errorf("bangdb: the second argument of nth_value must be a positive integer")
				}
				if int64(a)+nth-1 <= int64(b) {
					set(k, arg(a+int(nth)-1, 0))
				} else {
					set(k, nil)
				}
			}
		}
	default: // aggregate over the frame
		e := &env{row: make([]any, 1)}
		step := func(st *aggState, j int) error {
			e.row[0] = arg(j, 0)
			return st.step(e)
		}
		// Frames that start at the partition's first row grow monotonically:
		// one running state suffices.
		running := s.frame == nil || s.frame.start == sql.UnboundedPreceding
		var st *aggState
		done := -1
		for k := 0; k < n; k++ {
			a, b := frame(k)
			a, b = max(a, 0), min(b, n-1)
			if !running || st == nil || b < done {
				st, done = newAggState(s.agg), a-1
			}
			for j := max(done+1, a); j <= b; j++ {
				if err := step(st, j); err != nil {
					return err
				}
				done = j
			}
			v, err := st.result()
			if err != nil {
				return err
			}
			set(k, v)
			if !running {
				st = nil
			}
		}
	}
	return nil
}

func isWindowOnly(name string) bool { _, ok := windowArgs[name]; return ok }
