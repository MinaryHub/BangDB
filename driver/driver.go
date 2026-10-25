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

	"github.com/knoxxr/bangdb/internal/engine"
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
func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if _, err := c.c.Exec("BEGIN"); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

func (c *conn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals, err := values(args)
	if err != nil {
		return nil, err
	}
	r, err := c.c.Exec(query, vals...)
	if err != nil {
		return nil, err
	}
	return result{r}, nil
}

func (c *conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals, err := values(args)
	if err != nil {
		return nil, err
	}
	r, err := c.c.Exec(query, vals...)
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
			return nil, errors.New("bangdb: named parameters are not supported; use ?")
		}
		out[i] = a.Value
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

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

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

type rows struct {
	r   engine.Result
	pos int
}

func (r *rows) Columns() []string { return r.r.Columns }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.r.Rows) {
		return io.EOF
	}
	for i, v := range r.r.Rows[r.pos] {
		dest[i] = v
	}
	r.pos++
	return nil
}
