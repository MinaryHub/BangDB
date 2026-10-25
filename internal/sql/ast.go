// Package sql contains the SQL lexer, AST and recursive-descent parser.
package sql

// Stmt is any parsed statement.
type Stmt interface{ stmt() }

// Expr is any expression node.
type Expr interface{ expr() }

type ColumnDef struct {
	Name          string
	Type          string
	PrimaryKey    bool
	AutoIncrement bool
	NotNull       bool
	Unique        bool
	Default       Expr // nil = no default
}

type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  []string   // table constraint PRIMARY KEY (a, b, ...)
	Uniques     [][]string // table constraints UNIQUE (a, b, ...)
}

type DropTable struct {
	Name     string
	IfExists bool
}

type AlterTableAddColumn struct {
	Table  string
	Column ColumnDef
}

type CreateIndex struct {
	Name        string
	Table       string
	Columns     []string
	Unique      bool
	IfNotExists bool
}

type DropIndex struct {
	Name     string
	IfExists bool
}

type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr
}

type TableRef struct {
	Name  string
	Alias string
}

type Join struct {
	Left  bool // LEFT [OUTER] JOIN
	Table TableRef
	On    Expr // nil for CROSS JOIN / comma join
}

type SelectItem struct {
	Expr      Expr
	Alias     string
	Star      bool   // * or t.*
	StarTable string // t in t.*
}

type OrderItem struct {
	Expr Expr
	Desc bool
}

type Select struct {
	Distinct bool
	Items    []SelectItem
	From     *TableRef
	Joins    []Join
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
}

type Assignment struct {
	Column string
	Value  Expr
}

type Update struct {
	Table string
	Sets  []Assignment
	Where Expr
}

type Delete struct {
	Table string
	Where Expr
}

type Begin struct{}
type Commit struct{}
type Rollback struct{}

// Explain wraps a statement whose query plan should be shown.
type Explain struct{ Stmt Stmt }

func (*CreateTable) stmt()         {}
func (*DropTable) stmt()           {}
func (*AlterTableAddColumn) stmt() {}
func (*CreateIndex) stmt()         {}
func (*DropIndex) stmt()           {}
func (*Insert) stmt()              {}
func (*Select) stmt()              {}
func (*Update) stmt()              {}
func (*Delete) stmt()              {}
func (*Begin) stmt()               {}
func (*Commit) stmt()              {}
func (*Rollback) stmt()            {}
func (*Explain) stmt()             {}

// ---- Expressions ----

type Literal struct{ Value any }

// Param is a positional placeholder (?); Index is 0-based.
type Param struct{ Index int }

type ColumnRef struct {
	Table string // optional qualifier
	Name  string
}

type Unary struct {
	Op string // "-", "+", "NOT"
	X  Expr
}

type Binary struct {
	Op   string // + - * / % || = != < <= > >= AND OR
	L, R Expr
}

type IsNull struct {
	X   Expr
	Not bool
}

type In struct {
	X    Expr
	List []Expr
	Not  bool
}

type Between struct {
	X, Lo, Hi Expr
	Not       bool
}

type Like struct {
	X, Pattern Expr
	Not        bool
}

type Func struct {
	Name     string // upper-case
	Args     []Expr
	Star     bool // COUNT(*)
	Distinct bool
}

type Case struct {
	Operand Expr // optional: CASE x WHEN ...
	Whens   []When
	Else    Expr
}

type When struct{ Cond, Result Expr }

func (*Literal) expr()   {}
func (*Param) expr()     {}
func (*ColumnRef) expr() {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*IsNull) expr()    {}
func (*In) expr()        {}
func (*Between) expr()   {}
func (*Like) expr()      {}
func (*Func) expr()      {}
func (*Case) expr()      {}
