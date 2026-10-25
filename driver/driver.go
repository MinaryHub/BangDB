// Package driver registers BangDB as a database/sql driver named "bangdb".
//
//	import (
//		"database/sql"
//		_ "github.com/knoxxr/bangdb/driver"
//	)
//
//	db, err := sql.Open("bangdb", "app.db") // or ":memory:"
//
// All connections of one *sql.DB share a single engine instance, so an
// in-memory database is visible to every connection of that *sql.DB.
package driver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
)

func init() { sql.Register("bangdb", &Driver{}) }

// Driver implements driver.Driver and driver.DriverContext.
type Driver struct{}

// Open is used by database/sql only when OpenConnector is unavailable; it
// opens a fresh engine whose lifetime is tied to the connection.
func (d *Driver) Open(name string) (driver.Conn, error) {
	db, err := engine.Open(name, nil)
	if err != nil {
		return nil, err
	}
	return &conn{c: db.Conn(), owned: db}, nil
}

// OpenConnector opens the database once; connections share it.
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	db, err := engine.Open(name, nil)
	if err != nil {
		return nil, err
	}
	return &connector{db: db, drv: d}, nil
}

type connector struct {
	db  *engine.DB
	drv *Driver
}

func (c *connector) Connect(context.Context) (driver.Conn, error) { return &conn{c: c.db.Conn()}, nil }
func (c *connector) Driver() driver.Driver                        { return c.drv }

// Close is called by sql.DB.Close.
func (c *connector) Close() error { return c.db.Close() }

var _ io.Closer = (*connector)(nil)

type conn struct {
	c     *engine.Conn
	owned *engine.DB
}

func (c *conn) Prepare(query string) (driver.Stmt, error) { return &stmt{c: c, query: query}, nil }

func (c *conn) Close() error {
	err := c.c.Close()
	if c.owned != nil {
		err = errors.Join(err, c.owned.Close())
	}
	return err
}

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

// BeginTx starts a transaction. Read-only transactions are served as
// regular ones (they take the single-writer lock).
func (c *conn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if _, err := c.c.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals, err := values(args)
	if err != nil {
		return nil, err
	}
	r, err := c.c.ExecContext(ctx, query, vals...)
	if err != nil {
		return nil, err
	}
	return result{r}, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals, err := values(args)
	if err != nil {
		return nil, err
	}
	r, err := c.c.QueryRows(ctx, query, vals...)
	if err != nil {
		return nil, err
	}
	return &rows{r: r}, nil
}

// ResetSession rolls back a transaction abandoned on a pooled connection.
func (c *conn) ResetSession(context.Context) error {
	if c.c.InTx() {
		return driver.ErrBadConn
	}
	return nil
}

func values(args []driver.NamedValue) ([]any, error) {
	out := make([]any, len(args))
	for i, a := range args {
		if a.Name != "" {
			out[i] = engine.NamedArg{Name: a.Name, Value: a.Value}
		} else {
			out[i] = a.Value
		}
	}
	return out, nil
}

type tx struct{ c *conn }

func (t *tx) Commit() error {
	_, err := t.c.c.Exec("COMMIT")
	return err
}

func (t *tx) Rollback() error {
	_, err := t.c.c.Exec("ROLLBACK")
	return err
}

type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error { return nil }

// NumInput lets database/sql check the argument count of statements that
// only use positional parameters.
func (s *stmt) NumInput() int {
	n, named, err := s.c.c.NumParams(s.query)
	if err != nil || named {
		return -1
	}
	return n
}

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.query, named(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.query, named(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

type result struct{ r engine.Result }

func (r result) LastInsertId() (int64, error) { return r.r.LastInsertID, nil }
func (r result) RowsAffected() (int64, error) { return r.r.RowsAffected, nil }

type rows struct{ r *engine.Rows }

func (r *rows) Columns() []string { return r.r.Columns() }

// ColumnTypeDatabaseTypeName reports the declared type of a column.
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	return strings.ToUpper(r.r.ColumnTypes()[i])
}

// ColumnTypeScanType suggests a Go type for a column.
func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	switch value.ParseType(r.r.ColumnTypes()[i]) {
	case value.Integer:
		return reflect.TypeOf(int64(0))
	case value.Real:
		return reflect.TypeOf(float64(0))
	case value.Text:
		return reflect.TypeOf("")
	case value.Blob:
		return reflect.TypeOf([]byte(nil))
	}
	return reflect.TypeOf((*any)(nil)).Elem()
}
func (r *rows) Close() error { return r.r.Close() }

func (r *rows) Next(dest []driver.Value) error {
	if !r.r.Next() {
		if err := r.r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	for i, v := range r.r.Row() {
		dest[i] = v
	}
	return nil
}
