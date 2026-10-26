package bangdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	db, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(path + "-wal")
	if err != nil {
		return db.Size()
	}
	return db.Size() + wal.Size()
}

func TestVacuum(t *testing.T) {
	durableOnly(t)
	path := filepath.Join(t.TempDir(), "v.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `
		CREATE TABLE doc (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT UNIQUE, body TEXT, att BLOB);
		CREATE INDEX doc_body ON doc (body);
		CREATE TABLE tag (doc INT REFERENCES doc ON DELETE CASCADE, name TEXT CHECK (name <> ''));
		CREATE INDEX tag_doc ON tag (doc);
		CREATE VIEW titles AS SELECT title FROM doc`)
	err = db.Update(func(tx *Tx) error {
		for i := 0; i < 20000; i++ {
			body := strings.Repeat("lorem ", 10+i%50)
			var att []byte
			if i%500 == 9 {
				att = []byte(strings.Repeat("big ", 3000)) // overflow pages
			}
			res, err := tx.Exec(`INSERT INTO doc (title, body, att) VALUES (?, ?, ?)`, "t"+strconv.Itoa(i), body, att)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO tag VALUES (?, 'x')`, res.LastInsertID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `DELETE FROM doc WHERE id % 10 <> 0`)
	db.Engine().Checkpoint()
	before := fileSize(t, path)
	seq := db.Seq()

	exec(t, db, `VACUUM`)
	after := fileSize(t, path)
	if after*3 > before {
		t.Fatalf("file size %d -> %d: not compacted", before, after)
	}
	if db.Seq() <= seq {
		t.Fatalf("seq went from %d to %d", seq, db.Seq())
	}
	checkIntegrity(t, db)
	expect(t, db, `SELECT count(*), sum(id), count(DISTINCT body) FROM doc`, "2000|20010000|5")
	expect(t, db, `SELECT count(*) FROM tag`, "2000")
	expect(t, db, `SELECT count(*) FROM titles`, "2000")
	exec(t, db, `DELETE FROM doc WHERE id = 20000`)
	exec(t, db, `INSERT INTO doc (title) VALUES ('new')`) // AUTOINCREMENT high-water mark survives
	expect(t, db, `SELECT id FROM doc WHERE title = 'new'`, "20001")
	if _, err := db.Exec(`INSERT INTO doc (title) VALUES ('t9')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("unique index after vacuum: %v", err)
	}
	mustFail(t, db, `INSERT INTO tag VALUES (1, 'x')`, "FOREIGN KEY")

	// Not inside a transaction.
	tx, _ := db.Begin()
	if _, err := tx.Exec(`VACUUM`); err == nil {
		t.Fatal("VACUUM inside a transaction")
	}
	tx.Rollback()
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkIntegrity(t, db)
	expect(t, db, `SELECT count(*) FROM doc`, "2000")
	expect(t, db, `SELECT title FROM doc WHERE length(att) = 12000 ORDER BY id LIMIT 2`, "t9;t509")
}

func TestVacuumMemoryAndConcurrency(t *testing.T) {
	durableOnly(t)
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := 0; i < 14; i++ {
		exec(t, db, `INSERT INTO t (v) SELECT ? FROM t UNION ALL SELECT ?`, strings.Repeat("x", 100), "y")
	}
	exec(t, db, `DELETE FROM t WHERE id > 10`)
	pages := db.Engine().PageCount()

	// Readers and writers keep working while VACUUM runs.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 8)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var err error
				if g == 0 {
					_, err = db.Exec(`INSERT INTO t (v) VALUES ('w')`)
				} else {
					var n int
					err = db.QueryRow(`SELECT count(*) FROM t WHERE id <= 10`).Scan(&n)
					if err == nil && n != 10 {
						err = errors.New("reader saw wrong data")
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	for i := 0; i < 5; i++ {
		if err := db.Vacuum(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := db.Engine().PageCount(); got >= pages {
		t.Fatalf("pages %d -> %d", pages, got)
	}
}
