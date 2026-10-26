package bangdb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMemoryTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT) USING btree;
		CREATE TABLE sessions (token TEXT PRIMARY KEY, user_id INT REFERENCES users ON DELETE CASCADE, hits INT DEFAULT 0) USING memory;
		CREATE INDEX sessions_user ON sessions (user_id);
		INSERT INTO users VALUES (1, 'kim'), (2, 'lee')`)
	for i := 0; i < 2000; i++ {
		exec(t, db, `INSERT INTO sessions (token, user_id) VALUES (?, ?)`, fmt.Sprintf("t%04d", i), i%2+1)
	}
	expect(t, db, `SELECT u.name, count(*) FROM sessions s JOIN users u ON u.id = s.user_id GROUP BY u.name ORDER BY 1`, "kim|1000;lee|1000")
	expect(t, db, `SELECT user_id FROM sessions WHERE token = 't0007'`, "2")
	if _, err := db.Exec(`INSERT INTO sessions (token, user_id) VALUES ('t0007', 1)`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("primary key on a memory table: %v", err)
	}
	mustFail(t, db, `INSERT INTO sessions (token, user_id) VALUES ('x', 9)`, "FOREIGN KEY")

	// A failing statement is rolled back as a whole; so is a transaction.
	mustFail(t, db, `INSERT INTO sessions (token, user_id) VALUES ('new1', 1), ('t0001', 1)`, "UNIQUE")
	expect(t, db, `SELECT count(*) FROM sessions WHERE token = 'new1'`, "0")
	tx, _ := db.Begin()
	tx.Exec(`UPDATE sessions SET hits = 5`)
	tx.Exec(`DELETE FROM sessions WHERE user_id = 1`)
	tx.Rollback()
	expect(t, db, `SELECT count(*), sum(hits) FROM sessions`, "2000|0")

	// Cascades from a durable parent reach the memory table.
	exec(t, db, `DELETE FROM users WHERE id = 1`)
	expect(t, db, `SELECT count(*) FROM sessions`, "1000")

	// A durable table may not depend on volatile rows.
	mustFail(t, db, `CREATE TABLE audit (token TEXT REFERENCES sessions) USING btree`, "durable table cannot reference a memory table")

	// A reader keeps its snapshot while the memory table changes.
	rows, err := db.Query(`SELECT token FROM sessions ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `DELETE FROM sessions WHERE token > 't0100'`)
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != 1000 {
		t.Fatalf("snapshot saw %d rows", n)
	}
	expect(t, db, `SELECT count(*) FROM sessions`, "50")

	// VACUUM keeps memory tables; the integrity check accounts for them.
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, db, `SELECT count(*) FROM sessions WHERE user_id = 2`, "50")
	checkIntegrity(t, db)
	if d := db.Engine().Catalog().Describe("sessions"); !strings.Contains(d, "USING memory") {
		t.Fatalf("describe: %s", d)
	}
	db.Close()

	// After a restart the memory table exists but is empty.
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expect(t, db, `SELECT count(*) FROM sessions`, "0")
	expect(t, db, `SELECT name FROM users`, "lee")
	exec(t, db, `INSERT INTO sessions (token, user_id) VALUES ('again', 2)`)
	expect(t, db, `SELECT token FROM sessions WHERE user_id = 2`, "again")
	exec(t, db, `DROP TABLE sessions`)
	checkIntegrity(t, db)
}

func TestMemoryTableConcurrency(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE c (id INTEGER PRIMARY KEY, a INT, b INT) USING memory; CREATE INDEX c_a ON c (a)`)
	for i := 1; i <= 500; i++ {
		exec(t, db, `INSERT INTO c VALUES (?, 0, 0)`, i)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var sa, sb int
				if err := db.QueryRow(`SELECT sum(a), sum(b) FROM c WHERE a >= 0`).Scan(&sa, &sb); err != nil || sa != sb {
					errs <- fmt.Errorf("torn read %d/%d: %v", sa, sb, err)
					return
				}
			}
		}()
	}
	for i := 0; i < 300; i++ {
		exec(t, db, `UPDATE c SET a = a + 1, b = b + 1 WHERE id % 7 = ?`, i%7)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
