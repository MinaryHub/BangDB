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
	// Generated is the expression of a generated column:
	// [GENERATED ALWAYS] AS (expr) [STORED | VIRTUAL].
	Generated Expr
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
	// UsingOptions are the kind's options: USING kind WITH (name = value).
	UsingOptions map[string]string
}

type CreateView struct {
	Name        string
	IfNotExists bool
	Columns     []string // optional column names
	Select      *Select
	SQL         string // the SELECT's original text
}

// CreateTrigger is CREATE TRIGGER name {BEFORE|AFTER} {INSERT|DELETE|
// UPDATE [OF cols]} ON table [FOR EACH ROW] [WHEN expr] BEGIN stmts END.
type CreateTrigger struct {
	Name        string
	IfNotExists bool
	Timing      string   // BEFORE or AFTER
	Event       string   // INSERT, UPDATE or DELETE
	Columns     []string // UPDATE OF columns
	Table       string
	When        Expr
	Body        []Stmt // INSERT, UPDATE, DELETE or SELECT statements
	SQL         string // the whole statement's text
}

type DropTrigger struct {
	Name     string
	IfExists bool
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
	Name    string
	Table   string
	Columns []string
	// Exprs is nil, or has an entry per key part: the expression of a part
	// that is not a plain column (whose Columns entry is then ""), else nil.
	Exprs       []Expr
	Unique      bool
	IfNotExists bool
	Where       Expr // partial index condition, or nil
}

type DropIndex struct {
	Name     string
	IfExists bool
}

// With is a WITH clause: common table expressions visible to the statement
// it precedes.
type With struct {
	Recursive bool // WITH RECURSIVE: a CTE may refer to itself
	CTEs      []CTE
}

// CTE is one "name [(columns)] AS (select)" of a WITH clause.
type CTE struct {
	Name    string
	Columns []string // optional column names
	Select  *Select
}

type Insert struct {
	With       *With
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
	// Base marks a name that must be a stored table (the target of UPDATE
	// or DELETE), never a common table expression.
	Base bool
	// Func marks a table-valued function call, FROM name(args): its rows
	// are computed from Args, which may use the tables to its left.
	Func bool
	Args []Expr
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
	With     *With // WITH clause in front of the SELECT
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
	With      *With
	Table     string
	Sets      []Assignment
	Where     Expr
	Returning []SelectItem
}

type Delete struct {
	With      *With
	Table     string
	Where     Expr
	Returning []SelectItem
}

type Begin struct{}

// Vacuum rebuilds the database file compactly.
type Vacuum struct{}

// Pragma reads (Value == nil) or sets a database setting:
// PRAGMA name, PRAGMA name = value, PRAGMA name(value).
type Pragma struct {
	Name  string // lower case
	Value Expr
	// Call is set for the function form, whose argument is an operand
	// (PRAGMA table_info(users)), not a new value.
	Call bool
}

// Analyze collects index statistics for the query planner (all tables,
// or one).
type Analyze struct{ Table string }
type Commit struct{}
type Rollback struct{}

// Explain wraps a statement whose query plan should be shown.
type Explain struct {
	Stmt    Stmt
	Analyze bool // EXPLAIN ANALYZE: run the statement and report actual row counts
}

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
func (*Analyze) stmt()                {}
func (*Pragma) stmt()                 {}
func (*Commit) stmt()                 {}
func (*Rollback) stmt()               {}
func (*Explain) stmt()                {}
func (*CreateTrigger) stmt()          {}
func (*DropTrigger) stmt()            {}

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
	Over     *Window // window function call: f(...) OVER (...)
}

// Window is the OVER clause of a window function.
type Window struct {
	PartitionBy []Expr
	OrderBy     []OrderItem
	Frame       *Frame // nil: the default frame
}

// Frame is ROWS|RANGE BETWEEN Start AND End.
type Frame struct {
	Rows       bool
	Start, End FrameBound
}

// Frame bound kinds.
const (
	UnboundedPreceding = iota
	Preceding
	CurrentRow
	Following
	UnboundedFollowing
)

// FrameBound is one end of a frame; N is the offset of Preceding/Following.
type FrameBound struct {
	Kind int
	N    Expr
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
