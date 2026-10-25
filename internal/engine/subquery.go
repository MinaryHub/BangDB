package engine

import (
	"errors"
	"fmt"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/value"
)

// subplan is a planned subquery. Uncorrelated subqueries are run once per
// statement and cached; correlated ones run again for every outer row.
type subplan struct {
	x          *execCtx
	run        *selectRun
	oc         *outerCtx
	correlated bool

	cached bool
	rows   [][]any
}

func (c *compiler) subplan(sel *sql.Select) (*subplan, error) {
	if c.x == nil {
		return nil, errors.New("bangdb: subqueries are not allowed here")
	}
	sc := c.sc
	if sc == nil {
		sc = &scope{outer: c.x.outer}
	}
	oc := &outerCtx{sc: sc}
	inner := *c.x
	inner.outer = oc
	run, err := inner.prepareSelect(sel)
	if err != nil {
		return nil, err
	}
	return &subplan{x: &inner, run: run, oc: oc, correlated: oc.used}, nil
}

// rows runs the subquery for the outer row e and returns up to limit rows
// (all if limit <= 0).
func (sp *subplan) results(e *env, limit int) ([][]any, error) {
	if sp.cached {
		return sp.rows, nil
	}
	sp.oc.cur = e
	var out [][]any
	err := sp.x.runSelect(sp.run, func(vals []any) (bool, error) {
		out = append(out, vals)
		return limit <= 0 || len(out) < limit, nil
	})
	if err != nil {
		return nil, err
	}
	if !sp.correlated && limit <= 0 {
		sp.cached, sp.rows = true, out
	}
	return out, nil
}

func (sp *subplan) columns() int { return len(sp.run.p.cols) }

// compileScalarSubquery: (SELECT x ...) yields the first row's value, or
// NULL when there is no row.
func (c *compiler) compileScalarSubquery(sel *sql.Select) (evalFn, error) {
	sp, err := c.subplan(sel)
	if err != nil {
		return nil, err
	}
	if sp.columns() != 1 {
		return nil, fmt.Errorf("bangdb: scalar subquery returns %d columns, expected 1", sp.columns())
	}
	limit := 1
	if !sp.correlated {
		limit = 0 // run once and cache
	}
	return func(e *env) (any, error) {
		rows, err := sp.results(e, limit)
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		return rows[0][0], nil
	}, nil
}

func (c *compiler) compileExists(x *sql.Exists) (evalFn, error) {
	sp, err := c.subplan(x.Select)
	if err != nil {
		return nil, err
	}
	not := x.Not
	return func(e *env) (any, error) {
		rows, err := sp.results(e, 1)
		if err != nil {
			return nil, err
		}
		return boolVal((len(rows) > 0) != not), nil
	}, nil
}

// compileInSubquery: x [NOT] IN (SELECT y ...), with SQL's NULL rules.
func (c *compiler) compileInSubquery(x *sql.In) (evalFn, error) {
	lhs, err := c.compile(x.X)
	if err != nil {
		return nil, err
	}
	sp, err := c.subplan(x.Sub)
	if err != nil {
		return nil, err
	}
	if sp.columns() != 1 {
		return nil, fmt.Errorf("bangdb: subquery in IN returns %d columns, expected 1", sp.columns())
	}
	not := x.Not
	// For uncorrelated subqueries the value set is built once.
	var set map[string]bool
	var setHasNull bool
	member := func(e *env, v any) (found, sawNull bool, err error) {
		if !sp.correlated && set != nil {
			return set[string(numKey(v))], setHasNull, nil
		}
		rows, err := sp.results(e, 0)
		if err != nil {
			return false, false, err
		}
		if !sp.correlated {
			set = map[string]bool{}
			for _, r := range rows {
				if r[0] == nil {
					setHasNull = true
				} else {
					set[string(numKey(r[0]))] = true
				}
			}
			return set[string(numKey(v))], setHasNull, nil
		}
		for _, r := range rows {
			if r[0] == nil {
				sawNull = true
			} else if value.Compare(v, r[0]) == 0 {
				return true, sawNull, nil
			}
		}
		return false, sawNull, nil
	}
	return func(e *env) (any, error) {
		v, err := lhs(e)
		if err != nil || v == nil {
			return nil, err
		}
		found, sawNull, err := member(e, v)
		if err != nil {
			return nil, err
		}
		if found {
			return boolVal(!not), nil
		}
		if sawNull {
			return nil, nil
		}
		return boolVal(not), nil
	}, nil
}

// numKey is a key under which values that compare equal collide: integers
// and integral reals share a key, as value.Compare treats them as equal.
func numKey(v any) []byte {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		v = int64(f)
	}
	return value.AppendKey(nil, v)
}

// containsSubquery reports whether e contains a subquery.
func containsSubquery(e sql.Expr) bool {
	found := false
	walk(e, func(n sql.Expr) {
		switch x := n.(type) {
		case *sql.Subquery, *sql.Exists:
			found = true
		case *sql.In:
			found = found || x.Sub != nil
		}
	})
	return found
}
