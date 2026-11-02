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
	"io"
	"strconv"
	"time"

	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/value"
)

// Options configures a database. See engine.Options.
type Options = engine.Options

// Stats describes database activity; see DB.Stats.
type Stats = engine.Stats

// SlowQuery is passed to Options.OnSlowQuery.
type SlowQuery = engine.SlowQuery

// NamedArg binds a value to a named placeholder; see Named.
type NamedArg = engine.NamedArg

// Named binds value to the placeholder :name, @name or $name:
//
//	db.Exec(`UPDATE t SET v = :v WHERE id = :id`, bangdb.Named("id", 1), bangdb.Named("v", "x"))
//
// database/sql's sql.Named values are accepted as well.
func Named(name string, value any) NamedArg { return NamedArg{Name: name, Value: value} }

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
//
// A single SELECT is streamed: rows are produced while you iterate, from a
// consistent snapshot taken when the query starts. Always Close the Rows (or
// iterate to the end); an open Rows keeps its snapshot pinned.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	r, err := db.e.QueryRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &Rows{r: r}, nil
}

// QueryRow runs a query expected to return at most one row.
func (db *DB) QueryRow(query string, args ...any) *Row {
	return db.QueryRowContext(context.Background(), query, args...)
}

// QueryRowContext is QueryRow with cancellation.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	r, err := db.e.QueryFirst(ctx, query, args...)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{rows: Rows{r: r}}
}

// Decimal is an exact decimal value, returned for DECIMAL(p,s) /
// NUMERIC(p,s) columns. Scan it into a *Decimal, *string (exact text) or
// *float64; pass a Decimal or a string such as "12.50" as an argument.
type Decimal = value.Decimal

// ParseDecimal parses a decimal number such as "12.50".
func ParseDecimal(s string) (Decimal, error) { return value.ParseDecimal(s) }

// Stmt is a prepared statement (see Prepare). It is safe for concurrent
// use; each call runs outside any transaction, like DB.Exec and DB.Query.
type Stmt struct{ s *engine.Stmt }

// Prepare parses query once for repeated execution. A single SELECT also
// keeps its query plan, re-planned automatically after schema changes.
func (db *DB) Prepare(query string) (*Stmt, error) {
	s, err := db.e.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &Stmt{s}, nil
}

// Exec runs the statement.
func (s *Stmt) Exec(args ...any) (Result, error) { return s.ExecContext(context.Background(), args...) }

// ExecContext is Exec with cancellation.
func (s *Stmt) ExecContext(ctx context.Context, args ...any) (Result, error) {
	r, err := s.s.Exec(ctx, args...)
	return Result{RowsAffected: r.RowsAffected, LastInsertID: r.LastInsertID}, err
}

// Query runs the statement as a query.
func (s *Stmt) Query(args ...any) (*Rows, error) {
	return s.QueryContext(context.Background(), args...)
}

// QueryContext is Query with cancellation.
func (s *Stmt) QueryContext(ctx context.Context, args ...any) (*Rows, error) {
	r, err := s.s.Query(ctx, args...)
	if err != nil {
		return nil, err
	}
	return &Rows{r: r}, nil
}

// QueryRow runs a query expected to return at most one row.
func (s *Stmt) QueryRow(args ...any) *Row { return s.QueryRowContext(context.Background(), args...) }

// QueryRowContext is QueryRow with cancellation.
func (s *Stmt) QueryRowContext(ctx context.Context, args ...any) *Row {
	r, err := s.s.QueryFirst(ctx, args...)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{rows: Rows{r: r}}
}

// Close releases the statement (it holds no resources; provided for
// symmetry with database/sql).
func (s *Stmt) Close() error { return nil }

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

// Vacuum rebuilds the database file compactly, returning the space of
// deleted data to the file system (same as the VACUUM statement). Writers
// wait while it runs; replicas re-synchronize from a snapshot afterwards.
func (db *DB) Vacuum(ctx context.Context) error { return db.e.Vacuum(ctx) }

// IntegrityCheck verifies checksums, B+tree structure, page accounting and
// index/table consistency on a snapshot, without blocking other work. It
// returns the problems found; an empty slice means the database is sound.
func (db *DB) IntegrityCheck() ([]string, error) { return db.e.IntegrityCheck(context.Background()) }

// ImportResult describes an import; see ImportSQL.
type ImportResult = engine.ImportResult

// CSVOptions configures ImportCSV and ExportCSV.
type CSVOptions = engine.CSVOptions

// Dump writes the database as an SQL script (schema, rows, indexes and
// views) from a consistent snapshot, without blocking writers. Load it with
// ImportSQL.
func (db *DB) Dump(ctx context.Context, w io.Writer) error { return db.e.Dump(ctx, w) }

// ImportSQL runs an SQL script, such as the output of Dump or of SQLite's
// .dump, in a single transaction. See engine.DB.ImportSQL for the SQLite
// statements it accepts or skips.
func (db *DB) ImportSQL(ctx context.Context, r io.Reader) (ImportResult, error) {
	return db.e.ImportSQL(ctx, r)
}

// ImportCSV inserts CSV records into table in one transaction (columns
// matched by the header row) and returns the number of rows inserted.
func (db *DB) ImportCSV(ctx context.Context, table string, r io.Reader, opts *CSVOptions) (int64, error) {
	return db.e.ImportCSV(ctx, table, r, opts)
}

// ExportCSV writes the result of a query as CSV and returns the number of
// rows written.
func (db *DB) ExportCSV(ctx context.Context, w io.Writer, opts *CSVOptions, query string, args ...any) (int64, error) {
	return db.e.ExportCSV(ctx, w, opts, query, args...)
}

// Stats returns counters and gauges of the database: statements, errors,
// slow calls, commits and their total time, write lock waits, WAL size,
// checkpoints, cache hits and misses, and open snapshots and cursors.
func (db *DB) Stats() Stats { return db.e.Stats() }

// Seq returns the sequence number of the last committed transaction. It
// increases by one per commit and is shared with replicas.
func (db *DB) Seq() uint64 { return db.e.Seq() }

// ErrReadOnly is returned for writes on a replica.
var ErrReadOnly = engine.ErrReadOnly

// Errors of encrypted databases (Options.EncryptionKey).
var (
	ErrEncrypted    = pager.ErrEncrypted    // the file is encrypted and no key was given
	ErrWrongKey     = pager.ErrWrongKey     // the key does not match the file
	ErrNotEncrypted = pager.ErrNotEncrypted // a key was given for a plaintext file
)

// ErrLocked is returned by Open when another process (or another open
// handle) is using the database file.
var ErrLocked = pager.ErrLocked

// BackupWithKey writes a backup like Backup, encrypted with key (nil: a
// plaintext copy). Use it to encrypt, decrypt or re-key a database.
func (db *DB) BackupWithKey(ctx context.Context, path string, key []byte) error {
	return db.e.BackupWithKey(ctx, path, key)
}

// Tables returns the names of all tables.
func (db *DB) Tables() []string { return db.e.Catalog().TableNames() }

// Views returns the names of all views.
func (db *DB) Views() []string { return db.e.Catalog().ViewNames() }

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

// QueryContext is Query with cancellation. Results inside a transaction are
// materialized.
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	r, err := tx.t.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &Rows{r: engine.RowsFromResult(r)}, nil
}

// QueryRow runs a query expected to return at most one row.
func (tx *Tx) QueryRow(query string, args ...any) *Row {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{rows: *rows}
}

// Commit makes the changes durable.
func (tx *Tx) Commit() error { return tx.t.Commit() }

// Rollback discards the changes.
func (tx *Tx) Rollback() error { return tx.t.Rollback() }

// Rows is a result set. Use Next to iterate and Close when done early.
type Rows struct{ r *engine.Rows }

// Columns returns the column names.
func (r *Rows) Columns() []string { return r.r.Columns() }

// Next advances to the next row. It returns false at the end of the result
// or on error (check Err); the Rows is then closed automatically.
func (r *Rows) Next() bool { return r.r.Next() }

// Values returns the current row. Values are nil, int64, float64, string or
// []byte.
func (r *Rows) Values() []any { return r.r.Row() }

// All reads every remaining row into memory and closes the Rows.
func (r *Rows) All() ([][]any, error) {
	var out [][]any
	for r.r.Next() {
		out = append(out, r.r.Row())
	}
	return out, r.r.Err()
}

// Close stops the query and releases its snapshot.
func (r *Rows) Close() error { return r.r.Close() }

// Err returns the error that ended iteration (for example a canceled
// context).
func (r *Rows) Err() error { return r.r.Err() }

// Scan copies the current row into dest. Supported destinations: *any,
// *int, *int64, *int32, *float64, *string, *[]byte, *bool, *time.Time.
func (r *Rows) Scan(dest ...any) error {
	row := r.r.Row()
	if row == nil {
		return errors.New("bangdb: Scan called without a current row")
	}
	if len(dest) != len(row) {
		return fmt.Errorf("bangdb: expected %d destination arguments, got %d", len(row), len(dest))
	}
	for i, d := range dest {
		if err := convertAssign(d, row[i]); err != nil {
			return fmt.Errorf("bangdb: column %s: %w", r.r.Columns()[i], err)
		}
	}
	return nil
}

// ScanValues converts row values (as returned by Rows.Values) into dest
// like Scan does; it serves other result types, such as the client
// package's.
func ScanValues(row []any, dest ...any) error {
	if len(dest) != len(row) {
		return fmt.Errorf("bangdb: expected %d destination arguments, got %d", len(row), len(dest))
	}
	for i, d := range dest {
		if err := convertAssign(d, row[i]); err != nil {
			return fmt.Errorf("bangdb: column %d: %w", i+1, err)
		}
	}
	return nil
}

// Row is the result of QueryRow.
type Row struct {
	rows Rows // held by value: one allocation per QueryRow
	err  error
}

// Scan scans the first row, or returns ErrNoRows.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
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
		case value.Decimal:
			d, err := x.Rescale(0)
			if err != nil || d.Cmp(x) != 0 {
				return fmt.Errorf("cannot convert decimal %s to integer without losing digits", x)
			}
			n = d.Unscaled
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
	case *Decimal:
		dv, ok := value.ToDecimal(v)
		if !ok {
			return fmt.Errorf("cannot scan %s into *bangdb.Decimal", value.TypeOf(v))
		}
		*d = dv
		return nil
	case *float64:
		switch x := v.(type) {
		case value.Decimal:
			*d = x.Float64()
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
