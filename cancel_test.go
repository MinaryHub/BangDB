package bangdb

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func slowDB(t *testing.T, opts *Options) *DB {
	db, err := OpenWithOptions(":memory:", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	db.Update(func(tx *Tx) error {
		for i := 0; i < 3000; i++ {
			tx.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprint(i))
		}
		return nil
	})
	return db
}

const slowQuery = `SELECT COUNT(*) FROM t a JOIN t b ON a.v = b.v`

func TestQueryCancellation(t *testing.T) {
	db := slowDB(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	var n int
	err := db.QueryRowContext(ctx, slowQuery).Scan(&n)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("cancellation took %v", el)
	}

	// In a script, earlier statements stay committed; the interrupted one
	// releases the write lock.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	_, err = db.ExecContext(ctx2, `INSERT INTO t (v) VALUES ('should-not-stay'); `+slowQuery)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("script: want DeadlineExceeded, got %v", err)
	}
	expect(t, db, `SELECT COUNT(*) FROM t WHERE v = 'should-not-stay'`, "1") // first statement committed on its own
	exec(t, db, `INSERT INTO t (v) VALUES ('lock released')`)
}

func TestQueryTimeoutOption(t *testing.T) {
	db := slowDB(t, &Options{QueryTimeout: 50 * time.Millisecond})
	var n int
	if err := db.QueryRow(slowQuery).Scan(&n); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	// Inside a transaction the canceled statement is undone, the tx survives.
	tx, _ := db.Begin()
	tx.Exec(`INSERT INTO t (v) VALUES ('kept')`)
	if _, err := tx.Query(slowQuery); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tx: want DeadlineExceeded, got %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	expect(t, db, `SELECT COUNT(*) FROM t WHERE v = 'kept'`, "1")
}

func TestBeginContextWaitsForLock(t *testing.T) {
	db := open(t)
	tx, _ := db.Begin()
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := db.BeginContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}
