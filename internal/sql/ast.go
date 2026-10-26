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
	Default       Expr   // nil = no default
	Collate       string // NOCASE, RTRIM, BINARY or ""
	Check         Expr   // column CHECK constraint
	References    *ForeignKey
}

// ForeignKey is FOREIGN KEY (cols) REFERENCES table [(cols)] [actions], or
// the column form "REFERENCES table [(col)]".
type ForeignKey struct {
	Columns    []string // empty for the column form
	RefTable   string
	RefColumns []string // empty: the referenced table's primary key
	OnDelete   string   // NO ACTION (default), RESTRICT, CASCADE, SET NULL
	OnUpdate   string
}

type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  []string   // table constraint PRIMARY KEY (a, b, ...)
	Uniques     [][]string // table constraints UNIQUE (a, b, ...)
	Checks      []Expr     // table constraints CHECK (...)
	ForeignKeys []ForeignKey
	Using       string // storage kind (CREATE TABLE ... USING kind); "" = default
}

type CreateView struct {
	Name        string
	IfNotExists bool
	Columns     []string // optional column names
	Select      *Select
	SQL         string // the SELECT's original text
}

type DropView struct {
	Name     string
	IfExists bool
}

type DropTable struct {
	Name     string
	IfExists bool
}

type AlterTableAddColumn struct {
	Table  string
	Column ColumnDef
}

type AlterTableRename struct {
	Table   string
	NewName string
}

type AlterTableRenameColumn struct {
	Table    string
	Old, New string
}

type AlterTableDropColumn struct {
	Table  string
	Column string
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
	Table      string
	Columns    []string
	Rows       [][]Expr // VALUES rows, or
	Select     *Select  // INSERT ... SELECT
	Or         string   // "", "IGNORE" or "REPLACE" (INSERT OR ...)
	OnConflict *OnConflict
	Returning  []SelectItem
}

// OnConflict is the UPSERT clause:
// ON CONFLICT [(cols)] DO NOTHING | DO UPDATE SET ... [WHERE ...].
type OnConflict struct {
	Target    []string // empty: any uniqueness conflict
	DoNothing bool
	Sets      []Assignment
	Where     Expr
}

type TableRef struct {
	Name  string
	Alias string
	Sub   *Select // derived table: FROM (SELECT ...) AS alias
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
	// Compound holds further SELECTs combined with UNION [ALL] / INTERSECT /
	// EXCEPT. When present, OrderBy/Limit/Offset apply to the combined result.
	Compound []SetOp
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
}

// SetOp is one "UNION [ALL] | INTERSECT | EXCEPT <select>" step.
type SetOp struct {
	Op     string // "UNION", "UNION ALL", "INTERSECT", "EXCEPT"
	Select *Select
}

type Assignment struct {
	Column string
	Value  Expr
}

type Update struct {
	Table     string
	Sets      []Assignment
	Where     Expr
	Returning []SelectItem
}

type Delete struct {
	Table     string
	Where     Expr
	Returning []SelectItem
}

type Begin struct{}

// Vacuum rebuilds the database file compactly.
type Vacuum struct{}
type Commit struct{}
type Rollback struct{}

// Explain wraps a statement whose query plan should be shown.
type Explain struct{ Stmt Stmt }

func (*CreateTable) stmt()            {}
func (*DropTable) stmt()              {}
func (*AlterTableAddColumn) stmt()    {}
func (*AlterTableRename) stmt()       {}
func (*AlterTableRenameColumn) stmt() {}
func (*AlterTableDropColumn) stmt()   {}
func (*CreateIndex) stmt()            {}
func (*DropIndex) stmt()              {}
func (*CreateView) stmt()             {}
func (*DropView) stmt()               {}
func (*Insert) stmt()                 {}
func (*Select) stmt()                 {}
func (*Update) stmt()                 {}
func (*Delete) stmt()                 {}
func (*Begin) stmt()                  {}
func (*Vacuum) stmt()                 {}
func (*Commit) stmt()                 {}
func (*Rollback) stmt()               {}
func (*Explain) stmt()                {}

// ---- Expressions ----

type Literal struct{ Value any }

// Param is a placeholder; Index is the 0-based argument slot. Named
// placeholders (:name, @name, $name) share a slot per name.
type Param struct {
	Index int
	Name  string
}

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
	Sub  *Select // x IN (SELECT ...)
	Not  bool
}

// Collate is "x COLLATE name".
type Collate struct {
	X    Expr
	Name string
}

// Subquery is a scalar subquery: (SELECT ...).
type Subquery struct{ Select *Select }

// Exists is [NOT] EXISTS (SELECT ...).
type Exists struct {
	Select *Select
	Not    bool
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
func (*Subquery) expr()  {}
func (*Collate) expr()   {}
func (*Exists) expr()    {}
