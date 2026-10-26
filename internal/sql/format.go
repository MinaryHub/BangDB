package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatExpr renders an expression back to SQL text. It is used for result
// column names and for persisting DEFAULT expressions.
func FormatExpr(e Expr) string {
	var sb strings.Builder
	formatExpr(&sb, e)
	return sb.String()
}

func formatExpr(sb *strings.Builder, e Expr) {
	switch x := e.(type) {
	case *Literal:
		switch v := x.Value.(type) {
		case nil:
			sb.WriteString("NULL")
		case int64:
			sb.WriteString(strconv.FormatInt(v, 10))
		case float64:
			s := strconv.FormatFloat(v, 'g', -1, 64)
			if !strings.ContainsAny(s, ".eEn") { // keep it a REAL literal
				s += ".0"
			}
			sb.WriteString(s)
		case string:
			sb.WriteString("'" + strings.ReplaceAll(v, "'", "''") + "'")
		case []byte:
			fmt.Fprintf(sb, "x'%x'", v)
		}
	case *Param:
		if x.Name != "" {
			sb.WriteString(":" + x.Name)
		} else {
			fmt.Fprintf(sb, "?%d", x.Index+1)
		}
	case *ColumnRef:
		if x.Table != "" {
			sb.WriteString(QuoteIdent(x.Table) + ".")
		}
		sb.WriteString(QuoteIdent(x.Name))
	case *Unary:
		if x.Op == "NOT" {
			sb.WriteString("NOT ")
		} else {
			sb.WriteString(x.Op)
		}
		formatExpr(sb, x.X)
	case *Binary:
		sb.WriteString("(")
		formatExpr(sb, x.L)
		sb.WriteString(" " + x.Op + " ")
		formatExpr(sb, x.R)
		sb.WriteString(")")
	case *IsNull:
		formatExpr(sb, x.X)
		if x.Not {
			sb.WriteString(" IS NOT NULL")
		} else {
			sb.WriteString(" IS NULL")
		}
	case *In:
		formatExpr(sb, x.X)
		if x.Not {
			sb.WriteString(" NOT")
		}
		sb.WriteString(" IN (")
		if x.Sub != nil {
			sb.WriteString("SELECT ...")
		}
		for i, a := range x.List {
			if i > 0 {
				sb.WriteString(", ")
			}
			formatExpr(sb, a)
		}
		sb.WriteString(")")
	case *Between:
		formatExpr(sb, x.X)
		if x.Not {
			sb.WriteString(" NOT")
		}
		sb.WriteString(" BETWEEN ")
		formatExpr(sb, x.Lo)
		sb.WriteString(" AND ")
		formatExpr(sb, x.Hi)
	case *Like:
		formatExpr(sb, x.X)
		if x.Not {
			sb.WriteString(" NOT")
		}
		sb.WriteString(" LIKE ")
		formatExpr(sb, x.Pattern)
	case *Func:
		switch x.Name {
		case "CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME":
			sb.WriteString(x.Name)
			return
		case "CAST":
			sb.WriteString("CAST(")
			formatExpr(sb, x.Args[0])
			sb.WriteString(" AS " + x.Args[1].(*Literal).Value.(string) + ")")
			return
		}
		sb.WriteString(x.Name + "(")
		if x.Star {
			sb.WriteString("*")
		}
		if x.Distinct {
			sb.WriteString("DISTINCT ")
		}
		for i, a := range x.Args {
			if i > 0 {
				sb.WriteString(", ")
			}
			formatExpr(sb, a)
		}
		sb.WriteString(")")
		if x.Over != nil {
			sb.WriteString(" OVER (")
			sep := ""
			if len(x.Over.PartitionBy) > 0 {
				sb.WriteString("PARTITION BY ")
				for i, a := range x.Over.PartitionBy {
					if i > 0 {
						sb.WriteString(", ")
					}
					formatExpr(sb, a)
				}
				sep = " "
			}
			if len(x.Over.OrderBy) > 0 {
				sb.WriteString(sep + "ORDER BY ")
				for i, o := range x.Over.OrderBy {
					if i > 0 {
						sb.WriteString(", ")
					}
					formatExpr(sb, o.Expr)
					if o.Desc {
						sb.WriteString(" DESC")
					}
				}
				sep = " "
			}
			if f := x.Over.Frame; f != nil {
				sb.WriteString(sep)
				if f.Rows {
					sb.WriteString("ROWS BETWEEN ")
				} else {
					sb.WriteString("RANGE BETWEEN ")
				}
				formatBound(sb, f.Start)
				sb.WriteString(" AND ")
				formatBound(sb, f.End)
			}
			sb.WriteString(")")
		}
	case *Collate:
		formatExpr(sb, x.X)
		sb.WriteString(" COLLATE " + x.Name)
	case *Subquery:
		sb.WriteString("(SELECT ...)")
	case *Exists:
		sb.WriteString("EXISTS (SELECT ...)")
	case *Case:
		sb.WriteString("CASE")
		if x.Operand != nil {
			sb.WriteString(" ")
			formatExpr(sb, x.Operand)
		}
		for _, w := range x.Whens {
			sb.WriteString(" WHEN ")
			formatExpr(sb, w.Cond)
			sb.WriteString(" THEN ")
			formatExpr(sb, w.Result)
		}
		if x.Else != nil {
			sb.WriteString(" ELSE ")
			formatExpr(sb, x.Else)
		}
		sb.WriteString(" END")
	default:
		fmt.Fprintf(sb, "%T", e)
	}
}

// ParseExpr parses a single expression.
func ParseExpr(src string) (Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errf("unexpected trailing input")
	}
	return e, nil
}

func formatBound(sb *strings.Builder, b FrameBound) {
	switch b.Kind {
	case UnboundedPreceding:
		sb.WriteString("UNBOUNDED PRECEDING")
	case UnboundedFollowing:
		sb.WriteString("UNBOUNDED FOLLOWING")
	case CurrentRow:
		sb.WriteString("CURRENT ROW")
	case Preceding:
		formatExpr(sb, b.N)
		sb.WriteString(" PRECEDING")
	default:
		formatExpr(sb, b.N)
		sb.WriteString(" FOLLOWING")
	}
}

// keywords that are quoted when used as identifiers in generated SQL.
var keywords = map[string]bool{
	"ADD": true, "ALL": true, "ALTER": true, "ANALYZE": true, "AUTOINCREMENT": true, "BEGIN": true,
	"CHECK": true, "COLUMN": true, "COMMIT": true, "CONFLICT": true, "CONSTRAINT": true, "CREATE": true,
	"CURRENT": true, "CURRENT_DATE": true, "CURRENT_TIME": true, "CURRENT_TIMESTAMP": true, "DEFAULT": true,
	"DELETE": true, "DO": true, "DROP": true, "EXPLAIN": true, "FALSE": true, "FOLLOWING": true, "FOREIGN": true,
	"FULL": true, "IF": true, "IGNORE": true, "INDEX": true, "INSERT": true, "KEY": true, "NOTHING": true,
	"OVER": true, "PARTITION": true, "PRECEDING": true, "PRIMARY": true, "RANGE": true, "REFERENCES": true,
	"RENAME": true, "REPLACE": true, "RIGHT": true, "ROLLBACK": true, "ROW": true, "ROWS": true, "TABLE": true,
	"TO": true, "TRANSACTION": true, "TRUE": true, "UNBOUNDED": true, "UNIQUE": true, "UPDATE": true,
	"USING": true, "VACUUM": true, "VIEW": true, "WITH": true, "CAST": true, "ESCAPE": true, "GLOB": true,
}

// QuoteIdent returns name as an SQL identifier, double-quoted when it is
// not a plain identifier or could be read as a keyword.
func QuoteIdent(name string) string {
	plain := name != ""
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			plain = false
			break
		}
	}
	if plain {
		u := strings.ToUpper(name)
		plain = !reserved[u] && !keywords[u]
	}
	if plain {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
