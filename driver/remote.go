package driver

import (
	"context"
	"database/sql/driver"
	"io"
	"strings"

	"github.com/minaryhub/bangdb/client"
	"github.com/minaryhub/bangdb/internal/value"
)

// Connections to a server (DSN "bangdb://...").

type remoteConnector struct {
	addr string
	opts *client.Options
	drv  *Driver
}

func (c *remoteConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cc, err := client.Dial(ctx, c.addr, c.opts)
	if err != nil {
		return nil, err
	}
	return &remoteConn{c: cc}, nil
}

func (c *remoteConnector) Driver() driver.Driver { return c.drv }

type remoteConn struct{ c *client.Conn }

func (c *remoteConn) Prepare(query string) (driver.Stmt, error) {
	return &remoteStmt{c: c, query: query}, nil
}

func (c *remoteConn) Close() error { return c.c.Close() }

func (c *remoteConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *remoteConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if _, err := c.c.Exec(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	return &remoteTx{c: c}, nil
}

func (c *remoteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals, _ := values(args)
	r, err := c.c.Exec(ctx, query, vals...)
	if err != nil {
		return nil, err
	}
	return remoteResult{r}, nil
}

func (c *remoteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals, _ := values(args)
	r, err := c.c.Query(ctx, query, vals...)
	if err != nil {
		return nil, err
	}
	return &remoteRows{r: r}, nil
}

func (c *remoteConn) Ping(ctx context.Context) error {
	if err := c.c.Ping(ctx); err != nil {
		return driver.ErrBadConn
	}
	return nil
}

// ResetSession discards a connection returned to the pool inside a
// transaction (the server rolls it back when the connection closes).
func (c *remoteConn) ResetSession(context.Context) error {
	if c.c.InTx() {
		return driver.ErrBadConn
	}
	return nil
}

type remoteTx struct{ c *remoteConn }

func (t *remoteTx) Commit() error {
	_, err := t.c.c.Exec(context.Background(), "COMMIT")
	return err
}

func (t *remoteTx) Rollback() error {
	_, err := t.c.c.Exec(context.Background(), "ROLLBACK")
	return err
}

type remoteStmt struct {
	c     *remoteConn
	query string
}

func (s *remoteStmt) Close() error  { return nil }
func (s *remoteStmt) NumInput() int { return -1 }

func (s *remoteStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.query, named(args))
}

func (s *remoteStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.query, named(args))
}

func (s *remoteStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func (s *remoteStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

type remoteResult struct{ r client.Result }

func (r remoteResult) LastInsertId() (int64, error) { return r.r.LastInsertID, nil }
func (r remoteResult) RowsAffected() (int64, error) { return r.r.RowsAffected, nil }

type remoteRows struct{ r *client.Rows }

func (r *remoteRows) Columns() []string { return r.r.Columns() }
func (r *remoteRows) Close() error      { return r.r.Close() }

func (r *remoteRows) ColumnTypeDatabaseTypeName(i int) string {
	return strings.ToUpper(r.r.ColumnTypes()[i])
}

func (r *remoteRows) Next(dest []driver.Value) error {
	if !r.r.Next() {
		if err := r.r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	for i, v := range r.r.Values() {
		if d, ok := v.(value.Decimal); ok {
			v = d.String()
		}
		dest[i] = v
	}
	return nil
}
