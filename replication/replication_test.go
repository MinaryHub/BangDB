package replication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minaryhub/bangdb"
	"github.com/minaryhub/bangdb/internal/value"
)

func openDB(t *testing.T, path string) *bangdb.DB {
	t.Helper()
	db, err := bangdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dump renders every table so that two databases can be compared.
func dump(t *testing.T, db *bangdb.DB) string {
	t.Helper()
	if problems, err := db.IntegrityCheck(); err != nil || len(problems) > 0 {
		t.Fatalf("integrity check: %v %v", err, problems)
	}
	names := db.Tables()
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		rows, err := db.Query("SELECT rowid, * FROM " + n + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(n + ":")
		for rows.Next() {
			for _, v := range rows.Values() {
				sb.WriteString(value.Format(v) + ",")
			}
			sb.WriteString(";")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func mustExec(t *testing.T, db *bangdb.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func startPrimary(t *testing.T, db *bangdb.DB, opts PrimaryOptions) (*Primary, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := NewPrimary(db, opts)
	go p.Serve(ln)
	t.Cleanup(func() { p.Close() })
	return p, ln.Addr().String()
}

func waitSynced(t *testing.T, r *Replica, primary *bangdb.DB) {
	t.Helper()
	// Generous: fsyncs on some CI disks (Windows) take tens of ms each.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.WaitFor(ctx, primary.Seq()); err != nil {
		dumpGoroutines(t)
		t.Fatalf("replica did not catch up (status %+v, primary seq %d): %v", r.Status(), primary.Seq(), err)
	}
}

// dumpGoroutines logs every goroutine's stack, to see what a stuck test
// waits for.
func dumpGoroutines(t *testing.T) {
	var buf bytes.Buffer
	pprof.Lookup("goroutine").WriteTo(&buf, 2)
	t.Log(buf.String())
}

// writer inserts rows in two tables per transaction until stopped, so any
// consistent copy has equal row counts in both tables.
func writer(t *testing.T, db *bangdb.DB) (stop func() int) {
	var n atomic.Int64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			err := db.Update(func(tx *bangdb.Tx) error {
				if _, err := tx.Exec(`INSERT INTO a (v, pad) VALUES (?, ?)`, i, strings.Repeat("x", i%900)); err != nil {
					return err
				}
				_, err := tx.Exec(`INSERT INTO b (v) VALUES (?)`, i)
				return err
			})
			if err != nil {
				t.Error(err)
				return
			}
			n.Add(1)
		}
	}()
	return func() int { close(done); wg.Wait(); return int(n.Load()) }
}

func setup(t *testing.T, db *bangdb.DB) {
	mustExec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, v INT, pad TEXT); CREATE INDEX a_v ON a (v);
		CREATE TABLE b (id INTEGER PRIMARY KEY, v INT UNIQUE)`)
	err := db.Update(func(tx *bangdb.Tx) error {
		for i := 0; i < 3000; i++ {
			if _, err := tx.Exec(`INSERT INTO a (v, pad) VALUES (?, ?)`, -i-1, strings.Repeat("y", 200)); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO b (v) VALUES (?)`, -i-1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkConsistent(t *testing.T, db *bangdb.DB) {
	t.Helper()
	var na, nb, nIdx int
	db.QueryRow(`SELECT COUNT(*) FROM a`).Scan(&na)
	db.QueryRow(`SELECT COUNT(*) FROM b`).Scan(&nb)
	db.QueryRow(`SELECT COUNT(*) FROM a WHERE v >= -100000`).Scan(&nIdx) // via index a_v
	if na != nb || na != nIdx {
		t.Fatalf("inconsistent copy: a=%d b=%d a(index)=%d", na, nb, nIdx)
	}
}

func TestOnlineBackupUnderLoad(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "main.db"))
	setup(t, db)
	stop := writer(t, db)
	time.Sleep(50 * time.Millisecond)
	backupPath := filepath.Join(dir, "backup.db")
	if err := db.Backup(backupPath); err != nil {
		t.Fatal(err)
	}
	written := stop()
	if written == 0 {
		t.Fatal("writer made no progress during backup")
	}
	b := openDB(t, backupPath)
	checkConsistent(t, b)
	// The backup is a normal, writable database.
	mustExec(t, b, `INSERT INTO b (v) VALUES (123456789)`)

	// In-memory databases can be backed up to a file too.
	mem := openDB(t, ":memory:")
	setup(t, mem)
	if err := mem.Backup(filepath.Join(dir, "mem.db")); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, openDB(t, filepath.Join(dir, "mem.db"))), dump(t, mem); got != want {
		t.Fatal("memory backup differs")
	}
}

func TestReplicaSnapshotThenStream(t *testing.T) {
	dir := t.TempDir()
	primary := openDB(t, filepath.Join(dir, "p.db"))
	setup(t, primary)
	_, addr := startPrimary(t, primary, PrimaryOptions{Token: "s3cret"})

	replicaDB := openDB(t, filepath.Join(dir, "r.db"))
	stop := writer(t, primary) // writes continue during the snapshot
	r := StartReplica(replicaDB, addr, ReplicaOptions{Token: "s3cret"})
	time.Sleep(200 * time.Millisecond)
	stop()
	waitSynced(t, r, primary)
	if got, want := dump(t, replicaDB), dump(t, primary); got != want {
		t.Fatal("replica differs from primary after snapshot + stream")
	}
	if st := r.Status(); st.Snapshots != 1 || !st.Connected {
		t.Fatalf("status %+v", st)
	}

	// Mirroring: new commits, including DDL, flow to the replica.
	mustExec(t, primary, `CREATE TABLE c (id INTEGER PRIMARY KEY, name TEXT)`)
	mustExec(t, primary, `INSERT INTO c (name) VALUES ('mirrored')`)
	mustExec(t, primary, `DELETE FROM a WHERE v < -2000`)
	waitSynced(t, r, primary)
	var name string
	if err := replicaDB.QueryRow(`SELECT name FROM c`).Scan(&name); err != nil || name != "mirrored" {
		t.Fatalf("mirrored row: %q %v", name, err)
	}
	if got, want := dump(t, replicaDB), dump(t, primary); got != want {
		t.Fatal("replica differs after streaming")
	}

	// Replicas are read-only.
	if _, err := replicaDB.Exec(`INSERT INTO c (name) VALUES ('x')`); !errors.Is(err, bangdb.ErrReadOnly) {
		t.Fatalf("write on replica: %v", err)
	}

	// Reconnect after downtime: catches up from the log, no new snapshot.
	r.Stop()
	mustExec(t, primary, `INSERT INTO c (name) VALUES ('while-away')`)
	r2 := StartReplica(replicaDB, addr, ReplicaOptions{Token: "s3cret"})
	defer r2.Stop()
	waitSynced(t, r2, primary)
	if st := r2.Status(); st.Snapshots != 0 {
		t.Fatalf("reconnect needed a snapshot: %+v", st)
	}
	if got, want := dump(t, replicaDB), dump(t, primary); got != want {
		t.Fatal("replica differs after reconnect")
	}
}

func TestReplicaSurvivesRestartAndLag(t *testing.T) {
	dir := t.TempDir()
	primary := openDB(t, filepath.Join(dir, "p.db"))
	setup(t, primary)
	// Tiny log: a replica that misses many commits must fall back to a snapshot.
	_, addr := startPrimary(t, primary, PrimaryOptions{LogSize: 1})

	rpath := filepath.Join(dir, "r.db")
	rdb, err := bangdb.Open(rpath)
	if err != nil {
		t.Fatal(err)
	}
	r := StartReplica(rdb, addr, ReplicaOptions{})
	waitSynced(t, r, primary)
	r.Stop()
	rdb.Close()

	for i := 0; i < 20; i++ {
		mustExec(t, primary, `INSERT INTO b (v) VALUES (?)`, 1000+i)
	}

	// The replica file is a durable copy: reopen it and resume.
	rdb = openDB(t, rpath)
	before := rdb.Seq()
	r = StartReplica(rdb, addr, ReplicaOptions{})
	defer r.Stop()
	waitSynced(t, r, primary)
	if before == 0 || r.Status().Snapshots != 1 {
		t.Fatalf("expected resume via snapshot from seq %d, status %+v", before, r.Status())
	}
	if got, want := dump(t, rdb), dump(t, primary); got != want {
		t.Fatal("replica differs after lag recovery")
	}
}

func TestMemoryReplicaAndPromote(t *testing.T) {
	primary := openDB(t, ":memory:")
	setup(t, primary)
	_, addr := startPrimary(t, primary, PrimaryOptions{})
	replicaDB := openDB(t, ":memory:")
	r := StartReplica(replicaDB, addr, ReplicaOptions{})
	waitSynced(t, r, primary)
	if got, want := dump(t, replicaDB), dump(t, primary); got != want {
		t.Fatal("memory replica differs")
	}
	oldID := replicaDB.Engine().DBID()
	if err := r.Promote(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, replicaDB, `INSERT INTO b (v) VALUES (424242)`)
	if replicaDB.Engine().DBID() == oldID {
		t.Fatal("promotion must assign a new identity")
	}

	// A replica of the promoted node starts from a snapshot.
	_, addr2 := startPrimary(t, replicaDB, PrimaryOptions{})
	second := openDB(t, ":memory:")
	r2 := StartReplica(second, addr2, ReplicaOptions{})
	defer r2.Stop()
	waitSynced(t, r2, replicaDB)
	if got, want := dump(t, second), dump(t, replicaDB); got != want {
		t.Fatal("cascaded replica differs")
	}
}

func TestReplicaRejectsBadToken(t *testing.T) {
	primary := openDB(t, ":memory:")
	_, addr := startPrimary(t, primary, PrimaryOptions{Token: "right"})
	r := StartReplica(openDB(t, ":memory:"), addr, ReplicaOptions{Token: "wrong"})
	defer r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.Status().LastError, "token") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected token error, status %+v", r.Status())
}

func TestMultipleReplicasReadScaleOut(t *testing.T) {
	primary := openDB(t, ":memory:")
	setup(t, primary)
	p, addr := startPrimary(t, primary, PrimaryOptions{})
	var replicas []*bangdb.DB
	var rs []*Replica
	for i := 0; i < 3; i++ {
		db := openDB(t, ":memory:")
		replicas = append(replicas, db)
		rs = append(rs, StartReplica(db, addr, ReplicaOptions{}))
	}
	stop := writer(t, primary)
	time.Sleep(100 * time.Millisecond)
	stop()
	want := dump(t, primary)
	for i, r := range rs {
		waitSynced(t, r, primary)
		if dump(t, replicas[i]) != want {
			t.Fatalf("replica %d differs", i)
		}
		r.Stop()
	}
	_ = p
	_ = fmt.Sprint
}

func TestCascadeAndBackupFromReplica(t *testing.T) {
	dir := t.TempDir()
	primary := openDB(t, ":memory:")
	setup(t, primary)
	_, addr := startPrimary(t, primary, PrimaryOptions{})

	mid := openDB(t, filepath.Join(dir, "mid.db"))
	r1 := StartReplica(mid, addr, ReplicaOptions{})
	defer r1.Stop()
	waitSynced(t, r1, primary)
	// The replica serves its own replicas (cascade) without being promoted.
	_, midAddr := startPrimary(t, mid, PrimaryOptions{})
	leaf := openDB(t, ":memory:")
	r2 := StartReplica(leaf, midAddr, ReplicaOptions{})
	defer r2.Stop()

	stop := writer(t, primary)
	// Backups can be taken on a replica while it is applying commits.
	if err := mid.Backup(filepath.Join(dir, "from-replica.db")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	stop()
	waitSynced(t, r1, primary)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r2.WaitFor(ctx, primary.Seq()); err != nil {
		t.Fatalf("leaf did not catch up: leaf %+v, mid %+v (mid seq %d, stats %+v), primary seq %d: %v",
			r2.Status(), r1.Status(), mid.Seq(), mid.Stats(), primary.Seq(), err)
	}
	want := dump(t, primary)
	if dump(t, mid) != want || dump(t, leaf) != want {
		t.Fatal("cascaded replicas differ from primary")
	}
	checkConsistent(t, openDB(t, filepath.Join(dir, "from-replica.db")))
}

func TestReplicaResyncsAfterVacuum(t *testing.T) {
	dir := t.TempDir()
	primary := openDB(t, filepath.Join(dir, "p.db"))
	setup(t, primary)
	_, addr := startPrimary(t, primary, PrimaryOptions{})
	replicaDB := openDB(t, filepath.Join(dir, "r.db"))
	r := StartReplica(replicaDB, addr, ReplicaOptions{})
	defer r.Stop()
	waitSynced(t, r, primary)

	mustExec(t, primary, `DELETE FROM a WHERE v % 3 <> 0; DELETE FROM b WHERE v % 3 <> 0`)
	if err := primary.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, primary, `INSERT INTO a (v) VALUES (777); INSERT INTO b (v) VALUES (777)`)
	waitSynced(t, r, primary)
	if got, want := dump(t, replicaDB), dump(t, primary); got != want {
		t.Fatal("replica differs after the primary was vacuumed")
	}
	if replicaDB.Engine().DBID() != primary.Engine().DBID() {
		t.Fatal("replica did not take the new identity")
	}
	checkConsistent(t, replicaDB)
}

// Memory tables are not replicated: the replica has the table (schema is
// in pages) but no rows, and durable tables are unaffected.
func TestMemoryTablesAreNotReplicated(t *testing.T) {
	primary := openDB(t, ":memory:")
	setup(t, primary)
	mustExec(t, primary, `CREATE TABLE cache (k TEXT PRIMARY KEY, v TEXT) USING memory`)
	mustExec(t, primary, `INSERT INTO cache VALUES ('a', '1'), ('b', '2')`)
	_, addr := startPrimary(t, primary, PrimaryOptions{})
	replicaDB := openDB(t, filepath.Join(t.TempDir(), "r.db"))
	r := StartReplica(replicaDB, addr, ReplicaOptions{})
	defer r.Stop()
	waitSynced(t, r, primary)
	mustExec(t, primary, `INSERT INTO cache VALUES ('c', '3'); INSERT INTO a (v) VALUES (99); INSERT INTO b (v) VALUES (99)`)
	waitSynced(t, r, primary)
	var n int
	if err := replicaDB.QueryRow(`SELECT count(*) FROM cache`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("replica memory table: %d rows, %v", n, err)
	}
	if err := replicaDB.QueryRow(`SELECT count(*) FROM b WHERE v = 99`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("durable table not replicated: %d %v", n, err)
	}
	checkConsistent(t, replicaDB)
}
