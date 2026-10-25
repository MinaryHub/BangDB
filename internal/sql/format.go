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
		sb.WriteString("?")
	case *ColumnRef:
		if x.Table != "" {
			sb.WriteString(x.Table + ".")
		}
		sb.WriteString(x.Name)
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
	p := &parser{toks: toks}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errf("unexpected trailing input")
	}
	return e, nil
}
