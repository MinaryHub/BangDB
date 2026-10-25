package bangdb

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCrashTorture repeatedly SIGKILLs a process that is committing and then
// checks, after reopening, that
//   - every commit the child reported as done is present (durability),
//   - at most the one in-flight commit is additionally present (atomicity),
//   - the two tables written by every transaction agree (consistency),
//   - the integrity check finds nothing.
//
// BANGDB_TORTURE=n runs n kill cycles (default 15; use -short to skip).
func TestCrashTorture(t *testing.T) {
	if os.Getenv("BANGDB_TORTURE_CHILD") != "" {
		tortureChild(os.Getenv("BANGDB_TORTURE_CHILD"))
		return
	}
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	cycles := 15
	if n, err := strconv.Atoi(os.Getenv("BANGDB_TORTURE")); err == nil {
		cycles = n
	}
	dir := t.TempDir()
	path := ""
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for cycle := 0; cycle < cycles; cycle++ {
		if cycle%25 == 0 { // keep cycle time constant: start a fresh database
			path = filepath.Join(dir, fmt.Sprintf("torture-%d.db", cycle))
		}
		cmd := osexec.Command(os.Args[0], "-test.run=^TestCrashTorture$", "-test.count=1")
		cmd.Env = append(os.Environ(), "BANGDB_TORTURE_CHILD="+path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		acked := make(chan int64, 1)
		go func() {
			last := int64(-1)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "committed "); ok {
					last, _ = strconv.ParseInt(v, 10, 64)
				}
			}
			acked <- last
		}()
		time.Sleep(time.Duration(100+rng.Intn(400)) * time.Millisecond)
		cmd.Process.Kill()
		cmd.Wait()
		lastAcked := <-acked

		db, err := Open(path)
		if err != nil {
			t.Fatalf("cycle %d: reopen after kill: %v", cycle, err)
		}
		var n, na, nb int64
		if err := db.QueryRow(`SELECT n FROM meta WHERE k = 1`).Scan(&n); err != nil && lastAcked >= 0 {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		db.QueryRow(`SELECT COUNT(*) FROM a`).Scan(&na)
		db.QueryRow(`SELECT COUNT(*) FROM b`).Scan(&nb)
		if lastAcked >= 0 && (n < lastAcked || n > lastAcked+1) {
			t.Fatalf("cycle %d: child acknowledged commit %d but the database has %d", cycle, lastAcked, n)
		}
		if na != nb {
			t.Fatalf("cycle %d: tables disagree: a=%d b=%d", cycle, na, nb)
		}
		if problems, err := db.IntegrityCheck(); err != nil || len(problems) > 0 {
			t.Fatalf("cycle %d: integrity: %v %v", cycle, err, problems)
		}
		db.Close()
		if cycle%25 == 24 {
			os.Remove(path)
			os.Remove(path + "-wal")
		}
		t.Logf("cycle %d: killed after commit %d, recovered %d (rows %d)", cycle, lastAcked, n, na)
	}
}

// tortureChild commits forever; the parent kills it.
func tortureChild(path string) {
	db, err := Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(2)
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS meta (k INTEGER PRIMARY KEY, n INT);
		CREATE TABLE IF NOT EXISTS a (id INTEGER PRIMARY KEY, v INT, pad TEXT);
		CREATE TABLE IF NOT EXISTS b (id INTEGER PRIMARY KEY, v INT UNIQUE);
		CREATE INDEX IF NOT EXISTS a_v ON a (v)`)
	var n int64
	if err := db.QueryRow(`SELECT n FROM meta WHERE k = 1`).Scan(&n); err != nil {
		db.Exec(`INSERT INTO meta VALUES (1, 0)`)
	}
	// A reader keeps snapshots open so checkpoints must work around them.
	go func() {
		for {
			if rows, err := db.Query(`SELECT a.v FROM a JOIN b ON b.v = a.v`); err == nil {
				for rows.Next() {
				}
				rows.Close()
			}
		}
	}()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		err := db.Update(func(tx *Tx) error {
			for i := 0; i < 1+rng.Intn(20); i++ {
				v := rng.Int63()
				pad := strings.Repeat("x", rng.Intn(3000)) // sometimes overflow pages
				if _, err := tx.Exec(`INSERT INTO a (v, pad) VALUES (?, ?)`, v, pad); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO b (v) VALUES (?)`, v); err != nil {
					return err
				}
			}
			if rng.Intn(4) == 0 {
				var v int64
				if err := tx.QueryRow(`SELECT v FROM b ORDER BY id LIMIT 1 OFFSET ?`, rng.Intn(50)).Scan(&v); err == nil {
					tx.Exec(`DELETE FROM a WHERE v = ?`, v)
					tx.Exec(`DELETE FROM b WHERE v = ?`, v)
				}
			}
			_, err := tx.Exec(`UPDATE meta SET n = n + 1 WHERE k = 1`)
			return err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "child commit:", err)
			os.Exit(3)
		}
		n++
		fmt.Printf("committed %d\n", n)
	}
}
