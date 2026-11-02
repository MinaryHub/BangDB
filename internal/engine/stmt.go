package engine

import (
	"context"
	"time"

	"github.com/knoxxr/bangdb/internal/sql"
)

// Stmt is a prepared statement: the SQL is parsed once and, for a single
// SELECT, its query plan is reused (and re-planned automatically after a
// schema change). It is safe for concurrent use and runs outside any
// transaction, like DB.Exec / DB.QueryRows.
type Stmt struct {
	db  *DB
	src string
	pr  parsed
	sel *sql.Select // the statement, when it is a single SELECT
	// plans holds this statement's plan, so that it is not evicted by
	// other statements.
	plans planCache
}

// Prepare parses src for repeated execution.
func (db *DB) Prepare(src string) (*Stmt, error) {
	stmts, names, err := sql.ParseNamed(src)
	if err != nil {
		return nil, err
	}
	s := &Stmt{db: db, src: src, pr: parsed{stmts, names}}
	if len(stmts) == 1 {
		s.sel, _ = stmts[0].(*sql.Select)
	}
	return s, nil
}

// NumInput returns the number of parameters and whether any is named.
func (s *Stmt) NumInput() (int, bool) {
	for _, n := range s.pr.names {
		if n != "" {
			return len(s.pr.names), true
		}
	}
	return len(s.pr.names), false
}

// Exec runs the statement(s).
func (s *Stmt) Exec(ctx context.Context, args ...any) (Result, error) {
	c := s.db.Conn()
	start := time.Now()
	res, err := c.execParsed(ctx, s.pr, args)
	if c.tx != nil {
		c.tx.Rollback()
		if err == nil {
			err = errScriptTx
		}
	}
	s.db.finishCall(s.src, args, start, err)
	return res, err
}

// Query runs a query; a single SELECT is streamed (see Conn.QueryRows).
func (s *Stmt) Query(ctx context.Context, args ...any) (*Rows, error) { return s.query(ctx, args, 0) }

// QueryFirst is Query for at most one row (see Conn.QueryFirst).
func (s *Stmt) QueryFirst(ctx context.Context, args ...any) (*Rows, error) {
	return s.query(ctx, args, 1)
}

func (s *Stmt) query(ctx context.Context, args []any, maxRows int) (*Rows, error) {
	if s.sel == nil {
		res, err := s.Exec(ctx, args...)
		if err != nil {
			return nil, err
		}
		return RowsFromResult(res), nil
	}
	nargs, err := normalizeArgs(args, s.pr.names)
	if err != nil {
		return nil, err
	}
	return s.db.stream(ctx, s.sel, nargs, maxRows, s.src, args, &s.plans)
}

// ReadOnlySQL reports whether src parses and consists only of statements
// that do not change the database (SELECT, EXPLAIN).
func (db *DB) ReadOnlySQL(src string) (bool, error) {
	pr, err := db.parse(src)
	if err != nil {
		return false, err
	}
	for _, s := range pr.stmts {
		if !isReadOnly(s) {
			return false, nil
		}
	}
	return len(pr.stmts) > 0, nil
}

// Access is the privilege a statement needs.
type Access int

const (
	AccessRead  Access = iota // SELECT, EXPLAIN, PRAGMA reads
	AccessWrite               // INSERT, UPDATE, DELETE and transaction control
	AccessAdmin               // schema changes, VACUUM, ANALYZE, PRAGMA settings
)

// SQLAccess returns the privilege needed by every statement of src.
func (db *DB) SQLAccess(src string) (Access, error) {
	pr, err := db.parse(src)
	if err != nil {
		return AccessAdmin, err
	}
	need := AccessRead
	for _, s := range pr.stmts {
		need = max(need, stmtAccess(s))
	}
	return need, nil
}

func stmtAccess(s sql.Stmt) Access {
	switch st := s.(type) {
	case *sql.Select:
		return AccessRead
	case *sql.Explain:
		if st.Analyze {
			return stmtAccess(st.Stmt) // it runs the statement
		}
		return AccessRead
	case *sql.Pragma:
		if st.Value == nil || st.Call {
			return AccessRead
		}
		return AccessAdmin
	case *sql.Insert, *sql.Update, *sql.Delete, *sql.Begin, *sql.Commit, *sql.Rollback:
		return AccessWrite
	}
	return AccessAdmin
}
