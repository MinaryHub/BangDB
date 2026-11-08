// Package bangdb is BangDB for Android (and iOS) apps, through gomobile:
//
//	gomobile bind -target=android -javapkg=com.knoxxr -o bangdb.aar ./mobile
//
// gives the Java/Kotlin classes com.knoxxr.bangdb.Bangdb (functions),
// Database, Tx, Rows, Args, Result, Options and Migrations. The API uses only
// types gomobile can bind: SQL parameters go in an Args list, results are
// read with typed getters, and every method that can fail throws an
// exception on the Java side.
//
//	val db = Bangdb.open(context.filesDir.path + "/app.db")
//	db.exec("CREATE TABLE IF NOT EXISTS note (id INTEGER PRIMARY KEY, body TEXT)", null)
//	db.exec("INSERT INTO note (body) VALUES (?)", Bangdb.newArgs().addString("hello"))
//	val rows = db.query("SELECT id, body FROM note", null)
//	while (rows.next()) { println("${rows.getLong(0)} ${rows.getString(1)}") }
//
// Keep the database in the app's private storage (Context.getFilesDir or
// getNoBackupFilesDir): shared storage does not support the file locking
// and syncing the database relies on.
package bangdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	core "github.com/minaryhub/bangdb"
)

// Version returns the BangDB version.
func Version() string { return core.Version }

// Options configures Open. Zero values mean the defaults.
type Options struct {
	// EncryptionKey encrypts the file (32 random bytes, e.g. kept in the
	// Android Keystore). Empty: not encrypted.
	EncryptionKey []byte
	// CacheSize in bytes (default 32 MiB; phones may prefer 8 MiB).
	CacheSize int64
	// SortMemory and TxMemory in bytes (defaults 64 MiB) bound memory for
	// large sorts and transactions; beyond them data goes to TempDir.
	SortMemory int64
	TxMemory   int64
	// TempDir for those files; use Context.getCacheDir. Default: the
	// system temporary directory, which apps may not be able to write.
	TempDir string
	// BusyTimeoutMillis: how long a writer waits for another (default 5000).
	BusyTimeoutMillis int64
	// QueryTimeoutMillis limits every statement (0: no limit).
	QueryTimeoutMillis int64
}

// NewOptions returns default options.
func NewOptions() *Options { return &Options{} }

// Database is an open database. It is safe to use from several threads;
// writes are serialized.
type Database struct {
	db *core.DB
}

// Open opens or creates a database file (":memory:" for a temporary one).
func Open(path string) (*Database, error) { return OpenWithOptions(path, nil) }

// OpenWithOptions opens a database with options (nil: defaults).
func OpenWithOptions(path string, o *Options) (*Database, error) {
	var opts *core.Options
	if o != nil {
		opts = &core.Options{
			EncryptionKey: o.EncryptionKey,
			CacheSize:     int(o.CacheSize),
			SortMemory:    int(o.SortMemory),
			TxMemory:      int(o.TxMemory),
			TempDir:       o.TempDir,
			BusyTimeout:   time.Duration(o.BusyTimeoutMillis) * time.Millisecond,
			QueryTimeout:  time.Duration(o.QueryTimeoutMillis) * time.Millisecond,
		}
	}
	db, err := core.OpenWithOptions(path, opts)
	if err != nil {
		return nil, err
	}
	return &Database{db: db}, nil
}

// Close closes the database (call it when the app no longer needs it).
func (d *Database) Close() error { return d.db.Close() }

// Exec runs one or more statements that return no rows. args may be nil.
func (d *Database) Exec(sql string, args *Args) (*Result, error) {
	r, err := d.db.Exec(sql, args.values()...)
	if err != nil {
		return nil, err
	}
	return &Result{RowsAffected: r.RowsAffected, LastInsertID: r.LastInsertID}, nil
}

// Query runs a SELECT (or a statement with RETURNING). Close the Rows if
// you stop before the end.
func (d *Database) Query(sql string, args *Args) (*Rows, error) {
	rows, err := d.db.Query(sql, args.values()...)
	if err != nil {
		return nil, err
	}
	return &Rows{r: rows}, nil
}

// QueryJSON runs a query and returns {"columns":[...],"rows":[[...],...]}.
// Integers and decimals are JSON numbers, blobs base64 strings, NULL null.
func (d *Database) QueryJSON(sql string, args *Args) (string, error) {
	rows, err := d.db.Query(sql, args.values()...)
	if err != nil {
		return "", err
	}
	return toJSON(&Rows{r: rows})
}

// Begin starts a write transaction. Finish it with Commit or Rollback.
func (d *Database) Begin() (*Tx, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// Backup writes a consistent copy of the database to path while it stays
// in use.
func (d *Database) Backup(path string) error { return d.db.Backup(path) }

// VerifyBackup checks that a backup opens and is intact.
func (d *Database) VerifyBackup(path string) error {
	return d.db.VerifyBackup(context.Background(), path)
}

// Vacuum rebuilds the file to give free space back to the system.
func (d *Database) Vacuum() error { return d.db.Vacuum(context.Background()) }

// IntegrityCheck returns "ok" or the problems found, one per line.
func (d *Database) IntegrityCheck() (string, error) {
	problems, err := d.db.IntegrityCheck()
	if err != nil {
		return "", err
	}
	if len(problems) == 0 {
		return "ok", nil
	}
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out, nil
}

// Migrate applies the migrations newer than the database's user_version, in
// order, each in its own transaction. It returns how many were applied.
func (d *Database) Migrate(m *Migrations) (int64, error) {
	if m == nil {
		return 0, nil
	}
	done, err := d.db.Migrate(context.Background(), m.steps)
	return int64(len(done)), err
}

// SchemaVersion returns PRAGMA user_version.
func (d *Database) SchemaVersion() (int64, error) {
	var v int64
	err := d.db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

// Tx is a write transaction. It must be used from one thread at a time.
type Tx struct {
	tx *core.Tx
}

// Exec runs statements in the transaction.
func (t *Tx) Exec(sql string, args *Args) (*Result, error) {
	r, err := t.tx.Exec(sql, args.values()...)
	if err != nil {
		return nil, err
	}
	return &Result{RowsAffected: r.RowsAffected, LastInsertID: r.LastInsertID}, nil
}

// Query runs a query that sees the transaction's changes.
func (t *Tx) Query(sql string, args *Args) (*Rows, error) {
	rows, err := t.tx.Query(sql, args.values()...)
	if err != nil {
		return nil, err
	}
	return &Rows{r: rows}, nil
}

// QueryJSON is Database.QueryJSON inside the transaction.
func (t *Tx) QueryJSON(sql string, args *Args) (string, error) {
	rows, err := t.tx.Query(sql, args.values()...)
	if err != nil {
		return "", err
	}
	return toJSON(&Rows{r: rows})
}

// Commit makes the changes durable.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback discards the changes.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Result reports what Exec changed.
type Result struct {
	RowsAffected int64
	LastInsertID int64
}

// Migrations is an ordered list of schema changes for Database.Migrate.
type Migrations struct {
	steps []core.Migration
}

// NewMigrations returns an empty list.
func NewMigrations() *Migrations { return &Migrations{} }

// Add appends migration version (1, 2, 3, ...) whose SQL brings the schema
// from version-1 to version. It returns the list, for chaining.
func (m *Migrations) Add(version int64, name, sql string) *Migrations {
	m.steps = append(m.steps, core.Migration{Version: int(version), Name: name, SQL: sql})
	return m
}

// ---- parameters ----

// Args is a list of SQL parameters (for ?, ?1 ... in statement order).
// Add methods return the list, for chaining.
type Args struct {
	vals []any
}

// NewArgs returns an empty parameter list.
func NewArgs() *Args { return &Args{} }

func (a *Args) values() []any {
	if a == nil {
		return nil
	}
	return a.vals
}

// AddLong adds an integer.
func (a *Args) AddLong(v int64) *Args { a.vals = append(a.vals, v); return a }

// AddDouble adds a floating-point number.
func (a *Args) AddDouble(v float64) *Args { a.vals = append(a.vals, v); return a }

// AddString adds text.
func (a *Args) AddString(v string) *Args { a.vals = append(a.vals, v); return a }

// AddBytes adds a blob.
func (a *Args) AddBytes(v []byte) *Args {
	a.vals = append(a.vals, append([]byte{}, v...)) // the caller's array may be reused
	return a
}

// AddBool adds true or false (stored as 1 or 0).
func (a *Args) AddBool(v bool) *Args { a.vals = append(a.vals, v); return a }

// AddNull adds NULL.
func (a *Args) AddNull() *Args { a.vals = append(a.vals, nil); return a }

// AddDecimal adds an exact decimal given as text, e.g. "12.50".
func (a *Args) AddDecimal(v string) (*Args, error) {
	d, err := core.ParseDecimal(v)
	if err != nil {
		return a, err
	}
	a.vals = append(a.vals, d)
	return a, nil
}

// Size returns the number of parameters.
func (a *Args) Size() int64 { return int64(len(a.vals)) }

// ---- results ----

// Column types reported by Rows.ColumnType.
const (
	TypeNull    = 0
	TypeLong    = 1
	TypeDouble  = 2
	TypeString  = 3
	TypeBytes   = 4
	TypeDecimal = 5
)

// Rows is a query result read one row at a time:
//
//	while (rows.next()) { rows.getLong(0); rows.getString(1) }
//
// It holds a snapshot until the end is reached or Close is called.
type Rows struct {
	r   *core.Rows
	cur []any
}

// Next moves to the next row; false at the end (or on error: see Err).
func (r *Rows) Next() bool {
	if !r.r.Next() {
		r.cur = nil
		return false
	}
	r.cur = r.r.Values()
	return true
}

// Err returns the error that ended the iteration, if any.
func (r *Rows) Err() error { return r.r.Err() }

// Close releases the result early.
func (r *Rows) Close() error { return r.r.Close() }

// ColumnCount returns the number of columns.
func (r *Rows) ColumnCount() int64 { return int64(len(r.r.Columns())) }

// ColumnName returns the name of column i (from 0).
func (r *Rows) ColumnName(i int64) string {
	cols := r.r.Columns()
	if i < 0 || int(i) >= len(cols) {
		return ""
	}
	return cols[i]
}

// ColumnIndex returns the position of the named column, or -1.
func (r *Rows) ColumnIndex(name string) int64 {
	for i, c := range r.r.Columns() {
		if c == name {
			return int64(i)
		}
	}
	return -1
}

func (r *Rows) value(i int64) any {
	if i < 0 || int(i) >= len(r.cur) {
		return nil
	}
	return r.cur[i]
}

// ColumnType returns the type of column i's value in the current row
// (TypeNull, TypeLong, ...).
func (r *Rows) ColumnType(i int64) int64 {
	switch r.value(i).(type) {
	case nil:
		return TypeNull
	case int64:
		return TypeLong
	case float64:
		return TypeDouble
	case string:
		return TypeString
	case []byte:
		return TypeBytes
	case core.Decimal:
		return TypeDecimal
	}
	return TypeString
}

// IsNull reports whether column i is NULL.
func (r *Rows) IsNull(i int64) bool { return r.value(i) == nil }

// GetLong returns column i as an integer (0 for NULL; text is parsed).
func (r *Rows) GetLong(i int64) (int64, error) {
	switch v := r.value(i).(type) {
	case nil:
		return 0, nil
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case core.Decimal:
		return int64(v.Float64()), nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, fmt.Errorf("bangdb: column %d is not a number", i)
}

// GetDouble returns column i as a floating-point number (0 for NULL).
func (r *Rows) GetDouble(i int64) (float64, error) {
	switch v := r.value(i).(type) {
	case nil:
		return 0, nil
	case int64:
		return float64(v), nil
	case float64:
		return v, nil
	case core.Decimal:
		return v.Float64(), nil
	case string:
		return strconv.ParseFloat(v, 64)
	}
	return 0, fmt.Errorf("bangdb: column %d is not a number", i)
}

// GetBool returns column i as a boolean (non-zero numbers are true).
func (r *Rows) GetBool(i int64) (bool, error) {
	n, err := r.GetDouble(i)
	return n != 0, err
}

// GetString returns column i as text ("" for NULL). Numbers and decimals
// are formatted; use this for DECIMAL columns to keep them exact.
func (r *Rows) GetString(i int64) string {
	switch v := r.value(i).(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case core.Decimal:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

// GetBytes returns column i as bytes (nil for NULL).
func (r *Rows) GetBytes(i int64) []byte {
	switch v := r.value(i).(type) {
	case nil:
		return nil
	case []byte:
		return v
	default:
		return []byte(r.GetString(i))
	}
}

// ---- JSON ----

func toJSON(r *Rows) (string, error) {
	defer r.Close()
	out := struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}{Columns: r.r.Columns(), Rows: [][]any{}}
	for r.Next() {
		row := make([]any, len(r.cur))
		for i, v := range r.cur {
			switch v := v.(type) {
			case core.Decimal:
				row[i] = json.Number(v.String())
			default:
				row[i] = v // []byte becomes base64
			}
		}
		out.Rows = append(out.Rows, row)
	}
	if err := r.Err(); err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// ---- errors ----

// Java receives errors as exceptions carrying the message; these classify
// such a message.

// IsConstraintError reports whether an error message is a UNIQUE, NOT NULL,
// CHECK or foreign key violation.
func IsConstraintError(message string) bool { return strings.Contains(message, "constraint failed") }

// IsBusyError reports whether an error message means another writer held
// the lock longer than the busy timeout.
func IsBusyError(message string) bool {
	return strings.Contains(message, core.ErrBusy.Error())
}

// IsLockedError reports whether an error message means another process (or
// another open Database) already has the file open.
func IsLockedError(message string) bool {
	return strings.Contains(message, core.ErrLocked.Error())
}
