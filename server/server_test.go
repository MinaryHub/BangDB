package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/client"
	_ "github.com/knoxxr/bangdb/driver"
	"github.com/knoxxr/bangdb/server"
)

func start(t *testing.T, opts server.Options) (*bangdb.DB, *server.Server, string) {
	t.Helper()
	db, err := bangdb.OpenWithOptions(":memory:", &bangdb.Options{BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(db, opts)
	go s.Serve(ln)
	t.Cleanup(func() {
		s.Close()
		db.Close()
	})
	return db, s, ln.Addr().String()
}

func dial(t *testing.T, addr string, opts *client.Options) *client.Conn {
	t.Helper()
	c, err := client.Dial(context.Background(), addr, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func mustExec(t *testing.T, c *client.Conn, q string, args ...any) client.Result {
	t.Helper()
	r, err := c.Exec(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

func TestServerBasics(t *testing.T) {
	db, s, addr := start(t, server.Options{Password: "pw"})
	if _, err := client.Dial(context.Background(), addr, &client.Options{Password: "nope"}); !errors.Is(err, client.ErrUnauthorized) {
		t.Fatalf("wrong password: %v", err)
	}
	c := dial(t, addr, &client.Options{User: "app", Password: "pw"})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, score REAL, data BLOB)")
	r := mustExec(t, c, "INSERT INTO t (name, score, data) VALUES (?, ?, ?), (:n, 2.5, NULL) RETURNING id, name",
		"kim", 1.5, []byte{0, 1}, bangdb.Named("n", "lee"))
	if r.RowsAffected != 2 || r.LastInsertID != 2 || len(r.Rows) != 2 || r.Rows[1][1] != "lee" {
		t.Fatalf("insert result %+v", r)
	}
	rows, err := c.Query(context.Background(), "SELECT id, name, score, data FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rows.Columns(), ",") != "id,name,score,data" || rows.ColumnTypes()[1] != "TEXT" {
		t.Fatalf("columns %v %v", rows.Columns(), rows.ColumnTypes())
	}
	var got []string
	for rows.Next() {
		var id int64
		var name string
		var score float64
		var data []byte
		if err := rows.Scan(&id, &name, &score, &data); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %s %v %x", id, name, score, data))
	}
	if rows.Err() != nil || strings.Join(got, "|") != "1 kim 1.5 0001|2 lee 2.5 " {
		t.Fatalf("rows %q %v", got, rows.Err())
	}
	// Errors keep their kind; the session survives them.
	if _, err := c.Exec(context.Background(), "INSERT INTO t (name) VALUES ('kim')"); !errors.Is(err, bangdb.ErrConstraint) {
		t.Fatalf("constraint error: %v", err)
	}
	if _, err := c.Query(context.Background(), "SELECT * FROM missing"); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("query error: %v", err)
	}
	// The data is in the shared database.
	var n int
	db.QueryRow("SELECT count(*) FROM t").Scan(&n)
	if n != 2 {
		t.Fatalf("server database has %d rows", n)
	}
	if st := s.Stats(); st.Connections != 1 || st.Rejected != 1 || st.Statements < 5 {
		t.Fatalf("stats %+v", st)
	}
}

func TestServerTransactionsAndSessions(t *testing.T) {
	db, _, addr := start(t, server.Options{})
	a := dial(t, addr, nil)
	b := dial(t, addr, nil)
	mustExec(t, a, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)")
	mustExec(t, a, "BEGIN")
	mustExec(t, a, "INSERT INTO t (v) VALUES (1)")
	if !a.InTx() {
		t.Fatal("InTx is false inside a transaction")
	}
	// Another session cannot write meanwhile, and does not see the change.
	if _, err := b.Exec(context.Background(), "INSERT INTO t (v) VALUES (2)"); !errors.Is(err, bangdb.ErrBusy) {
		t.Fatalf("concurrent writer: %v", err)
	}
	r, _ := b.Exec(context.Background(), "SELECT count(*) FROM t")
	if r.Rows[0][0] != int64(0) {
		t.Fatal("uncommitted row visible to another session")
	}
	mustExec(t, a, "COMMIT")
	mustExec(t, b, "INSERT INTO t (v) VALUES (2)")

	// A connection closed inside a transaction is rolled back.
	c := dial(t, addr, nil)
	mustExec(t, c, "BEGIN; INSERT INTO t (v) VALUES (3)")
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := db.Exec("INSERT INTO t (v) VALUES (4)"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned transaction kept the write lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var vals string
	db.QueryRow("SELECT group_concat(v) FROM t").Scan(&vals)
	if vals != "1,2,4" {
		t.Fatalf("values %s", vals)
	}
}

func TestServerStreamingAndCancel(t *testing.T) {
	_, _, addr := start(t, server.Options{})
	c := dial(t, addr, nil)
	mustExec(t, c, "CREATE TABLE n (i INTEGER PRIMARY KEY)")
	mustExec(t, c, "BEGIN")
	for i := 0; i < 20000; i++ {
		mustExec(t, c, "INSERT INTO n VALUES (?)", i)
	}
	mustExec(t, c, "COMMIT")

	rows, err := c.Query(context.Background(), "SELECT i FROM n")
	if err != nil {
		t.Fatal(err)
	}
	k := 0
	for rows.Next() {
		k++
	}
	if k != 20000 || rows.Err() != nil {
		t.Fatalf("streamed %d rows: %v", k, rows.Err())
	}

	// Closing a result early cancels the rest; the connection stays usable.
	rows, _ = c.Query(context.Background(), "SELECT a.i FROM n a, n b")
	rows.Next()
	start := time.Now()
	rows.Close()
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close did not cancel the query")
	}
	r := mustExec(t, c, "SELECT count(*) FROM n")
	if r.Rows[0][0] != int64(20000) {
		t.Fatalf("after cancel: %v", r.Rows)
	}

	// A context deadline interrupts a long statement.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = c.Exec(ctx, "SELECT count(*) FROM n a, n b")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted statement: %v", err)
	}
	mustExec(t, c, "SELECT 1")
	// A new query discards an unread result.
	rows, _ = c.Query(context.Background(), "SELECT i FROM n")
	rows.Next()
	r = mustExec(t, c, "SELECT 2")
	if r.Rows[0][0] != int64(2) {
		t.Fatal("result after an abandoned one")
	}
}

func TestServerLimitsAndClose(t *testing.T) {
	_, s, addr := start(t, server.Options{MaxConns: 1, IdleTimeout: 200 * time.Millisecond})
	a := dial(t, addr, nil)
	if _, err := client.Dial(context.Background(), addr, nil); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("second connection: %v", err)
	}
	time.Sleep(400 * time.Millisecond) // idle: the server closes a
	if err := a.Ping(context.Background()); err == nil {
		t.Fatal("idle connection still open")
	}
	b := dial(t, addr, nil)
	mustExec(t, b, "SELECT 1")
	s.Close()
	if _, err := b.Exec(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("connection survived server Close")
	}
}

func TestServerTLSAndDatabaseSQL(t *testing.T) {
	db, err := bangdb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cert := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(db, server.Options{Authenticate: func(u, p string) bool { return u == "app" && p == "pw" }})
	go s.Serve(ln)
	defer s.Close()

	sdb, err := sql.Open("bangdb", "bangdb://app:pw@"+ln.Addr().String()+"?tls=insecure")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	if _, err := sdb.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := sdb.Exec("INSERT INTO t (v) VALUES (?)", g); err != nil && !errors.Is(err, bangdb.ErrBusy) {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	tx, err := sdb.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec("INSERT INTO t (v) VALUES (-1)")
	tx.Rollback()
	var n, neg int
	if err := sdb.QueryRow("SELECT count(*), count(CASE WHEN v < 0 THEN 1 END) FROM t").Scan(&n, &neg); err != nil {
		t.Fatal(err)
	}
	if n == 0 || neg != 0 {
		t.Fatalf("rows %d, rolled back rows %d", n, neg)
	}
	rows, err := sdb.Query("SELECT id, v FROM t WHERE v = ? ORDER BY id", 3)
	if err != nil {
		t.Fatal(err)
	}
	k := 0
	for rows.Next() {
		k++
	}
	rows.Close()
	if k == 0 {
		t.Fatal("no rows through database/sql")
	}
	if _, err := sql.Open("bangdb", "bangdb://"); err == nil {
		t.Error("DSN without a host accepted")
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
