package engine

import (
	"sync"

	"github.com/minaryhub/bangdb/internal/sql"
)

// planCache reuses compiled SELECT plans. Parsed statements are cached by
// SQL text, so the same *sql.Select comes back for the same query; a plan is
// valid for the catalog it was built against. Only plans without run-time
// state are cached: no subqueries (their results are cached per run), no
// derived tables or views (materialized while planning) and no hash joins
// (the hash table is built during the run). Cached plans are shared by
// concurrent queries and are never modified after planning.
type planCache struct {
	mu sync.Mutex
	m  map[*sql.Select]cachedPlan
}

type cachedPlan struct {
	cat *Catalog
	p   *selectPlan
}

const planCacheMax = 512

func (c *planCache) get(st *sql.Select, cat *Catalog) *selectPlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[st]; ok && e.cat == cat {
		return e.p
	}
	return nil
}

func (c *planCache) put(st *sql.Select, cat *Catalog, p *selectPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= planCacheMax {
		c.m = map[*sql.Select]cachedPlan{}
	}
	c.m[st] = cachedPlan{cat, p}
}

// cacheable reports whether p (planned from st) holds no run-time state.
func (p *selectPlan) cacheable(st *sql.Select) bool {
	if len(st.Compound) > 0 {
		return false
	}
	if st.From != nil && st.From.Sub != nil {
		return false
	}
	for _, j := range st.Joins {
		if j.Table.Sub != nil || j.On != nil && containsSubquery(j.On) {
			return false
		}
	}
	exprs := []sql.Expr{st.Where, st.Having, st.Limit, st.Offset}
	for _, it := range st.Items {
		exprs = append(exprs, it.Expr)
	}
	exprs = append(exprs, st.GroupBy...)
	for _, o := range st.OrderBy {
		exprs = append(exprs, o.Expr)
	}
	for _, e := range exprs {
		if e != nil && containsSubquery(e) {
			return false
		}
	}
	var stateful func(a *access) bool
	stateful = func(a *access) bool {
		if a == nil {
			return false
		}
		if a.kind == accHash {
			return true
		}
		for _, s := range a.subs {
			if stateful(s) {
				return true
			}
		}
		return false
	}
	for _, l := range p.levels {
		if l.virtual != nil || stateful(l.acc) {
			return false
		}
	}
	return true
}
