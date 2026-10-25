package sql

import (
	"strings"
	"testing"
)

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"SELECT a, COUNT(*) FROM t WHERE x IN (1, 2) AND y LIKE 'a%' GROUP BY a HAVING COUNT(*) > 1 ORDER BY 2 DESC LIMIT 3 OFFSET 1",
		"INSERT INTO t (a, b) VALUES (?, 'x''y'), (-9223372036854775808, x'00ff')",
		"CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY AUTOINCREMENT, n VARCHAR(10) NOT NULL DEFAULT 'q' UNIQUE)",
		"UPDATE t SET a = CASE WHEN b IS NULL THEN 1 ELSE CAST(b AS INT) END WHERE a BETWEEN 1 AND 2",
		"SELECT * FROM a LEFT JOIN b ON a.id = b.id, c; BEGIN; COMMIT; EXPLAIN DELETE FROM t",
		"/* c */ SELECT 1e10, .5, \"q\".x -- end",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		stmts, _, err := Parse(src)
		if err != nil {
			return
		}
		// Formatting any parsed expression must work and re-parse.
		for _, st := range stmts {
			if sel, ok := st.(*Select); ok && sel.Where != nil {
				text := FormatExpr(sel.Where)
				if strings.Contains(text, "SELECT ...") {
					continue // subqueries are abbreviated in FormatExpr
				}
				if _, err := ParseExpr(text); err != nil {
					t.Fatalf("FormatExpr output %q does not re-parse: %v", text, err)
				}
			}
		}
	})
}
