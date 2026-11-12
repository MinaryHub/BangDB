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
	stmts, names, err := ParseNamed(src)
	return stmts, len(names), err
}

// ParseNamed is Parse that also returns, for every argument slot, the name
// of its placeholder ("" for positional ones).
func ParseNamed(src string) ([]Stmt, []string, error) {
	stmts, p, err := parseAll(src)
	if err != nil {
		return nil, nil, err
	}
	return stmts, p.names, nil
}

func parseAll(src string) ([]Stmt, *parser, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, nil, err
	}
	p := &parser{toks: toks, src: src}
	var stmts []Stmt
	for {
		for p.acceptOp(";") {
		}
		if p.peek().kind == tEOF {
			break
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, nil, err
		}
		stmts = append(stmts, s)
		if !p.acceptOp(";") && p.peek().kind != tEOF {
			return nil, nil, p.errf("expected ';' or end of input")
		}
	}
	return stmts, p, nil
}

type parser struct {
	src   string
	toks  []token
	pos   int
	names []string // per argument slot: placeholder name or ""
}

// param assigns the argument slot of a placeholder token.
func (p *parser) param(t token) (*Param, error) {
	text := t.text
	if text == "?" {
		p.names = append(p.names, "")
		return &Param{Index: len(p.names) - 1}, nil
	}
	body := text[1:]
	if n, err := strconv.Atoi(body); err == nil && (text[0] == '?' || text[0] == '$') {
		if n < 1 || n > 1000 {
			return nil, &SyntaxError{t.pos, "parameter number out of range: " + text}
		}
		for len(p.names) < n {
			p.names = append(p.names, "")
		}
		return &Param{Index: n - 1}, nil
	}
	if text[0] == '?' {
		return nil, &SyntaxError{t.pos, "invalid parameter " + text}
	}
	for i, name := range p.names {
		if strings.EqualFold(name, body) {
			return &Param{Index: i, Name: body}, nil
		}
	}
	p.names = append(p.names, body)
	return &Param{Index: len(p.names) - 1, Name: body}, nil
}

// reserved words that cannot be used as bare aliases.
var reserved = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true,
	"LIMIT": true, "OFFSET": true, "JOIN": true, "LEFT": true, "INNER": true, "CROSS": true,
	"OUTER": true, "ON": true, "AND": true, "OR": true, "NOT": true, "AS": true, "SET": true,
	"VALUES": true, "UNION": true, "BY": true, "ASC": true, "DESC": true, "IS": true, "IN": true,
	"LIKE": true, "BETWEEN": true, "NULL": true, "CASE": true, "WHEN": true, "THEN": true,
	"ELSE": true, "END": true, "DISTINCT": true, "INTO": true, "RETURNING": true,
	"INTERSECT": true, "EXCEPT": true, "EXISTS": true, "COLLATE": true,
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

// parseOptions parses "(name = value, ...)": values are identifiers,
// strings or numbers.
func (p *parser) parseOptions() (map[string]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	opts := map[string]string{}
	for {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		t := p.peek()
		switch t.kind {
		case tIdent, tQuotedIdent, tString, tNumber:
			p.pos++
		default:
			return nil, p.errf("expected an option value")
		}
		opts[strings.ToLower(name)] = t.text
		if !p.acceptOp(",") {
			break
		}
	}
	return opts, p.expectOp(")")
}

func (p *parser) parseStmt() (Stmt, error) {
	switch {
	case p.acceptKw("EXPLAIN"):
		analyze := p.acceptKw("ANALYZE")
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: s, Analyze: analyze}, nil
	case p.isKw("SELECT"):
		return p.parseSelect()
	case p.isKw("WITH"):
		return p.parseWithStmt()
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
	case p.acceptKw("VACUUM"):
		return &Vacuum{}, nil
	case p.acceptKw("PRAGMA"):
		t := p.next()
		if t.kind != tIdent && t.kind != tQuotedIdent {
			return nil, &SyntaxError{t.pos, "expected a pragma name"}
		}
		st := &Pragma{Name: strings.ToLower(t.text)}
		switch {
		case p.acceptOp("="):
			// A bare word (ON, OFF, WAL) is the value as text.
			if a := p.peek(); a.kind == tIdent && (p.peekAt(1).kind == tEOF || p.peekAt(1).text == ";") {
				p.next()
				st.Value = &Literal{Value: a.text}
				break
			}
			v, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			st.Value = v
		case p.acceptOp("("):
			// The argument may be a bare name (a table): read it as one.
			if a := p.peek(); (a.kind == tIdent || a.kind == tQuotedIdent) && p.peekAt(1).text == ")" {
				p.next()
				st.Value = &Literal{Value: a.text}
			} else {
				v, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				st.Value = v
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			st.Call = true
		}
		return st, nil
	case p.acceptKw("ANALYZE"):
		st := &Analyze{}
		if t := p.peek(); t.kind == tIdent || t.kind == tQuotedIdent {
			st.Table = p.next().text
		}
		return st, nil
	case p.acceptKw("ROLLBACK"):
		p.acceptKw("TRANSACTION")
		return &Rollback{}, nil
	}
	return nil, p.errf("unknown statement")
}

func (p *parser) parseCreate() (Stmt, error) {
	createPos := p.toks[p.pos-1].pos
	if p.acceptKw("TRIGGER") || p.acceptKw("TEMP", "TRIGGER") || p.acceptKw("TEMPORARY", "TRIGGER") {
		return p.parseTrigger(createPos)
	}
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
			} else if p.acceptKw("CHECK") {
				e, err := p.parseParenExpr()
				if err != nil {
					return nil, err
				}
				ct.Checks = append(ct.Checks, e)
			} else if p.acceptKw("FOREIGN", "KEY") {
				cols, err := p.parseColumnList()
				if err != nil {
					return nil, err
				}
				if err := p.expectKw("REFERENCES"); err != nil {
					return nil, err
				}
				fk, err := p.parseReferences()
				if err != nil {
					return nil, err
				}
				fk.Columns = cols
				ct.ForeignKeys = append(ct.ForeignKeys, *fk)
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
		if p.acceptKw("USING") {
			if ct.Using, err = p.ident(); err != nil {
				return nil, err
			}
			if p.acceptKw("WITH") {
				if ct.UsingOptions, err = p.parseOptions(); err != nil {
					return nil, err
				}
			}
		}
		return ct, nil
	}
	if p.acceptKw("VIEW") {
		cv := &CreateView{IfNotExists: p.acceptKw("IF", "NOT", "EXISTS")}
		var err error
		if cv.Name, err = p.ident(); err != nil {
			return nil, err
		}
		if p.peek().kind == tOp && p.peek().text == "(" {
			if cv.Columns, err = p.parseColumnList(); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("AS"); err != nil {
			return nil, err
		}
		start := p.peek().pos
		if cv.Select, err = p.parseSelect(); err != nil {
			return nil, err
		}
		cv.SQL = strings.TrimSpace(p.src[start:p.peek().pos])
		return cv, nil
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
		if p.acceptKw("WHERE") {
			if ci.Where, err = p.parseExpr(); err != nil {
				return nil, err
			}
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
	size := ""
	if len(typ) > 0 && p.acceptOp("(") {
		var parts []string
		for !p.acceptOp(")") {
			t := p.next()
			if t.kind == tEOF {
				return cd, p.errf("unterminated type")
			}
			if t.text != "," {
				parts = append(parts, t.text)
			}
		}
		size = "(" + strings.Join(parts, ",") + ")"
	}
	cd.Type = strings.ToUpper(strings.Join(typ, " ")) + size
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
		case p.acceptKw("COLLATE"):
			name, err := p.collationName()
			if err != nil {
				return cd, err
			}
			cd.Collate = name
		case p.acceptKw("CHECK"):
			e, err := p.parseParenExpr()
			if err != nil {
				return cd, err
			}
			cd.Check = e
		case p.acceptKw("REFERENCES"):
			fk, err := p.parseReferences()
			if err != nil {
				return cd, err
			}
			cd.References = fk
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
	case "PRIMARY", "NOT", "NULL", "UNIQUE", "DEFAULT", "AUTOINCREMENT", "AUTO_INCREMENT", "CONSTRAINT", "CHECK", "REFERENCES", "COLLATE":
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
	if p.acceptKw("VIEW") {
		d := &DropView{IfExists: p.acceptKw("IF", "EXISTS")}
		var err error
		d.Name, err = p.ident()
		return d, err
	}
	if p.acceptKw("TRIGGER") {
		d := &DropTrigger{IfExists: p.acceptKw("IF", "EXISTS")}
		var err error
		d.Name, err = p.ident()
		return d, err
	}
	return nil, p.errf("expected TABLE, INDEX, VIEW or TRIGGER")
}

// parseParenExpr parses "( expr )".
func (p *parser) parseParenExpr() (Expr, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return e, p.expectOp(")")
}

// parseReferences parses "table [(cols)] [ON DELETE action] [ON UPDATE
// action]" after REFERENCES.
func (p *parser) parseReferences() (*ForeignKey, error) {
	fk := &ForeignKey{}
	var err error
	if fk.RefTable, err = p.ident(); err != nil {
		return nil, err
	}
	if p.peek().kind == tOp && p.peek().text == "(" {
		if fk.RefColumns, err = p.parseColumnList(); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("ON") {
		var target *string
		switch {
		case p.acceptKw("DELETE"):
			target = &fk.OnDelete
		case p.acceptKw("UPDATE"):
			target = &fk.OnUpdate
		default:
			return nil, p.errf("expected DELETE or UPDATE")
		}
		switch {
		case p.acceptKw("CASCADE"):
			*target = "CASCADE"
		case p.acceptKw("RESTRICT"):
			*target = "RESTRICT"
		case p.acceptKw("NO", "ACTION"):
			*target = "NO ACTION"
		case p.acceptKw("SET", "NULL"):
			*target = "SET NULL"
		default:
			return nil, p.errf("expected CASCADE, RESTRICT, NO ACTION or SET NULL")
		}
	}
	return fk, nil
}

func (p *parser) parseAlter() (Stmt, error) {
	if err := p.expectKw("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("ADD"):
		p.acceptKw("COLUMN")
		cd, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		return &AlterTableAddColumn{Table: name, Column: cd}, nil
	case p.acceptKw("DROP"):
		p.acceptKw("COLUMN")
		col, err := p.ident()
		return &AlterTableDropColumn{Table: name, Column: col}, err
	case p.acceptKw("RENAME"):
		if p.acceptKw("TO") {
			n, err := p.ident()
			return &AlterTableRename{Table: name, NewName: n}, err
		}
		p.acceptKw("COLUMN")
		old, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("TO"); err != nil {
			return nil, err
		}
		n, err := p.ident()
		return &AlterTableRenameColumn{Table: name, Old: old, New: n}, err
	}
	return nil, p.errf("expected ADD, DROP or RENAME")
}

func (p *parser) parseInsert() (Stmt, error) {
	ins := &Insert{}
	if p.acceptKw("OR") {
		switch {
		case p.acceptKw("IGNORE"):
			ins.Or = "IGNORE"
		case p.acceptKw("REPLACE"):
			ins.Or = "REPLACE"
		default:
			return nil, p.errf("expected IGNORE or REPLACE")
		}
	}
	if err := p.expectKw("INTO"); err != nil {
		return nil, err
	}
	var err error
	if ins.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.peek().kind == tOp && p.peek().text == "(" {
		if ins.Columns, err = p.parseColumnList(); err != nil {
			return nil, err
		}
	}
	if p.isSelectStart() {
		if ins.Select, err = p.parseSelect(); err != nil {
			return nil, err
		}
	} else {
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
	}
	if p.acceptKw("ON", "CONFLICT") {
		if ins.Or != "" {
			return nil, p.errf("INSERT OR %s cannot be combined with ON CONFLICT", ins.Or)
		}
		oc := &OnConflict{}
		if p.peek().kind == tOp && p.peek().text == "(" {
			if oc.Target, err = p.parseColumnList(); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("DO"); err != nil {
			return nil, err
		}
		if p.acceptKw("NOTHING") {
			oc.DoNothing = true
		} else {
			if err := p.expectKw("UPDATE", "SET"); err != nil {
				return nil, err
			}
			if oc.Sets, err = p.parseAssignments(); err != nil {
				return nil, err
			}
			if p.acceptKw("WHERE") {
				if oc.Where, err = p.parseExpr(); err != nil {
					return nil, err
				}
			}
		}
		ins.OnConflict = oc
	}
	ins.Returning, err = p.parseReturning()
	return ins, err
}

// parseSelectItems parses a select list (also used by RETURNING).
func (p *parser) parseSelectItems() ([]SelectItem, error) {
	var items []SelectItem
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
		items = append(items, item)
		if !p.acceptOp(",") {
			return items, nil
		}
	}
}

// parseReturning parses an optional RETURNING clause.
func (p *parser) parseReturning() ([]SelectItem, error) {
	if !p.acceptKw("RETURNING") {
		return nil, nil
	}
	return p.parseSelectItems()
}

func (p *parser) parseAssignments() ([]Assignment, error) {
	var sets []Assignment
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
		sets = append(sets, Assignment{col, e})
		if !p.acceptOp(",") {
			return sets, nil
		}
	}
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
	if up.Sets, err = p.parseAssignments(); err != nil {
		return nil, err
	}
	if p.acceptKw("WHERE") {
		if up.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	up.Returning, err = p.parseReturning()
	return up, err
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
	d.Returning, err = p.parseReturning()
	return d, err
}

func (p *parser) parseTableRef() (TableRef, error) {
	var tr TableRef
	var err error
	if p.peek().kind == tOp && p.peek().text == "(" {
		p.next()
		if tr.Sub, err = p.parseSelect(); err != nil {
			return tr, err
		}
		if err := p.expectOp(")"); err != nil {
			return tr, err
		}
		p.acceptKw("AS")
		if tr.Alias, err = p.ident(); err != nil {
			return tr, p.errf("a subquery in FROM needs an alias")
		}
		tr.Name = tr.Alias
		return tr, nil
	}
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

// isSelectStart reports whether a query follows: SELECT, or WITH and the
// name of a common table expression.
func (p *parser) isSelectStart() bool {
	if p.isKw("SELECT") {
		return true
	}
	t := p.peekAt(1)
	return p.isKw("WITH") && (t.kind == tIdent || t.kind == tQuotedIdent)
}

// parseWith parses the rest of a WITH clause (after WITH):
// [RECURSIVE] name [(columns)] AS (select), ...
func (p *parser) parseWith() (*With, error) {
	w := &With{}
	// RECURSIVE is the keyword unless it is itself the table's name.
	if p.isKw("RECURSIVE") && !p.isKwAt(1, "AS") && p.peekAt(1).text != "(" {
		p.next()
		w.Recursive = true
	}
	for {
		var c CTE
		var err error
		if c.Name, err = p.ident(); err != nil {
			return nil, err
		}
		for _, o := range w.CTEs {
			if strings.EqualFold(o.Name, c.Name) {
				return nil, p.errf("duplicate WITH table name %s", c.Name)
			}
		}
		if p.peek().kind == tOp && p.peek().text == "(" {
			if c.Columns, err = p.parseColumnList(); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("AS"); err != nil {
			return nil, err
		}
		// Materialization hints are accepted and ignored.
		if !p.acceptKw("MATERIALIZED") {
			p.acceptKw("NOT", "MATERIALIZED")
		}
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if c.Select, err = p.parseSelect(); err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		w.CTEs = append(w.CTEs, c)
		if !p.acceptOp(",") {
			return w, nil
		}
	}
}

// parseWithStmt parses a statement that starts with a WITH clause: a
// SELECT, INSERT, UPDATE or DELETE.
func (p *parser) parseWithStmt() (Stmt, error) {
	p.next()
	w, err := p.parseWith()
	if err != nil {
		return nil, err
	}
	switch {
	case p.isKw("SELECT"):
		s, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		s.With = w
		return s, nil
	case p.acceptKw("INSERT"):
		st, err := p.parseInsert()
		if err != nil {
			return nil, err
		}
		st.(*Insert).With = w
		return st, nil
	case p.acceptKw("UPDATE"):
		st, err := p.parseUpdate()
		if err != nil {
			return nil, err
		}
		st.(*Update).With = w
		return st, nil
	case p.acceptKw("DELETE"):
		st, err := p.parseDelete()
		if err != nil {
			return nil, err
		}
		st.(*Delete).With = w
		return st, nil
	}
	return nil, p.errf("expected SELECT, INSERT, UPDATE or DELETE after WITH")
}

// parseSelect parses a full SELECT: an optional WITH clause, one or more
// cores combined with set operators, then ORDER BY / LIMIT / OFFSET.
func (p *parser) parseSelect() (*Select, error) {
	var with *With
	if p.isKw("WITH") {
		p.next()
		var err error
		if with, err = p.parseWith(); err != nil {
			return nil, err
		}
	}
	s, err := p.parseSelectCore()
	if err != nil {
		return nil, err
	}
	s.With = with
	for {
		var op string
		switch {
		case p.acceptKw("UNION", "ALL"):
			op = "UNION ALL"
		case p.acceptKw("UNION"):
			op = "UNION"
		case p.acceptKw("INTERSECT"):
			op = "INTERSECT"
		case p.acceptKw("EXCEPT"):
			op = "EXCEPT"
		}
		if op == "" {
			break
		}
		next, err := p.parseSelectCore()
		if err != nil {
			return nil, err
		}
		s.Compound = append(s.Compound, SetOp{op, next})
	}
	return s, p.parseOrderLimit(s)
}

// parseSelectCore parses SELECT ... [FROM ...] [WHERE ...] [GROUP BY ...]
// [HAVING ...].
func (p *parser) parseSelectCore() (*Select, error) {
	if err := p.expectKw("SELECT"); err != nil {
		return nil, err
	}
	s := &Select{}
	if p.acceptKw("DISTINCT") {
		s.Distinct = true
	} else {
		p.acceptKw("ALL")
	}
	items, err := p.parseSelectItems()
	if err != nil {
		return nil, err
	}
	s.Items = items
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
	return s, nil
}

func (p *parser) parseOrderLimit(s *Select) error {
	var err error
	if p.acceptKw("ORDER", "BY") {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return err
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
			return err
		}
		if p.acceptKw("OFFSET") {
			if s.Offset, err = p.parseExpr(); err != nil {
				return err
			}
		} else if p.acceptOp(",") { // LIMIT offset, count
			s.Offset = s.Limit
			if s.Limit, err = p.parseExpr(); err != nil {
				return err
			}
		}
	}
	return nil
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
			if p.isSelectStart() {
				sub, err := p.parseSelect()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				l = &In{X: l, Sub: sub, Not: not}
				continue
			}
			list, err := p.parseExprList()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			l = &In{X: l, List: list, Not: not}
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
	for {
		var op string
		switch {
		case p.acceptOp("||"):
			op = "||"
		case p.acceptOp("->"):
			op = "->"
		case p.acceptOp("->>"):
			op = "->>"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &Binary{op, l, r}
	}
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
	for p.acceptKw("COLLATE") {
		name, err := p.collationName()
		if err != nil {
			return nil, err
		}
		x = &Collate{X: x, Name: name}
	}
	return x, nil
}

// collationName parses a collation: NOCASE, RTRIM or BINARY.
func (p *parser) collationName() (string, error) {
	t := p.next()
	name := strings.ToUpper(t.text)
	switch name {
	case "NOCASE", "RTRIM", "BINARY":
		return name, nil
	}
	return "", &SyntaxError{t.pos, "unknown collation " + t.text + " (use NOCASE, RTRIM or BINARY)"}
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
		return p.param(t)
	case tOp:
		if t.text == "(" {
			p.next()
			if p.isSelectStart() {
				sub, err := p.parseSelect()
				if err != nil {
					return nil, err
				}
				return &Subquery{sub}, p.expectOp(")")
			}
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
		case "EXISTS":
			p.next()
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			sub, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			return &Exists{Select: sub}, p.expectOp(")")
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
			name := strings.Join(typ, " ")
			if p.acceptOp("(") { // type arguments: DECIMAL(10,2), VARCHAR(20)
				var args []string
				for !p.acceptOp(")") && p.peek().kind != tEOF {
					args = append(args, p.next().text)
				}
				name += "(" + strings.Join(args, "") + ")"
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &Func{Name: "CAST", Args: []Expr{e, &Literal{name}}}, nil
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
				return f, p.parseOver(f)
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return f, p.parseOver(f)
		}
		return p.columnRest(t.text)
	}
	return nil, p.errf("expected expression")
}

// parseOver parses an optional OVER (PARTITION BY ... ORDER BY ... frame)
// after a function call.
func (p *parser) parseOver(f *Func) error {
	if !p.acceptKw("OVER") {
		return nil
	}
	if err := p.expectOp("("); err != nil {
		return err
	}
	w := &Window{}
	var err error
	if p.acceptKw("PARTITION", "BY") {
		if w.PartitionBy, err = p.parseExprList(); err != nil {
			return err
		}
	}
	if p.acceptKw("ORDER", "BY") {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			oi := OrderItem{Expr: e}
			if p.acceptKw("DESC") {
				oi.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			w.OrderBy = append(w.OrderBy, oi)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if rows, rng := p.isKw("ROWS"), p.isKw("RANGE"); rows || rng {
		p.next()
		fr := &Frame{Rows: rows}
		if p.acceptKw("BETWEEN") {
			if fr.Start, err = p.parseFrameBound(); err != nil {
				return err
			}
			if err := p.expectKw("AND"); err != nil {
				return err
			}
			if fr.End, err = p.parseFrameBound(); err != nil {
				return err
			}
		} else {
			if fr.Start, err = p.parseFrameBound(); err != nil {
				return err
			}
			fr.End = FrameBound{Kind: CurrentRow}
		}
		if fr.Start.Kind == UnboundedFollowing || fr.End.Kind == UnboundedPreceding || fr.Start.Kind > fr.End.Kind {
			return p.errf("invalid window frame")
		}
		w.Frame = fr
	}
	f.Over = w
	return p.expectOp(")")
}

func (p *parser) parseFrameBound() (FrameBound, error) {
	switch {
	case p.acceptKw("UNBOUNDED", "PRECEDING"):
		return FrameBound{Kind: UnboundedPreceding}, nil
	case p.acceptKw("UNBOUNDED", "FOLLOWING"):
		return FrameBound{Kind: UnboundedFollowing}, nil
	case p.acceptKw("CURRENT", "ROW"):
		return FrameBound{Kind: CurrentRow}, nil
	}
	n, err := p.parseAdditive()
	if err != nil {
		return FrameBound{}, err
	}
	switch {
	case p.acceptKw("PRECEDING"):
		return FrameBound{Kind: Preceding, N: n}, nil
	case p.acceptKw("FOLLOWING"):
		return FrameBound{Kind: Following, N: n}, nil
	}
	return FrameBound{}, p.errf("expected PRECEDING or FOLLOWING")
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

// parseTrigger parses the rest of CREATE TRIGGER (start: offset of CREATE).
func (p *parser) parseTrigger(start int) (Stmt, error) {
	ct := &CreateTrigger{IfNotExists: p.acceptKw("IF", "NOT", "EXISTS"), Timing: "BEFORE"}
	var err error
	if ct.Name, err = p.ident(); err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("BEFORE"):
	case p.acceptKw("AFTER"):
		ct.Timing = "AFTER"
	case p.acceptKw("INSTEAD", "OF"):
		return nil, p.errf("INSTEAD OF triggers are not supported")
	}
	switch {
	case p.acceptKw("INSERT"):
		ct.Event = "INSERT"
	case p.acceptKw("DELETE"):
		ct.Event = "DELETE"
	case p.acceptKw("UPDATE"):
		ct.Event = "UPDATE"
		if p.acceptKw("OF") {
			for {
				c, err := p.ident()
				if err != nil {
					return nil, err
				}
				ct.Columns = append(ct.Columns, c)
				if !p.acceptOp(",") {
					break
				}
			}
		}
	default:
		return nil, p.errf("expected INSERT, UPDATE or DELETE")
	}
	if err := p.expectKw("ON"); err != nil {
		return nil, err
	}
	if ct.Table, err = p.ident(); err != nil {
		return nil, err
	}
	p.acceptKw("FOR", "EACH", "ROW")
	if p.acceptKw("WHEN") {
		if ct.When, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if err := p.expectKw("BEGIN"); err != nil {
		return nil, err
	}
	for {
		for p.acceptOp(";") {
		}
		if p.isKw("END") {
			end := p.next()
			ct.SQL = strings.TrimSpace(p.src[start : end.pos+len(end.text)])
			break
		}
		if p.peek().kind == tEOF {
			return nil, p.errf("expected END")
		}
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		switch st.(type) {
		case *Insert, *Update, *Delete, *Select:
		default:
			return nil, p.errf("a trigger body may only contain INSERT, UPDATE, DELETE and SELECT")
		}
		ct.Body = append(ct.Body, st)
		if !p.isKw("END") {
			if err := p.expectOp(";"); err != nil {
				return nil, err
			}
		}
	}
	if len(ct.Body) == 0 {
		return nil, p.errf("empty trigger body")
	}
	return ct, nil
}
