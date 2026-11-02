package bangdb

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Large-scale and long-running tests. They are off by default:
//
//	BANGDB_SCALE=20000000 go test -run TestScale -timeout 2h -v .
//	BANGDB_SOAK=30m       go test -run TestSoak  -timeout 2h -v .
//
// BANGDB_SCALE_DIR chooses where the database goes (default: a temp dir).

func scaleDir(t *testing.T) string {
	if d := os.Getenv("BANGDB_SCALE_DIR"); d != "" {
		d = filepath.Join(d, fmt.Sprintf("bangdb-%s-%d", t.Name(), time.Now().UnixNano()))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
	return t.TempDir()
}

func percentile(d []time.Duration, p float64) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[min(len(d)-1, int(float64(len(d))*p))]
}

// TestScale loads N rows with two indexes, then measures lookups, range
// queries, a full scan, reopen, the integrity check, bulk updates and
// deletes, and VACUUM, checking results along the way.
func TestScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("BANGDB_SCALE"))
	if n <= 0 {
		t.Skip("set BANGDB_SCALE=<rows> to run")
	}
	durableOnly(t)
	path := filepath.Join(scaleDir(t), "scale.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer INT NOT NULL, amount DECIMAL(12,2) NOT NULL,
		status TEXT NOT NULL, created INT NOT NULL, note TEXT);
		CREATE INDEX orders_customer ON orders (customer);
		CREATE INDEX orders_status_created ON orders (status, created)`)
	customers := max(n/50, 1)
	statuses := []string{"new", "paid", "shipped", "done", "cancelled"}
	note := make([]byte, 150)
	for i := range note {
		note[i] = 'a' + byte(i%26)
	}
	rng := rand.New(rand.NewSource(1))
	start := time.Now()
	const batch = 10000
	lastReport, lastRows := time.Now(), 0
	var rates []float64
	for done := 0; done < n; done += batch {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := done; i < min(done+batch, n); i++ {
			amount := Decimal{Unscaled: int64(rng.Intn(1000000)), Scale: 2}
			if _, err := tx.Exec("INSERT INTO orders (customer, amount, status, created, note) VALUES (?, ?, ?, ?, ?)",
				rng.Intn(customers), amount, statuses[rng.Intn(len(statuses))], 1_700_000_000+i, string(note)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if time.Since(lastReport) > 10*time.Second || done+batch >= n {
			rows := min(done+batch, n)
			rate := float64(rows-lastRows) / time.Since(lastReport).Seconds()
			rates = append(rates, rate)
			t.Logf("loaded %d rows (%.0f rows/s now), file %.2f GB, WAL %d pages", rows, rate,
				float64(sizeOf(path))/1e9, db.Stats().WALPages)
			lastReport, lastRows = time.Now(), rows
		}
	}
	load := time.Since(start)
	t.Logf("LOAD: %d rows in %v (%.0f rows/s), %.1f bytes/row", n, load.Round(time.Millisecond),
		float64(n)/load.Seconds(), float64(sizeOf(path))/float64(n))
	peakRSS(t)
	if len(rates) >= 4 && rates[len(rates)-1] < rates[0]/4 {
		t.Errorf("insert rate fell from %.0f to %.0f rows/s while loading", rates[0], rates[len(rates)-1])
	}

	measure := func(name string, count int, fn func(i int)) {
		lat := make([]time.Duration, count)
		start := time.Now()
		for i := 0; i < count; i++ {
			s := time.Now()
			fn(i)
			lat[i] = time.Since(s)
		}
		t.Logf("%-28s %6d ops  %8.0f ops/s  p50 %-10v p99 %v", name, count, float64(count)/time.Since(start).Seconds(),
			percentile(lat, 0.5), percentile(lat, 0.99))
	}
	get, _ := db.Prepare("SELECT customer, amount FROM orders WHERE id = ?")
	measure("point lookup (PK)", 20000, func(int) {
		var c int64
		var a Decimal
		if err := get.QueryRow(rng.Intn(n)+1).Scan(&c, &a); err != nil {
			t.Fatal(err)
		}
	})
	byCustomer, _ := db.Prepare("SELECT count(*), sum(amount) FROM orders WHERE customer = ?")
	measure("index lookup (~50 rows)", 5000, func(int) {
		var c int64
		var s any
		if err := byCustomer.QueryRow(rng.Intn(customers)).Scan(&c, &s); err != nil {
			t.Fatal(err)
		}
	})
	rangeQ, _ := db.Prepare("SELECT id FROM orders WHERE status = ? AND created BETWEEN ? AND ? ORDER BY created LIMIT 100")
	measure("index range + LIMIT 100", 2000, func(int) {
		lo := 1_700_000_000 + rng.Intn(n)
		rows, err := rangeQ.Query(statuses[rng.Intn(len(statuses))], lo, lo+100000)
		if err != nil {
			t.Fatal(err)
		}
		rows.All()
	})
	s := time.Now()
	var total int64
	var sum Decimal
	if err := db.QueryRow("SELECT count(*), sum(amount) FROM orders").Scan(&total, &sum); err != nil || total != int64(n) {
		t.Fatalf("full scan: %d %v", total, err)
	}
	t.Logf("FULL SCAN + SUM: %v (%.0f rows/s)", time.Since(s).Round(time.Millisecond), float64(n)/time.Since(s).Seconds())
	peakRSS(t)
	s = time.Now()
	rows, err := db.Query("SELECT status, count(*), sum(amount) FROM orders GROUP BY status")
	if err != nil {
		t.Fatal(err)
	}
	rows.All()
	t.Logf("GROUP BY status: %v", time.Since(s).Round(time.Millisecond))
	peakRSS(t)

	// Reopen and check.
	s = time.Now()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("CLOSE: %v", time.Since(s).Round(time.Millisecond))
	s = time.Now()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Logf("REOPEN: %v", time.Since(s).Round(time.Millisecond))
	s = time.Now()
	if problems, err := db.IntegrityCheck(); err != nil || len(problems) > 0 {
		t.Fatalf("integrity: %v %v", err, problems)
	}
	t.Logf("INTEGRITY CHECK: %v", time.Since(s).Round(time.Millisecond))
	peakRSS(t)

	// Bulk changes.
	s = time.Now()
	r, err := db.Exec("UPDATE orders SET status = 'archived' WHERE id % 100 = 0")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("UPDATE 1%% (%d rows, indexed column): %v", r.RowsAffected, time.Since(s).Round(time.Millisecond))
	peakRSS(t)
	s = time.Now()
	r, err = db.Exec("DELETE FROM orders WHERE id % 10 = 1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DELETE 10%% (%d rows): %v", r.RowsAffected, time.Since(s).Round(time.Millisecond))
	peakRSS(t)
	before := sizeOf(path)
	s = time.Now()
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("VACUUM: %v, %.2f GB -> %.2f GB", time.Since(s).Round(time.Millisecond), float64(before)/1e9, float64(sizeOf(path))/1e9)
	peakRSS(t)
	want := int64(n) - r.RowsAffected
	if err := db.QueryRow("SELECT count(*) FROM orders").Scan(&total); err != nil || total != want {
		t.Fatalf("after changes: %d rows, want %d (%v)", total, want, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM orders WHERE status = 'archived'").Scan(&total); err != nil || total == 0 {
		t.Fatalf("archived rows: %d %v", total, err)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("heap in use at the end: %.0f MB", float64(ms.HeapInuse)/1e6)
}

// TestSoak runs a mixed workload for a while: writers move money between
// accounts (the total never changes), readers check the total and an index
// in their snapshots, and maintenance (checkpoints, backups with
// verification, ANALYZE, VACUUM) runs alongside. It checks the invariants,
// that memory stays bounded and that only expected errors occur.
func TestSoak(t *testing.T) {
	d, _ := time.ParseDuration(os.Getenv("BANGDB_SOAK"))
	if d <= 0 {
		t.Skip("set BANGDB_SOAK=<duration> to run")
	}
	durableOnly(t)
	dir := scaleDir(t)
	db, err := OpenWithOptions(filepath.Join(dir, "soak.db"), &Options{BusyTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const accounts = 20000
	const start = 1000
	exec(t, db, `CREATE TABLE acct (id INTEGER PRIMARY KEY, owner TEXT NOT NULL, balance DECIMAL(14,2) NOT NULL CHECK (balance >= 0));
		CREATE INDEX acct_owner ON acct (owner);
		CREATE TABLE ledger (id INTEGER PRIMARY KEY AUTOINCREMENT, src INT REFERENCES acct, dst INT REFERENCES acct, amount DECIMAL(14,2), at INT)`)
	db.Update(func(tx *Tx) error {
		for i := 1; i <= accounts; i++ {
			if _, err := tx.Exec("INSERT INTO acct VALUES (?, ?, ?)", i, fmt.Sprintf("owner%05d", i%500), start); err != nil {
				return err
			}
		}
		return nil
	})
	wantTotal := fmt.Sprintf("%d.00", accounts*start)
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var transfers, reads, busy, backups atomic.Int64
	var failed atomic.Value
	fail := func(format string, args ...any) {
		failed.CompareAndSwap(nil, fmt.Sprintf(format, args...))
		cancel()
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for ctx.Err() == nil {
				err := db.Update(func(tx *Tx) error {
					for k := 0; k < 1+rng.Intn(5); k++ {
						a, b := 1+rng.Intn(accounts), 1+rng.Intn(accounts)
						amt := fmt.Sprintf("%d.%02d", rng.Intn(20), rng.Intn(100))
						r, err := tx.Exec("UPDATE acct SET balance = balance - ? WHERE id = ? AND balance >= ?", amt, a, amt)
						if err != nil {
							return err
						}
						if r.RowsAffected == 0 {
							continue
						}
						if _, err := tx.Exec("UPDATE acct SET balance = balance + ? WHERE id = ?", amt, b); err != nil {
							return err
						}
						if _, err := tx.Exec("INSERT INTO ledger (src, dst, amount, at) VALUES (?, ?, ?, ?)", a, b, amt, time.Now().Unix()); err != nil {
							return err
						}
					}
					return nil
				})
				switch {
				case err == nil:
					transfers.Add(1)
				case errors.Is(err, ErrBusy):
					busy.Add(1)
				case ctx.Err() != nil:
				default:
					fail("writer %d: %v", w, err)
				}
			}
		}(w)
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(100 + r)))
			for ctx.Err() == nil {
				var total string
				if err := db.QueryRow("SELECT sum(balance) FROM acct").Scan(&total); err != nil {
					if ctx.Err() == nil {
						fail("reader %d: %v", r, err)
					}
					return
				}
				if total != wantTotal {
					fail("reader %d saw total %s, want %s", r, total, wantTotal)
					return
				}
				var n int64
				owner := fmt.Sprintf("owner%05d", rng.Intn(500))
				if err := db.QueryRow("SELECT count(*) FROM acct WHERE owner = ?", owner).Scan(&n); err != nil || n != accounts/500 {
					if ctx.Err() == nil {
						fail("reader %d: owner %s has %d accounts (%v)", r, owner, n, err)
					}
					return
				}
				reads.Add(1)
			}
		}(r)
	}
	// Maintenance.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(max(d/10, 5*time.Second))
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			bk := filepath.Join(dir, fmt.Sprintf("bk%d.db", i))
			if err := db.Backup(bk); err != nil && ctx.Err() == nil {
				fail("backup: %v", err)
				return
			}
			if err := db.VerifyBackup(ctx, bk); err != nil && ctx.Err() == nil {
				fail("backup check: %v", err)
				return
			}
			os.Remove(bk)
			backups.Add(1)
			var err error
			switch i % 3 {
			case 0:
				_, err = db.Exec("ANALYZE")
			case 1:
				err = db.Vacuum(ctx)
			case 2:
				_, err = db.Exec("DELETE FROM ledger WHERE id < (SELECT max(id) - 50000 FROM ledger)")
			}
			if err != nil && ctx.Err() == nil && !errors.Is(err, ErrBusy) {
				fail("maintenance %d: %v", i, err)
				return
			}
		}
	}()
	// Memory must stay bounded.
	var peak uint64
	for ctx.Err() == nil {
		time.Sleep(time.Second)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapInuse)
		if ms.HeapInuse > 1<<30 {
			fail("heap in use %d MB", ms.HeapInuse>>20)
		}
	}
	wg.Wait()
	if f := failed.Load(); f != nil && f.(string) != "" {
		t.Fatal(f)
	}
	var total string
	db.QueryRow("SELECT sum(balance) FROM acct").Scan(&total)
	if total != wantTotal {
		t.Fatalf("final total %s, want %s", total, wantTotal)
	}
	if problems, err := db.IntegrityCheck(); err != nil || len(problems) > 0 {
		t.Fatalf("integrity: %v %v", err, problems)
	}
	st := db.Stats()
	t.Logf("SOAK %v: %d transfers (%.0f/s), %d snapshot reads (%.0f/s), %d busy, %d verified backups, peak heap %d MB, %d commits, %d checkpoints",
		d, transfers.Load(), float64(transfers.Load())/d.Seconds(), reads.Load(), float64(reads.Load())/d.Seconds(),
		busy.Load(), backups.Load(), peak>>20, st.TxCommits, st.Checkpoints)
}

func sizeOf(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// peakRSS logs the process's peak resident memory since the last call and
// resets it (Linux; elsewhere it does nothing).
func peakRSS(t *testing.T) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "VmHWM:" {
			kb, _ := strconv.Atoi(f[1])
			t.Logf("    peak RSS %d MB", kb>>10)
		}
	}
	os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) // reset the peak
}
