package sql

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Parse parses one or more ';'-separated statements and returns them with
// the number of '?' placeholders. Parameter indexes are numbered across the
// whole script.
func Parse(src string) ([]Stmt, int, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, 0, err
	}
	p := &parser{toks: toks}
	var stmts []Stmt
	for {
		for p.acceptOp(";") {
		}
		if p.peek().kind == tEOF {
			break
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, 0, err
		}
		stmts = append(stmts, s)
		if !p.acceptOp(";") && p.peek().kind != tEOF {
			return nil, 0, p.errf("expected ';' or end of input")
		}
	}
	return stmts, p.params, nil
}

type parser struct {
	toks   []token
	pos    int
	params int
}

// reserved words that cannot be used as bare aliases.
var reserved = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true,
	"LIMIT": true, "OFFSET": true, "JOIN": true, "LEFT": true, "INNER": true, "CROSS": true,
	"OUTER": true, "ON": true, "AND": true, "OR": true, "NOT": true, "AS": true, "SET": true,
	"VALUES": true, "UNION": true, "BY": true, "ASC": true, "DESC": true, "IS": true, "IN": true,
	"LIKE": true, "BETWEEN": true, "NULL": true, "CASE": true, "WHEN": true, "THEN": true,
	"ELSE": true, "END": true, "DISTINCT": true, "INTO": true,
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) peekAt(n int) token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) errf(format string, args ...any) error {
	t := p.peek()
	near := t.text
	if t.kind == tEOF {
		near = "end of input"
	}
	return &SyntaxError{t.pos, fmt.Sprintf(format, args...) + fmt.Sprintf(" (near %q)", near)}
}

func (p *parser) isKw(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) isKwAt(n int, kw string) bool {
	t := p.peekAt(n)
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) acceptKw(kws ...string) bool {
	for i, kw := range kws {
		if !p.isKwAt(i, kw) {
			return false
		}
	}
	p.pos += len(kws)
	return true
}

func (p *parser) expectKw(kws ...string) error {
	if !p.acceptKw(kws...) {
		return p.errf("expected %s", strings.Join(kws, " "))
	}
	return nil
}

func (p *parser) acceptOp(op string) bool {
	t := p.peek()
	if t.kind == tOp && t.text == op {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errf("expected %q", op)
	}
	return nil
}

func (p *parser) ident() (string, error) {
	t := p.peek()
	if t.kind == tQuotedIdent || t.kind == tIdent && !reserved[strings.ToUpper(t.text)] {
		p.pos++
		return t.text, nil
	}
	return "", p.errf("expected identifier")
}

func (p *parser) parseStmt() (Stmt, error) {
	switch {
	case p.acceptKw("EXPLAIN"):
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: s}, nil
	case p.isKw("SELECT"):
		return p.parseSelect()
	case p.acceptKw("INSERT"):
		return p.parseInsert()
	case p.acceptKw("UPDATE"):
		return p.parseUpdate()
	case p.acceptKw("DELETE"):
		return p.parseDelete()
	case p.acceptKw("CREATE"):
		return p.parseCreate()
	case p.acceptKw("DROP"):
		return p.parseDrop()
	case p.acceptKw("ALTER"):
		return p.parseAlter()
	case p.acceptKw("BEGIN"):
		p.acceptKw("TRANSACTION")
		return &Begin{}, nil
	case p.acceptKw("COMMIT"), p.acceptKw("END"):
		p.acceptKw("TRANSACTION")
		return &Commit{}, nil
	case p.acceptKw("ROLLBACK"):
		p.acceptKw("TRANSACTION")
		return &Rollback{}, nil
	}
	return nil, p.errf("unknown statement")
}

func (p *parser) parseCreate() (Stmt, error) {
	if p.acceptKw("TABLE") {
		ct := &CreateTable{}
		ct.IfNotExists = p.acceptKw("IF", "NOT", "EXISTS")
		var err error
		if ct.Name, err = p.ident(); err != nil {
			return nil, err
		}
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		for {
			if p.acceptKw("CONSTRAINT") {
				if _, err := p.ident(); err != nil {
					return nil, err
				}
			}
			if p.acceptKw("PRIMARY", "KEY") {
				if ct.PrimaryKey != nil {
					return nil, p.errf("more than one PRIMARY KEY constraint")
				}
				cols, err := p.parseColumnList()
				if err != nil {
					return nil, err
				}
				ct.PrimaryKey = cols
			} else if p.isKw("UNIQUE") && p.peekAt(1).kind == tOp && p.peekAt(1).text == "(" {
				p.next()
				cols, err := p.parseColumnList()
				if err != nil {
					return nil, err
				}
				ct.Uniques = append(ct.Uniques, cols)
			} else {
				cd, err := p.parseColumnDef()
				if err != nil {
					return nil, err
				}
				ct.Columns = append(ct.Columns, cd)
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return ct, nil
	}
	unique := p.acceptKw("UNIQUE")
	if p.acceptKw("INDEX") {
		ci := &CreateIndex{Unique: unique}
		ci.IfNotExists = p.acceptKw("IF", "NOT", "EXISTS")
		var err error
		if ci.Name, err = p.ident(); err != nil {
			return nil, err
		}
		if err := p.expectKw("ON"); err != nil {
			return nil, err
		}
		if ci.Table, err = p.ident(); err != nil {
			return nil, err
		}
		if ci.Columns, err = p.parseColumnList(); err != nil {
			return nil, err
		}
		return ci, nil
	}
	return nil, p.errf("expected TABLE or INDEX")
}

// parseColumnList parses "(a [ASC|DESC], b, ...)". Sort directions are
// accepted for compatibility and ignored.
func (p *parser) parseColumnList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var cols []string
	for {
		c, err := p.ident()
		if err != nil {
			return nil, err
		}
		cols = append(cols, c)
		if !p.acceptKw("ASC") {
			p.acceptKw("DESC")
		}
		if !p.acceptOp(",") {
			break
		}
	}
	return cols, p.expectOp(")")
}

func (p *parser) parseColumnDef() (ColumnDef, error) {
	var cd ColumnDef
	var err error
	if cd.Name, err = p.ident(); err != nil {
		return cd, err
	}
	// Type name: one or more identifiers, optionally followed by (n[,m]).
	var typ []string
	for p.peek().kind == tIdent && !isConstraintKw(p.peek().text) {
		typ = append(typ, p.next().text)
	}
	if len(typ) > 0 && p.acceptOp("(") {
		for !p.acceptOp(")") {
			if p.peek().kind == tEOF {
				return cd, p.errf("unterminated type")
			}
			p.next()
		}
	}
	cd.Type = strings.ToUpper(strings.Join(typ, " "))
	for {
		switch {
		case p.acceptKw("PRIMARY", "KEY"):
			cd.PrimaryKey = true
			p.acceptKw("ASC")
			p.acceptKw("DESC")
			if p.acceptKw("AUTOINCREMENT") || p.acceptKw("AUTO_INCREMENT") {
				cd.AutoIncrement = true
			}
		case p.acceptKw("AUTOINCREMENT"), p.acceptKw("AUTO_INCREMENT"):
			cd.AutoIncrement = true
		case p.acceptKw("NOT", "NULL"):
			cd.NotNull = true
		case p.acceptKw("NULL"):
		case p.acceptKw("UNIQUE"):
			cd.Unique = true
		case p.acceptKw("DEFAULT"):
			if p.acceptOp("(") {
				e, err := p.parseExpr()
				if err != nil {
					return cd, err
				}
				if err := p.expectOp(")"); err != nil {
					return cd, err
				}
				cd.Default = e
			} else {
				e, err := p.parseUnary()
				if err != nil {
					return cd, err
				}
				cd.Default = e
			}
		default:
			return cd, nil
		}
	}
}

func isConstraintKw(s string) bool {
	switch strings.ToUpper(s) {
	case "PRIMARY", "NOT", "NULL", "UNIQUE", "DEFAULT", "AUTOINCREMENT", "AUTO_INCREMENT", "CONSTRAINT", "CHECK", "REFERENCES":
		return true
	}
	return false
}

func (p *parser) parseDrop() (Stmt, error) {
	if p.acceptKw("TABLE") {
		d := &DropTable{IfExists: p.acceptKw("IF", "EXISTS")}
		var err error
		d.Name, err = p.ident()
		return d, err
	}
	if p.acceptKw("INDEX") {
		d := &DropIndex{IfExists: p.acceptKw("IF", "EXISTS")}
		var err error
		d.Name, err = p.ident()
		return d, err
	}
	return nil, p.errf("expected TABLE or INDEX")
}

func (p *parser) parseAlter() (Stmt, error) {
	if err := p.expectKw("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("ADD"); err != nil {
		return nil, err
	}
	p.acceptKw("COLUMN")
	cd, err := p.parseColumnDef()
	if err != nil {
		return nil, err
	}
	return &AlterTableAddColumn{Table: name, Column: cd}, nil
}

func (p *parser) parseInsert() (Stmt, error) {
	if err := p.expectKw("INTO"); err != nil {
		return nil, err
	}
	ins := &Insert{}
	var err error
	if ins.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.acceptOp("(") {
		for {
			c, err := p.ident()
			if err != nil {
				return nil, err
			}
			ins.Columns = append(ins.Columns, c)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKw("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		row, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		if !p.acceptOp(",") {
			break
		}
	}
	return ins, nil
}

func (p *parser) parseUpdate() (Stmt, error) {
	up := &Update{}
	var err error
	if up.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if err := p.expectKw("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		up.Sets = append(up.Sets, Assignment{col, e})
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("WHERE") {
		if up.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return up, nil
}

func (p *parser) parseDelete() (Stmt, error) {
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	d := &Delete{}
	var err error
	if d.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.acceptKw("WHERE") {
		if d.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (p *parser) parseTableRef() (TableRef, error) {
	var tr TableRef
	var err error
	if tr.Name, err = p.ident(); err != nil {
		return tr, err
	}
	if p.acceptKw("AS") {
		tr.Alias, err = p.ident()
	} else if t := p.peek(); t.kind == tQuotedIdent || t.kind == tIdent && !reserved[strings.ToUpper(t.text)] {
		tr.Alias, err = p.ident()
	}
	return tr, err
}

func (p *parser) parseSelect() (*Select, error) {
	if err := p.expectKw("SELECT"); err != nil {
		return nil, err
	}
	s := &Select{}
	if p.acceptKw("DISTINCT") {
		s.Distinct = true
	} else {
		p.acceptKw("ALL")
	}
	for {
		var item SelectItem
		switch {
		case p.acceptOp("*"):
			item.Star = true
		case (p.peek().kind == tIdent || p.peek().kind == tQuotedIdent) && p.peekAt(1).kind == tOp && p.peekAt(1).text == "." &&
			p.peekAt(2).kind == tOp && p.peekAt(2).text == "*":
			item.Star, item.StarTable = true, p.next().text
			p.pos += 2
		default:
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item.Expr = e
			if p.acceptKw("AS") {
				if item.Alias, err = p.ident(); err != nil {
					if p.peek().kind == tString {
						item.Alias, err = p.next().text, nil
					} else {
						return nil, err
					}
				}
			} else if t := p.peek(); t.kind == tQuotedIdent || t.kind == tIdent && !reserved[strings.ToUpper(t.text)] {
				item.Alias, _ = p.ident()
			}
		}
		s.Items = append(s.Items, item)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("FROM") {
		tr, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		s.From = &tr
		for {
			var j Join
			switch {
			case p.acceptOp(","), p.acceptKw("CROSS", "JOIN"):
			case p.acceptKw("JOIN"), p.acceptKw("INNER", "JOIN"):
			case p.acceptKw("LEFT", "JOIN"), p.acceptKw("LEFT", "OUTER", "JOIN"):
				j.Left = true
			default:
				goto doneJoins
			}
			if j.Table, err = p.parseTableRef(); err != nil {
				return nil, err
			}
			if p.acceptKw("ON") {
				if j.On, err = p.parseExpr(); err != nil {
					return nil, err
				}
			} else if j.Left {
				return nil, p.errf("LEFT JOIN requires ON")
			}
			s.Joins = append(s.Joins, j)
		}
	doneJoins:
	}
	var err error
	if p.acceptKw("WHERE") {
		if s.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("GROUP", "BY") {
		if s.GroupBy, err = p.parseExprList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("HAVING") {
		if s.Having, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("ORDER", "BY") {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			oi := OrderItem{Expr: e}
			if p.acceptKw("DESC") {
				oi.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			s.OrderBy = append(s.OrderBy, oi)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("LIMIT") {
		if s.Limit, err = p.parseExpr(); err != nil {
			return nil, err
		}
		if p.acceptKw("OFFSET") {
			if s.Offset, err = p.parseExpr(); err != nil {
				return nil, err
			}
		} else if p.acceptOp(",") { // LIMIT offset, count
			s.Offset = s.Limit
			if s.Limit, err = p.parseExpr(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (p *parser) parseExprList() ([]Expr, error) {
	var list []Expr
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if !p.acceptOp(",") {
			return list, nil
		}
	}
}

// Precedence (low -> high): OR, AND, NOT, comparison, + -, * / %, ||, unary.
func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("OR") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &Binary{"OR", l, r}
	}
	return l, nil
}

func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("AND") {
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &Binary{"AND", l, r}
	}
	return l, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.acceptKw("NOT") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &Unary{"NOT", x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Expr, error) {
	l, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind == tOp {
			switch t.text {
			case "=", "!=", "<", "<=", ">", ">=":
				p.next()
				r, err := p.parseAdditive()
				if err != nil {
					return nil, err
				}
				l = &Binary{t.text, l, r}
				continue
			}
		}
		if p.acceptKw("IS") {
			not := p.acceptKw("NOT")
			if err := p.expectKw("NULL"); err != nil {
				return nil, err
			}
			l = &IsNull{l, not}
			continue
		}
		if p.acceptKw("ISNULL") {
			l = &IsNull{l, false}
			continue
		}
		if p.acceptKw("NOTNULL") {
			l = &IsNull{l, true}
			continue
		}
		not := false
		if p.isKw("NOT") && (p.isKwAt(1, "IN") || p.isKwAt(1, "LIKE") || p.isKwAt(1, "BETWEEN")) {
			p.next()
			not = true
		}
		switch {
		case p.acceptKw("IN"):
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			list, err := p.parseExprList()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			l = &In{l, list, not}
		case p.acceptKw("LIKE"):
			r, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			l = &Like{l, r, not}
		case p.acceptKw("BETWEEN"):
			lo, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("AND"); err != nil {
				return nil, err
			}
			hi, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			l = &Between{l, lo, hi, not}
		default:
			return l, nil
		}
	}
}

func (p *parser) parseAdditive() (Expr, error) {
	l, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tOp || (t.text != "+" && t.text != "-") {
			return l, nil
		}
		p.next()
		r, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		l = &Binary{t.text, l, r}
	}
}

func (p *parser) parseMultiplicative() (Expr, error) {
	l, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tOp || (t.text != "*" && t.text != "/" && t.text != "%") {
			return l, nil
		}
		p.next()
		r, err := p.parseConcat()
		if err != nil {
			return nil, err
		}
		l = &Binary{t.text, l, r}
	}
}

func (p *parser) parseConcat() (Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("||") {
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &Binary{"||", l, r}
	}
	return l, nil
}

func (p *parser) parseUnary() (Expr, error) {
	if p.acceptOp("-") {
		parse := p.parseUnary
		if p.peek().kind == tNumber {
			parse = p.parsePrimary
		}
		x, err := parse()
		if err != nil {
			return nil, err
		}
		// Fold negative numeric literals so that -9223372036854775808 works.
		if lit, ok := x.(*Literal); ok {
			switch v := lit.Value.(type) {
			case int64:
				return &Literal{-v}, nil
			case float64:
				return &Literal{-v}, nil
			case uint64:
				if v == 1<<63 {
					return &Literal{int64(-1 << 63)}, nil
				}
			}
		}
		return &Unary{"-", x}, nil
	}
	if p.acceptOp("+") {
		return p.parseUnary()
	}
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if lit, ok := x.(*Literal); ok {
		if _, big := lit.Value.(uint64); big {
			return nil, p.errf("integer literal out of range")
		}
	}
	return x, nil
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.next()
		if !strings.ContainsAny(t.text, ".eE") {
			if i, err := strconv.ParseInt(t.text, 10, 64); err == nil {
				return &Literal{i}, nil
			}
			if u, err := strconv.ParseUint(t.text, 10, 64); err == nil && u == 1<<63 {
				return &Literal{u}, nil // only valid after unary minus
			}
		}
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, &SyntaxError{t.pos, "invalid number " + t.text}
		}
		return &Literal{f}, nil
	case tString:
		p.next()
		return &Literal{t.text}, nil
	case tBlob:
		p.next()
		b, _ := hex.DecodeString(t.text)
		return &Literal{b}, nil
	case tParam:
		p.next()
		p.params++
		return &Param{p.params - 1}, nil
	case tOp:
		if t.text == "(" {
			p.next()
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			return e, p.expectOp(")")
		}
	case tQuotedIdent:
		p.next()
		return p.columnRest(t.text)
	case tIdent:
		up := strings.ToUpper(t.text)
		switch up {
		case "NULL":
			p.next()
			return &Literal{nil}, nil
		case "TRUE":
			p.next()
			return &Literal{int64(1)}, nil
		case "FALSE":
			p.next()
			return &Literal{int64(0)}, nil
		case "CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME":
			p.next()
			return &Func{Name: up}, nil
		case "CASE":
			p.next()
			return p.parseCase()
		case "CAST":
			p.next()
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("AS"); err != nil {
				return nil, err
			}
			var typ []string
			for p.peek().kind == tIdent {
				typ = append(typ, p.next().text)
			}
			if p.acceptOp("(") {
				for !p.acceptOp(")") && p.peek().kind != tEOF {
					p.next()
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &Func{Name: "CAST", Args: []Expr{e, &Literal{strings.Join(typ, " ")}}}, nil
		}
		if reserved[up] {
			return nil, p.errf("unexpected keyword")
		}
		p.next()
		if p.acceptOp("(") {
			f := &Func{Name: up}
			if p.acceptOp("*") {
				f.Star = true
			} else if !p.acceptOp(")") {
				f.Distinct = p.acceptKw("DISTINCT")
				args, err := p.parseExprList()
				if err != nil {
					return nil, err
				}
				f.Args = args
			} else {
				return f, nil
			}
			return f, p.expectOp(")")
		}
		return p.columnRest(t.text)
	}
	return nil, p.errf("expected expression")
}

func (p *parser) columnRest(first string) (Expr, error) {
	if p.acceptOp(".") {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &ColumnRef{Table: first, Name: name}, nil
	}
	return &ColumnRef{Name: first}, nil
}

func (p *parser) parseCase() (Expr, error) {
	c := &Case{}
	var err error
	if !p.isKw("WHEN") {
		if c.Operand, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("WHEN") {
		var w When
		if w.Cond, err = p.parseExpr(); err != nil {
			return nil, err
		}
		if err := p.expectKw("THEN"); err != nil {
			return nil, err
		}
		if w.Result, err = p.parseExpr(); err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, w)
	}
	if len(c.Whens) == 0 {
		return nil, p.errf("CASE requires WHEN")
	}
	if p.acceptKw("ELSE") {
		if c.Else, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("END")
}
