// Package bangdb is a lightweight embedded relational database written in
// pure Go with no dependencies.
//
//	db, err := bangdb.Open("app.db") // or ":memory:"
//	db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
//	db.Exec(`INSERT INTO users (name) VALUES (?)`, "kim")
//	rows, err := db.Query(`SELECT id, name FROM users WHERE name = ?`, "kim")
//	for rows.Next() {
//		var id int64
//		var name string
//		rows.Scan(&id, &name)
//	}
//
// A database/sql driver named "bangdb" is available in the driver package.
package bangdb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
)

// Options configures a database. See engine.Options.
type Options = engine.Options

// Result reports the outcome of a write statement.
type Result struct {
	RowsAffected int64
	LastInsertID int64
}

var (
	// ErrNoRows is returned by Row.Scan when the query returned no rows.
	ErrNoRows = errors.New("bangdb: no rows in result set")
	// ErrBusy is returned when another write transaction holds the lock
	// for longer than the busy timeout.
	ErrBusy = engine.ErrBusy
	// ErrConstraint is wrapped by UNIQUE / NOT NULL violations; test with
	// errors.Is.
	ErrConstraint = engine.ErrConstraint
	// ErrTxDone is returned when using a finished transaction.
	ErrTxDone = engine.ErrTxDone
)

// DB is a database handle, safe for concurrent use by multiple goroutines.
type DB struct{ e *engine.DB }

// Open opens or creates the database file at path. Use ":memory:" for a
// non-persistent in-memory database.
func Open(path string) (*DB, error) { return OpenWithOptions(path, nil) }

// OpenWithOptions is Open with explicit options.
func OpenWithOptions(path string, opts *Options) (*DB, error) {
	e, err := engine.Open(path, opts)
	if err != nil {
		return nil, err
	}
	return &DB{e}, nil
}

// Close closes the database.
func (db *DB) Close() error { return db.e.Close() }

// Exec executes one or more ';'-separated statements. Each statement outside
// an explicit transaction is atomic and durable when Exec returns.
func (db *DB) Exec(query string, args ...any) (Result, error) {
	r, err := db.e.Exec(query, args...)
	return Result{r.RowsAffected, r.LastInsertID}, err
}

// ExecContext is Exec with cancellation: when ctx is canceled or its
// deadline passes, the running statement stops, is rolled back, and the
// error matches context.Canceled / context.DeadlineExceeded.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (Result, error) {
	r, err := db.e.ExecContext(ctx, query, args...)
	return Result{r.RowsAffected, r.LastInsertID}, err
}

// Query runs a query and returns its (fully materialized) rows.
func (db *DB) Query(query string, args ...any) (*Rows, error) {
	return db.QueryContext(context.Background(), query, args...)
}

// QueryContext is Query with cancellation (see ExecContext).
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	r, err := db.e.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return newRows(r), nil
}

// QueryRow runs a query expected to return at most one row.
func (db *DB) QueryRow(query string, args ...any) *Row {
	return db.QueryRowContext(context.Background(), query, args...)
}

// QueryRowContext is QueryRow with cancellation.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	rows, err := db.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// Begin starts a write transaction. Only one write transaction can be active
// at a time; others wait up to the busy timeout. Readers are not blocked.
func (db *DB) Begin() (*Tx, error) { return db.BeginContext(context.Background()) }

// BeginContext is Begin; ctx bounds the wait for the write lock.
func (db *DB) BeginContext(ctx context.Context) (*Tx, error) {
	tx, err := db.e.BeginContext(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{tx}, nil
}

// Update runs fn inside a transaction, committing if it returns nil and
// rolling back otherwise.
func (db *DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Backup writes a consistent copy of the database to path while it stays
// online: readers are never blocked and commits pause only briefly. The
// backup is a regular database file; restore it by opening (or copying) it.
func (db *DB) Backup(path string) error { return db.e.Backup(context.Background(), path) }

// BackupContext is Backup with cancellation.
func (db *DB) BackupContext(ctx context.Context, path string) error { return db.e.Backup(ctx, path) }

// Seq returns the sequence number of the last committed transaction. It
// increases by one per commit and is shared with replicas.
func (db *DB) Seq() uint64 { return db.e.Seq() }

// ErrReadOnly is returned for writes on a replica.
var ErrReadOnly = engine.ErrReadOnly

// Tables returns the names of all tables.
func (db *DB) Tables() []string { return db.e.Catalog().TableNames() }

// Engine exposes the underlying engine (used by the driver and CLI).
func (db *DB) Engine() *engine.DB { return db.e }

// Tx is a write transaction. It is not safe for concurrent use.
type Tx struct{ t *engine.Tx }

// Exec executes statements in the transaction. A failed statement is undone
// but the transaction stays open.
func (tx *Tx) Exec(query string, args ...any) (Result, error) {
	r, err := tx.t.Exec(query, args...)
	return Result{r.RowsAffected, r.LastInsertID}, err
}

// ExecContext is Exec with cancellation of the running statement.
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (Result, error) {
	r, err := tx.t.ExecContext(ctx, query, args...)
	return Result{r.RowsAffected, r.LastInsertID}, err
}

// Query runs a query inside the transaction (it sees uncommitted changes).
func (tx *Tx) Query(query string, args ...any) (*Rows, error) {
	return tx.QueryContext(context.Background(), query, args...)
}

// QueryContext is Query with cancellation.
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	r, err := tx.t.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return newRows(r), nil
}

// QueryRow runs a query expected to return at most one row.
func (tx *Tx) QueryRow(query string, args ...any) *Row {
	rows, err := tx.Query(query, args...)
	return &Row{rows: rows, err: err}
}

// Commit makes the changes durable.
func (tx *Tx) Commit() error { return tx.t.Commit() }

// Rollback discards the changes.
func (tx *Tx) Rollback() error { return tx.t.Rollback() }

// Rows is a result set.
type Rows struct {
	cols []string
	data [][]any
	pos  int
}

func newRows(r engine.Result) *Rows { return &Rows{cols: r.Columns, data: r.Rows, pos: -1} }

// Columns returns the column names.
func (r *Rows) Columns() []string { return r.cols }

// Len returns the number of rows.
func (r *Rows) Len() int { return len(r.data) }

// Next advances to the next row.
func (r *Rows) Next() bool {
	if r.pos+1 >= len(r.data) {
		r.pos = len(r.data)
		return false
	}
	r.pos++
	return true
}

// Values returns the current row. Values are nil, int64, float64, string or
// []byte.
func (r *Rows) Values() []any { return r.data[r.pos] }

// All returns every row.
func (r *Rows) All() [][]any { return r.data }

// Close is a no-op kept for symmetry with database/sql.
func (r *Rows) Close() error { return nil }

// Err is always nil; errors are reported by Query. Kept for symmetry with
// database/sql.
func (r *Rows) Err() error { return nil }

// Scan copies the current row into dest. Supported destinations: *any,
// *int, *int64, *int32, *float64, *string, *[]byte, *bool, *time.Time.
func (r *Rows) Scan(dest ...any) error {
	if r.pos < 0 || r.pos >= len(r.data) {
		return errors.New("bangdb: Scan called without a current row")
	}
	row := r.data[r.pos]
	if len(dest) != len(row) {
		return fmt.Errorf("bangdb: expected %d destination arguments, got %d", len(row), len(dest))
	}
	for i, d := range dest {
		if err := convertAssign(d, row[i]); err != nil {
			return fmt.Errorf("bangdb: column %s: %w", r.cols[i], err)
		}
	}
	return nil
}

// Row is the result of QueryRow.
type Row struct {
	rows *Rows
	err  error
}

// Scan scans the first row, or returns ErrNoRows.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if !r.rows.Next() {
		return ErrNoRows
	}
	return r.rows.Scan(dest...)
}

func convertAssign(dest, v any) error {
	switch d := dest.(type) {
	case *any:
		*d = v
		return nil
	case *string:
		if v == nil {
			return errors.New("cannot scan NULL into *string")
		}
		if b, ok := v.([]byte); ok {
			*d = string(b)
		} else {
			*d = value.Format(v)
		}
		return nil
	case *[]byte:
		switch x := v.(type) {
		case nil:
			*d = nil
		case []byte:
			*d = append([]byte(nil), x...)
		default:
			*d = []byte(value.Format(x))
		}
		return nil
	case *int64, *int, *int32:
		var n int64
		switch x := v.(type) {
		case int64:
			n = x
		case float64:
			n = int64(x)
		case string:
			var err error
			if n, err = strconv.ParseInt(x, 10, 64); err != nil {
				return fmt.Errorf("cannot convert %q to integer", x)
			}
		default:
			return fmt.Errorf("cannot scan %s into integer", value.TypeOf(v))
		}
		switch d := dest.(type) {
		case *int64:
			*d = n
		case *int:
			*d = int(n)
		case *int32:
			*d = int32(n)
		}
		return nil
	case *float64:
		switch x := v.(type) {
		case int64:
			*d = float64(x)
		case float64:
			*d = x
		case string:
			f, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return fmt.Errorf("cannot convert %q to float", x)
			}
			*d = f
		default:
			return fmt.Errorf("cannot scan %s into *float64", value.TypeOf(v))
		}
		return nil
	case *bool:
		b, ok := value.Truthy(v)
		if !ok {
			return errors.New("cannot scan NULL into *bool")
		}
		*d = b
		return nil
	case *time.Time:
		switch x := v.(type) {
		case string:
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"} {
				if t, err := time.Parse(layout, x); err == nil {
					*d = t
					return nil
				}
			}
			return fmt.Errorf("cannot parse %q as time", x)
		case int64:
			*d = time.Unix(0, x).UTC()
			return nil
		}
		return fmt.Errorf("cannot scan %s into *time.Time", value.TypeOf(v))
	}
	return fmt.Errorf("unsupported Scan destination %T", dest)
}
