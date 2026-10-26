package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// env is the runtime state an expression is evaluated against.
type env struct {
	row  []any // combined row of all tables in scope
	aggs []any // aggregate results for the current group
	wins []any // window function results for the current row
	args []any // bound parameters
	buf  [8]any
}

// newEnv returns an env with a row of n slots (allocated together with it
// when small).
func newEnv(n int, args []any) *env {
	e := &env{args: args}
	if n <= len(e.buf) {
		e.row = e.buf[:n:n]
	} else {
		e.row = make([]any, n)
	}
	return e
}

type evalFn func(e *env) (any, error)

type scopeCol struct {
	table  string // alias (or table name), lower-case
	name   string
	level  int // position of the table in the FROM list
	hidden bool
	coll   string // collation of the column (NOCASE, RTRIM or "")
}

// scope maps column references to positions in env.row. outer links to
// the enclosing query when this scope belongs to a subquery.
type scope struct {
	cols  []scopeCol
	outer *outerCtx
}

// outerCtx is the enclosing query of a subquery: its scope, and the row it
// is currently evaluating (set each time the subquery runs).
type outerCtx struct {
	sc   *scope
	cur  *env
	used bool // the subquery references this scope (it is correlated)
}

// resolveOuter finds a column in the enclosing queries and returns an
// evaluator reading it from the outer row being processed.
func (s *scope) resolveOuter(tbl, name string) (evalFn, bool) {
	var chain []*outerCtx
	for o := s.outer; o != nil && o.sc != nil; o = o.sc.outer {
		chain = append(chain, o)
		if i, err := o.sc.resolve(tbl, name); err == nil {
			for _, c := range chain {
				c.used = true
			}
			return func(*env) (any, error) { return o.cur.row[i], nil }, true
		}
	}
	return nil, false
}

// isOuter reports whether a column reference belongs to an enclosing query.
func (s *scope) isOuter(tbl, name string) bool {
	for o := s.outer; o != nil && o.sc != nil; o = o.sc.outer {
		if _, err := o.sc.resolve(tbl, name); err == nil {
			return true
		}
	}
	return false
}

func (s *scope) resolve(tbl, name string) (int, error) {
	tbl, name = strings.ToLower(tbl), strings.ToLower(name)
	found := -1
	for pass := 0; pass < 2 && found < 0; pass++ {
		for i, c := range s.cols {
			if c.hidden != (pass == 1) || c.name != name || (tbl != "" && c.table != tbl) {
				continue
			}
			if found >= 0 {
				return 0, fmt.Errorf("bangdb: ambiguous column name %q", name)
			}
			found = i
		}
	}
	if found < 0 {
		if tbl != "" {
			return 0, fmt.Errorf("bangdb: no such column: %s.%s", tbl, name)
		}
		return 0, fmt.Errorf("bangdb: no such column: %s", name)
	}
	return found, nil
}

// aggSpec is one aggregate call found while compiling a grouped query.
type aggSpec struct {
	name     string
	arg      evalFn // nil for COUNT(*)
	distinct bool
}

type compiler struct {
	x        *execCtx // needed to plan subqueries; nil where they are not allowed
	sc       *scope
	allowAgg bool
	aggs     *[]*aggSpec
	wins     *[]*winSpec         // nil where window functions are not allowed
	aliases  map[string]sql.Expr // SELECT aliases usable in HAVING / ORDER BY
}

var aggregateFuncs = map[string]bool{"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true, "TOTAL": true, "GROUP_CONCAT": true,
	"JSON_GROUP_ARRAY": true, "JSON_GROUP_OBJECT": true}

func isAggregateCall(f *sql.Func) bool {
	if !aggregateFuncs[f.Name] || f.Over != nil {
		return false
	}
	// min(a, b) / max(a, b) are scalar functions.
	return !((f.Name == "MIN" || f.Name == "MAX") && len(f.Args) > 1)
}

// hasAggregate reports whether e contains an aggregate call.
func hasAggregate(e sql.Expr) bool {
	found := false
	walk(e, func(x sql.Expr) {
		if f, ok := x.(*sql.Func); ok && isAggregateCall(f) {
			found = true
		}
	})
	return found
}

func walk(e sql.Expr, fn func(sql.Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch x := e.(type) {
	case *sql.Unary:
		walk(x.X, fn)
	case *sql.Binary:
		walk(x.L, fn)
		walk(x.R, fn)
	case *sql.IsNull:
		walk(x.X, fn)
	case *sql.In:
		walk(x.X, fn)
		for _, a := range x.List {
			walk(a, fn)
		}
	case *sql.Between:
		walk(x.X, fn)
		walk(x.Lo, fn)
		walk(x.Hi, fn)
	case *sql.Like:
		walk(x.X, fn)
		walk(x.Pattern, fn)
	case *sql.Func:
		for _, a := range x.Args {
			walk(a, fn)
		}
		if x.Over != nil {
			for _, p := range x.Over.PartitionBy {
				walk(p, fn)
			}
			for _, o := range x.Over.OrderBy {
				walk(o.Expr, fn)
			}
		}
	case *sql.Collate:
		walk(x.X, fn)
	case *sql.Case:
		walk(x.Operand, fn)
		for _, w := range x.Whens {
			walk(w.Cond, fn)
			walk(w.Result, fn)
		}
		walk(x.Else, fn)
	}
}

func (c *compiler) compile(e sql.Expr) (evalFn, error) {
	switch x := e.(type) {
	case *sql.Literal:
		v := x.Value
		return func(*env) (any, error) { return v, nil }, nil
	case *sql.Param:
		i := x.Index
		return func(e *env) (any, error) {
			if i >= len(e.args) {
				return nil, fmt.Errorf("bangdb: missing argument for parameter %d", i+1)
			}
			return e.args[i], nil
		}, nil
	case *sql.ColumnRef:
		if c.sc == nil {
			return nil, fmt.Errorf("bangdb: no such column: %s", x.Name)
		}
		i, err := c.sc.resolve(x.Table, x.Name)
		if err != nil {
			if alias, ok := c.aliases[strings.ToLower(x.Name)]; ok && x.Table == "" {
				// Prevent infinite recursion on self-referencing aliases.
				inner := *c
				inner.aliases = nil
				return inner.compile(alias)
			}
			if f, ok := c.sc.resolveOuter(x.Table, x.Name); ok {
				return f, nil
			}
			return nil, err
		}
		return func(e *env) (any, error) { return e.row[i], nil }, nil
	case *sql.Unary:
		inner, err := c.compile(x.X)
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case "NOT":
			return func(e *env) (any, error) {
				v, err := inner(e)
				if err != nil || v == nil {
					return nil, err
				}
				b, _ := value.Truthy(v)
				return boolVal(!b), nil
			}, nil
		case "-":
			return func(e *env) (any, error) {
				v, err := inner(e)
				if err != nil {
					return nil, err
				}
				return arith("-", int64(0), v)
			}, nil
		}
		return inner, nil
	case *sql.Binary:
		return c.compileBinary(x)
	case *sql.IsNull:
		inner, err := c.compile(x.X)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(e *env) (any, error) {
			v, err := inner(e)
			if err != nil {
				return nil, err
			}
			return boolVal((v == nil) != not), nil
		}, nil
	case *sql.In:
		if x.Sub != nil {
			return c.compileInSubquery(x)
		}
		lhs, err := c.compile(x.X)
		if err != nil {
			return nil, err
		}
		list, err := c.compileList(x.List)
		if err != nil {
			return nil, err
		}
		if coll := c.collationOf(x.X); coll != "" {
			lhs = withCollation(lhs, coll)
			for i := range list {
				list[i] = withCollation(list[i], coll)
			}
		}
		not := x.Not
		return func(e *env) (any, error) {
			v, err := lhs(e)
			if err != nil || v == nil {
				return nil, err
			}
			sawNull := false
			for _, f := range list {
				w, err := f(e)
				if err != nil {
					return nil, err
				}
				if w == nil {
					sawNull = true
				} else if value.Compare(v, w) == 0 {
					return boolVal(!not), nil
				}
			}
			if sawNull {
				return nil, nil
			}
			return boolVal(not), nil
		}, nil
	case *sql.Between:
		return c.compile(betweenAsAnd(x))
	case *sql.Collate:
		return c.compile(x.X) // collations only matter where values are compared
	case *sql.Subquery:
		return c.compileScalarSubquery(x.Select)
	case *sql.Exists:
		return c.compileExists(x)
	case *sql.Like:
		lhs, err := c.compile(x.X)
		if err != nil {
			return nil, err
		}
		pat, err := c.compile(x.Pattern)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(e *env) (any, error) {
			v, err := lhs(e)
			if err != nil {
				return nil, err
			}
			p, err := pat(e)
			if err != nil || v == nil || p == nil {
				return nil, err
			}
			return boolVal(likeMatch(value.Format(p), value.Format(v)) != not), nil
		}, nil
	case *sql.Func:
		return c.compileFunc(x)
	case *sql.Case:
		return c.compileCase(x)
	}
	return nil, fmt.Errorf("bangdb: unsupported expression %T", e)
}

func betweenAsAnd(x *sql.Between) sql.Expr {
	var e sql.Expr = &sql.Binary{Op: "AND", L: &sql.Binary{Op: ">=", L: x.X, R: x.Lo}, R: &sql.Binary{Op: "<=", L: x.X, R: x.Hi}}
	if x.Not {
		e = &sql.Unary{Op: "NOT", X: e}
	}
	return e
}

func (c *compiler) compileList(list []sql.Expr) ([]evalFn, error) {
	out := make([]evalFn, len(list))
	for i, a := range list {
		f, err := c.compile(a)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

func boolVal(b bool) any {
	if b {
		return int64(1)
	}
	return int64(0)
}

func (c *compiler) compileBinary(x *sql.Binary) (evalFn, error) {
	l, err := c.compile(x.L)
	if err != nil {
		return nil, err
	}
	r, err := c.compile(x.R)
	if err != nil {
		return nil, err
	}
	op := x.Op
	switch op {
	case "AND", "OR":
		isAnd := op == "AND"
		return func(e *env) (any, error) {
			lv, err := l(e)
			if err != nil {
				return nil, err
			}
			lb, lok := value.Truthy(lv)
			if lok && lb != isAnd { // FALSE AND x, TRUE OR x
				return boolVal(lb), nil
			}
			rv, err := r(e)
			if err != nil {
				return nil, err
			}
			rb, rok := value.Truthy(rv)
			if rok && rb != isAnd {
				return boolVal(rb), nil
			}
			if !lok || !rok {
				return nil, nil
			}
			return boolVal(isAnd), nil
		}, nil
	case "=", "!=", "<", "<=", ">", ">=":
		if coll := c.comparisonCollation(x.L, x.R); coll != "" {
			l, r = withCollation(l, coll), withCollation(r, coll)
		}
		return func(e *env) (any, error) {
			lv, err := l(e)
			if err != nil {
				return nil, err
			}
			rv, err := r(e)
			if err != nil || lv == nil || rv == nil {
				return nil, err
			}
			c := value.Compare(lv, rv)
			var b bool
			switch op {
			case "=":
				b = c == 0
			case "!=":
				b = c != 0
			case "<":
				b = c < 0
			case "<=":
				b = c <= 0
			case ">":
				b = c > 0
			case ">=":
				b = c >= 0
			}
			return boolVal(b), nil
		}, nil
	case "->", "->>":
		asJSON := op == "->"
		return func(e *env) (any, error) {
			lv, err := l(e)
			if err != nil {
				return nil, err
			}
			rv, err := r(e)
			if err != nil {
				return nil, err
			}
			return jsonArrow(lv, rv, asJSON)
		}, nil
	case "||":
		return func(e *env) (any, error) {
			lv, err := l(e)
			if err != nil {
				return nil, err
			}
			rv, err := r(e)
			if err != nil || lv == nil || rv == nil {
				return nil, err
			}
			return value.Format(lv) + value.Format(rv), nil
		}, nil
	}
	return func(e *env) (any, error) {
		lv, err := l(e)
		if err != nil {
			return nil, err
		}
		rv, err := r(e)
		if err != nil {
			return nil, err
		}
		return arith(op, lv, rv)
	}, nil
}

func toNumber(v any) (any, error) {
	switch x := v.(type) {
	case int64, float64:
		return x, nil
	case string:
		s := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, nil
		}
	}
	return nil, fmt.Errorf("bangdb: cannot use %s value %q in arithmetic", value.TypeOf(v), value.Format(v))
}

func arith(op string, a, b any) (any, error) {
	if a == nil || b == nil {
		return nil, nil
	}
	a, err := toNumber(a)
	if err != nil {
		return nil, err
	}
	b, err = toNumber(b)
	if err != nil {
		return nil, err
	}
	ai, aInt := a.(int64)
	bi, bInt := b.(int64)
	if aInt && bInt {
		switch op {
		case "+":
			s := ai + bi
			if (s > ai) == (bi > 0) {
				return s, nil
			}
		case "-":
			s := ai - bi
			if (s < ai) == (bi > 0) {
				return s, nil
			}
		case "*":
			if ai == 0 || bi == 0 {
				return int64(0), nil
			}
			s := ai * bi
			if s/bi == ai && !(ai == -1 && bi == math.MinInt64) && !(bi == -1 && ai == math.MinInt64) {
				return s, nil
			}
		case "/":
			if bi == 0 {
				return nil, nil
			}
			if !(ai == math.MinInt64 && bi == -1) {
				return ai / bi, nil
			}
		case "%":
			if bi == 0 {
				return nil, nil
			}
			if bi == -1 {
				return int64(0), nil
			}
			return ai % bi, nil
		}
		// integer overflow: fall through to floating point
	}
	af, bf := toFloat(a), toFloat(b)
	switch op {
	case "+":
		return af + bf, nil
	case "-":
		return af - bf, nil
	case "*":
		return af * bf, nil
	case "/":
		if bf == 0 {
			return nil, nil
		}
		return af / bf, nil
	case "%":
		if bf == 0 {
			return nil, nil
		}
		return math.Mod(af, bf), nil
	}
	return nil, fmt.Errorf("bangdb: unknown operator %s", op)
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// likeMatch implements SQL LIKE with % and _, case-insensitive for ASCII.
func likeMatch(pattern, s string) bool {
	p, t := []rune(pattern), []rune(s)
	pi, ti, star, mark := 0, 0, -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && p[pi] == '%':
			star, mark = pi, ti
			pi++
		case pi < len(p) && (p[pi] == '_' || foldASCII(p[pi]) == foldASCII(t[ti])):
			pi++
			ti++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

// foldASCII folds case for LIKE (Unicode-aware despite its name: ASCII
// takes a fast path).
func foldASCII(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	if r < 0x80 {
		return r
	}
	return unicode.ToLower(r)
}

// collationFold returns the text folding of a collation (nil = binary).
func collationFold(name string) func(string) string {
	switch name {
	case "NOCASE":
		return strings.ToLower
	case "RTRIM":
		return func(s string) string { return strings.TrimRight(s, " ") }
	}
	return nil
}

// foldValue applies a collation to a text value.
func foldValue(v any, coll string) any {
	if s, ok := v.(string); ok {
		if f := collationFold(coll); f != nil {
			return f(s)
		}
	}
	return v
}

func withCollation(fn evalFn, coll string) evalFn {
	if collationFold(coll) == nil {
		return fn
	}
	return func(e *env) (any, error) {
		v, err := fn(e)
		return foldValue(v, coll), err
	}
}

// collationOf returns the collation of an expression: an explicit COLLATE,
// or the collation of a column it directly refers to.
func (c *compiler) collationOf(e sql.Expr) string {
	switch x := e.(type) {
	case *sql.Collate:
		return x.Name
	case *sql.ColumnRef:
		if c.sc != nil {
			if i, err := c.sc.resolve(x.Table, x.Name); err == nil {
				return c.sc.cols[i].coll
			}
		}
	}
	return ""
}

// comparisonCollation follows SQLite: an explicit COLLATE on the left, then
// on the right, then the left column's collation, then the right's.
func (c *compiler) comparisonCollation(l, r sql.Expr) string {
	if x, ok := l.(*sql.Collate); ok {
		return x.Name
	}
	if x, ok := r.(*sql.Collate); ok {
		return x.Name
	}
	if coll := c.collationOf(l); coll != "" {
		return coll
	}
	return c.collationOf(r)
}

func (c *compiler) compileCase(x *sql.Case) (evalFn, error) {
	var operand evalFn
	var err error
	if x.Operand != nil {
		if operand, err = c.compile(x.Operand); err != nil {
			return nil, err
		}
	}
	conds := make([]evalFn, len(x.Whens))
	results := make([]evalFn, len(x.Whens))
	for i, w := range x.Whens {
		if conds[i], err = c.compile(w.Cond); err != nil {
			return nil, err
		}
		if results[i], err = c.compile(w.Result); err != nil {
			return nil, err
		}
	}
	var elseFn evalFn
	if x.Else != nil {
		if elseFn, err = c.compile(x.Else); err != nil {
			return nil, err
		}
	}
	return func(e *env) (any, error) {
		var opv any
		if operand != nil {
			var err error
			if opv, err = operand(e); err != nil {
				return nil, err
			}
		}
		for i, cond := range conds {
			cv, err := cond(e)
			if err != nil {
				return nil, err
			}
			var hit bool
			if operand != nil {
				hit = opv != nil && cv != nil && value.Compare(opv, cv) == 0
			} else {
				hit, _ = value.Truthy(cv)
			}
			if hit {
				return results[i](e)
			}
		}
		if elseFn != nil {
			return elseFn(e)
		}
		return nil, nil
	}, nil
}

func (c *compiler) compileFunc(f *sql.Func) (evalFn, error) {
	if f.Over != nil {
		return c.compileWindow(f)
	}
	if isWindowOnly(f.Name) {
		return nil, fmt.Errorf("bangdb: %s() requires an OVER clause", f.Name)
	}
	if isAggregateCall(f) {
		if !c.allowAgg || c.aggs == nil {
			return nil, fmt.Errorf("bangdb: misuse of aggregate function %s()", f.Name)
		}
		spec := &aggSpec{name: f.Name, distinct: f.Distinct}
		if !f.Star {
			if f.Name == "JSON_GROUP_OBJECT" {
				if len(f.Args) != 2 {
					return nil, fmt.Errorf("bangdb: %s() takes exactly two arguments", f.Name)
				}
			} else if len(f.Args) != 1 && f.Name != "GROUP_CONCAT" {
				return nil, fmt.Errorf("bangdb: %s() takes exactly one argument", f.Name)
			}
			if len(f.Args) == 0 {
				return nil, fmt.Errorf("bangdb: %s() requires an argument", f.Name)
			}
			inner := &compiler{x: c.x, sc: c.sc}
			arg, err := inner.compile(f.Args[0])
			if err != nil {
				return nil, err
			}
			if jsonValueArg(f.Name, 0) && jsonProducing(f.Args[0]) {
				arg = asJSONText(arg)
			}
			if f.Name == "JSON_GROUP_OBJECT" {
				val, err := inner.compile(f.Args[1])
				if err != nil {
					return nil, err
				}
				if jsonProducing(f.Args[1]) {
					val = asJSONText(val)
				}
				key := arg
				arg = func(e *env) (any, error) {
					k, err := key(e)
					if err != nil {
						return nil, err
					}
					v, err := val(e)
					return jsonPair{k, v}, err
				}
			}
			spec.arg = arg
		} else if f.Name != "COUNT" {
			return nil, fmt.Errorf("bangdb: %s(*) is not supported", f.Name)
		}
		idx := len(*c.aggs)
		*c.aggs = append(*c.aggs, spec)
		return func(e *env) (any, error) { return e.aggs[idx], nil }, nil
	}
	args, err := c.compileList(f.Args)
	if err != nil {
		return nil, err
	}
	for i, a := range f.Args {
		if jsonValueArg(f.Name, i) && jsonProducing(a) {
			args[i] = asJSONText(args[i])
		}
	}
	impl, ok := scalarFuncs[f.Name]
	if !ok {
		return nil, fmt.Errorf("bangdb: no such function: %s", f.Name)
	}
	if impl.minArgs >= 0 && len(args) < impl.minArgs || impl.maxArgs >= 0 && len(args) > impl.maxArgs {
		return nil, fmt.Errorf("bangdb: wrong number of arguments to %s()", f.Name)
	}
	if impl.lazy != nil {
		return impl.lazy(args), nil
	}
	fn := impl.fn
	return func(e *env) (any, error) {
		vals := make([]any, len(args))
		for i, a := range args {
			v, err := a(e)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		return fn(vals)
	}, nil
}

type scalarFunc struct {
	minArgs, maxArgs int
	fn               func(args []any) (any, error)
	lazy             func(args []evalFn) evalFn // for short-circuit functions
}

var scalarFuncs = map[string]scalarFunc{
	"LOWER":     {1, 1, strFunc(strings.ToLower), nil},
	"UPPER":     {1, 1, strFunc(strings.ToUpper), nil},
	"TRIM":      {1, 1, strFunc(strings.TrimSpace), nil},
	"LTRIM":     {1, 1, strFunc(func(s string) string { return strings.TrimLeft(s, " \t\r\n") }), nil},
	"RTRIM":     {1, 1, strFunc(func(s string) string { return strings.TrimRight(s, " \t\r\n") }), nil},
	"LENGTH":    {1, 1, fnLength, nil},
	"ABS":       {1, 1, fnAbs, nil},
	"ROUND":     {1, 2, fnRound, nil},
	"TYPEOF":    {1, 1, func(a []any) (any, error) { return strings.ToLower(value.TypeOf(a[0])), nil }, nil},
	"SUBSTR":    {2, 3, fnSubstr, nil},
	"SUBSTRING": {2, 3, fnSubstr, nil},
	"REPLACE": {3, 3, func(a []any) (any, error) {
		if a[0] == nil || a[1] == nil || a[2] == nil {
			return nil, nil
		}
		return strings.ReplaceAll(value.Format(a[0]), value.Format(a[1]), value.Format(a[2])), nil
	}, nil},
	"INSTR": {2, 2, func(a []any) (any, error) {
		if a[0] == nil || a[1] == nil {
			return nil, nil
		}
		i := strings.Index(value.Format(a[0]), value.Format(a[1]))
		if i < 0 {
			return int64(0), nil
		}
		return int64(utf8.RuneCountInString(value.Format(a[0])[:i]) + 1), nil
	}, nil},
	"NULLIF": {2, 2, func(a []any) (any, error) {
		if a[0] != nil && a[1] != nil && value.Compare(a[0], a[1]) == 0 {
			return nil, nil
		}
		return a[0], nil
	}, nil},
	"MIN":      {2, -1, func(a []any) (any, error) { return extreme(a, -1), nil }, nil},
	"MAX":      {2, -1, func(a []any) (any, error) { return extreme(a, 1), nil }, nil},
	"COALESCE": {1, -1, nil, coalesce},
	"IFNULL":   {2, 2, nil, coalesce},
	"CAST":     {2, 2, fnCast, nil},
	"CURRENT_TIMESTAMP": {0, 0, func([]any) (any, error) {
		return time.Now().UTC().Format("2006-01-02 15:04:05"), nil
	}, nil},
	"CURRENT_DATE": {0, 0, func([]any) (any, error) { return time.Now().UTC().Format("2006-01-02"), nil }, nil},
	"CURRENT_TIME": {0, 0, func([]any) (any, error) { return time.Now().UTC().Format("15:04:05"), nil }, nil},
	"NOW": {0, 0, func([]any) (any, error) {
		return time.Now().UTC().Format(time.RFC3339Nano), nil
	}, nil},
}

func coalesce(args []evalFn) evalFn {
	return func(e *env) (any, error) {
		for _, a := range args {
			v, err := a(e)
			if err != nil || v != nil {
				return v, err
			}
		}
		return nil, nil
	}
}

func extreme(a []any, sign int) any {
	var best any
	for _, v := range a {
		if v == nil {
			return nil
		}
		if best == nil || value.Compare(v, best)*sign > 0 {
			best = v
		}
	}
	return best
}

func strFunc(f func(string) string) func([]any) (any, error) {
	return func(a []any) (any, error) {
		if a[0] == nil {
			return nil, nil
		}
		return f(value.Format(a[0])), nil
	}
}

func fnLength(a []any) (any, error) {
	switch x := a[0].(type) {
	case nil:
		return nil, nil
	case []byte:
		return int64(len(x)), nil
	default:
		return int64(utf8.RuneCountInString(value.Format(x))), nil
	}
}

func fnAbs(a []any) (any, error) {
	switch x := a[0].(type) {
	case nil:
		return nil, nil
	case int64:
		if x == math.MinInt64 {
			return nil, fmt.Errorf("bangdb: integer overflow in ABS()")
		}
		if x < 0 {
			return -x, nil
		}
		return x, nil
	case float64:
		return math.Abs(x), nil
	}
	n, err := toNumber(a[0])
	if err != nil {
		return nil, err
	}
	return fnAbs([]any{n})
}

func fnRound(a []any) (any, error) {
	if a[0] == nil {
		return nil, nil
	}
	n, err := toNumber(a[0])
	if err != nil {
		return nil, err
	}
	digits := int64(0)
	if len(a) > 1 && a[1] != nil {
		d, err := toNumber(a[1])
		if err != nil {
			return nil, err
		}
		digits = int64(toFloat(d))
	}
	p := math.Pow(10, float64(digits))
	return math.Round(toFloat(n)*p) / p, nil
}

func fnSubstr(a []any) (any, error) {
	if a[0] == nil || a[1] == nil {
		return nil, nil
	}
	r := []rune(value.Format(a[0]))
	startN, err := toNumber(a[1])
	if err != nil {
		return nil, err
	}
	start := int(toFloat(startN))
	length := len(r) + 1
	if len(a) > 2 {
		if a[2] == nil {
			return nil, nil
		}
		ln, err := toNumber(a[2])
		if err != nil {
			return nil, err
		}
		length = int(toFloat(ln))
	}
	// SQL positions are 1-based; non-positive starts eat into the length.
	if start < 1 {
		length += start - 1
		start = 1
	}
	if length < 0 {
		length = 0
	}
	from := min(start-1, len(r))
	to := min(from+length, len(r))
	return string(r[from:to]), nil
}

func fnCast(a []any) (any, error) {
	if a[0] == nil {
		return nil, nil
	}
	t := value.ParseType(a[1].(string))
	switch t {
	case value.Integer:
		switch x := a[0].(type) {
		case float64:
			return int64(x), nil
		case string:
			n, err := toNumber(x)
			if err != nil {
				return int64(0), nil
			}
			if f, ok := n.(float64); ok {
				return int64(f), nil
			}
			return n, nil
		case []byte:
			return fnCast([]any{string(x), a[1]})
		}
		return a[0], nil
	case value.Real:
		switch x := a[0].(type) {
		case string, []byte:
			n, err := toNumber(value.Format(x))
			if err != nil {
				return float64(0), nil
			}
			return toFloat(n), nil
		}
		return toFloat(a[0]), nil
	case value.Text:
		if b, ok := a[0].([]byte); ok {
			return string(b), nil
		}
		return value.Format(a[0]), nil
	case value.Blob:
		if b, ok := a[0].([]byte); ok {
			return b, nil
		}
		return []byte(value.Format(a[0])), nil
	}
	return a[0], nil
}

// ---- aggregate state ----

type aggState struct {
	spec  *aggSpec
	count int64
	sumI  int64
	sumF  float64
	float bool
	ovf   bool
	best  any
	parts []string
	seen  map[string]bool
	items []any    // JSON_GROUP_ARRAY
	obj   *jsonObj // JSON_GROUP_OBJECT
}

func newAggState(s *aggSpec) *aggState {
	st := &aggState{spec: s}
	if s.distinct {
		st.seen = map[string]bool{}
	}
	return st
}

func (st *aggState) step(e *env) error {
	if st.spec.arg == nil { // COUNT(*)
		st.count++
		return nil
	}
	v, err := st.spec.arg(e)
	if err != nil || v == nil && st.spec.name != "JSON_GROUP_ARRAY" {
		return err
	}
	if st.seen != nil {
		k := string(value.AppendKey(nil, v))
		if st.seen[k] {
			return nil
		}
		st.seen[k] = true
	}
	st.count++
	switch st.spec.name {
	case "SUM", "AVG", "TOTAL":
		n, err := toNumber(v)
		if err != nil {
			return err
		}
		switch x := n.(type) {
		case int64:
			if !st.float && !st.ovf {
				s := st.sumI + x
				if (s > st.sumI) == (x > 0) || x == 0 {
					st.sumI = s
				} else {
					st.ovf = true
				}
			}
			st.sumF += float64(x)
		case float64:
			st.float = true
			st.sumF += x
		}
	case "MIN":
		if st.best == nil || value.Compare(v, st.best) < 0 {
			st.best = v
		}
	case "MAX":
		if st.best == nil || value.Compare(v, st.best) > 0 {
			st.best = v
		}
	case "GROUP_CONCAT":
		st.parts = append(st.parts, value.Format(v))
	case "JSON_GROUP_ARRAY", "JSON_GROUP_OBJECT":
		return st.stepJSON(v)
	}
	return nil
}

func (st *aggState) result() (any, error) {
	switch st.spec.name {
	case "COUNT":
		return st.count, nil
	case "SUM":
		if st.count == 0 {
			return nil, nil
		}
		if st.float {
			return st.sumF, nil
		}
		if st.ovf {
			return nil, fmt.Errorf("bangdb: integer overflow in SUM()")
		}
		return st.sumI, nil
	case "TOTAL":
		return st.sumF, nil
	case "AVG":
		if st.count == 0 {
			return nil, nil
		}
		return st.sumF / float64(st.count), nil
	case "MIN", "MAX":
		return st.best, nil
	case "GROUP_CONCAT":
		if st.count == 0 {
			return nil, nil
		}
		return strings.Join(st.parts, ","), nil
	case "JSON_GROUP_ARRAY", "JSON_GROUP_OBJECT":
		return st.resultJSON(), nil
	}
	return nil, nil
}
