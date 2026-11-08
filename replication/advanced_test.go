package replication

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minaryhub/bangdb"
)

func TestReplicaAcksAndLag(t *testing.T) {
	dir := t.TempDir()
	primary := openDB(t, filepath.Join(dir, "p.db"))
	setup(t, primary)
	p, addr := startPrimary(t, primary, PrimaryOptions{})
	rdb := openDB(t, filepath.Join(dir, "r.db"))
	r := StartReplica(rdb, addr, ReplicaOptions{})
	defer r.Stop()
	waitSynced(t, r, primary)
	mustExec(t, primary, "INSERT INTO b (v) VALUES (1)")
	waitSynced(t, r, primary)
	deadline := time.Now().Add(5 * time.Second)
	for {
		infos := p.Replicas()
		if len(infos) == 1 && infos[0].AckSeq == primary.Seq() {
			if infos[0].Lag != 0 || infos[0].LagTime != 0 || infos[0].AckTime.IsZero() {
				t.Fatalf("synced replica: %+v", infos[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no acknowledgement: %+v (primary seq %d)", infos, primary.Seq())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := r.Status(); st.Lag != 0 || st.Term != 0 {
		t.Fatalf("replica status %+v", st)
	}
}

func TestSemiSyncReplication(t *testing.T) {
	dir := t.TempDir()
	primary := openDB(t, filepath.Join(dir, "p.db"))
	setup(t, primary)
	// The timeout is generous so a slow CI machine (Windows fsync) never
	// hits it while the replica is connected.
	const timeout = 2 * time.Second
	p, addr := startPrimary(t, primary, PrimaryOptions{SyncReplicas: 1, SyncTimeout: timeout})
	rdb := openDB(t, filepath.Join(dir, "r.db"))
	r := StartReplica(rdb, addr, ReplicaOptions{})
	waitSynced(t, r, primary)

	// Every commit returns only once the replica has applied it.
	for i := 0; i < 50; i++ {
		mustExec(t, primary, "INSERT INTO b (v) VALUES (?)", i)
		if got, want := rdb.Seq(), primary.Seq(); got != want {
			t.Fatalf("commit %d returned before the replica applied it (replica %d, primary %d)", i, got, want)
		}
	}
	if st := p.Stats(); st.SyncWaits < 50 || st.SyncTimeouts != 0 {
		t.Fatalf("stats %+v", st)
	}

	// Without a replica the commit waits for the timeout, then succeeds.
	r.Stop()
	start := time.Now()
	mustExec(t, primary, "INSERT INTO b (v) VALUES (1000)")
	if d := time.Since(start); d < timeout*9/10 {
		t.Fatalf("commit without replicas returned after %v", d)
	}
	if st := p.Stats(); st.SyncTimeouts != 1 {
		t.Fatalf("stats %+v", st)
	}
	var n int
	primary.QueryRow("SELECT count(*) FROM b WHERE v = 1000").Scan(&n)
	if n != 1 {
		t.Fatal("the timed-out commit was lost")
	}
	p.Close()
	start = time.Now()
	mustExec(t, primary, "INSERT INTO b (v) VALUES (1001)") // no waiting once closed
	if d := time.Since(start); d > timeout/2 {
		t.Fatalf("commit still waits after the primary was closed (%v)", d)
	}
}

func TestFencingAfterFailover(t *testing.T) {
	dir := t.TempDir()
	a := openDB(t, filepath.Join(dir, "a.db"))
	setup(t, a)
	var fenced atomic.Uint64
	_, addrA := startPrimary(t, a, PrimaryOptions{OnFenced: func(term uint64) { fenced.Store(term) }})
	b := openDB(t, filepath.Join(dir, "b.db"))
	rb := StartReplica(b, addrA, ReplicaOptions{})
	waitSynced(t, rb, a)
	c := openDB(t, filepath.Join(dir, "c.db"))
	rc := StartReplica(c, addrA, ReplicaOptions{})
	waitSynced(t, rc, a)
	rc.Stop()

	// Failover: b becomes the primary of term 1; c follows it.
	if err := rb.Promote(); err != nil {
		t.Fatal(err)
	}
	if b.Engine().Term() != 1 || a.Engine().Term() != 0 {
		t.Fatalf("terms: a=%d b=%d", a.Engine().Term(), b.Engine().Term())
	}
	mustExec(t, b, "INSERT INTO b (v) VALUES (424242)")
	_, addrB := startPrimary(t, b, PrimaryOptions{})
	rc = StartReplica(c, addrB, ReplicaOptions{})
	waitSynced(t, rc, b)
	rc.Stop()
	if c.Engine().Term() != 1 {
		t.Fatalf("c has term %d", c.Engine().Term())
	}

	// The old primary is still running. A node of the new term that reaches
	// it fences it: it stops accepting writes.
	mustExec(t, a, "INSERT INTO b (v) VALUES (1)") // not yet fenced
	rc = StartReplica(c, addrA, ReplicaOptions{})
	defer rc.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for fenced.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("old primary was not fenced")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := a.Exec("INSERT INTO b (v) VALUES (2)"); !errors.Is(err, bangdb.ErrReadOnly) {
		t.Fatalf("write on the fenced primary: %v", err)
	}
	for !strings.Contains(rc.Status().LastError, "fenced") {
		if time.Now().After(deadline) {
			t.Fatalf("replica status %+v", rc.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// c did not take anything from the stale primary.
	var n int
	c.QueryRow("SELECT count(*) FROM b WHERE v = 424242").Scan(&n)
	if n != 1 {
		t.Fatal("c lost the new primary's data")
	}
}

func TestArchiveAndPointInTimeRestore(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{9}, 32)
	akey := bytes.Repeat([]byte{5}, 32)
	db, err := bangdb.OpenWithOptions(filepath.Join(dir, "main.db"), &bangdb.Options{EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE ev (id INTEGER PRIMARY KEY, v TEXT)")
	arch := filepath.Join(dir, "archive")
	opts := ArchiveOptions{SegmentBytes: 64 << 10, BaseEvery: -1, SyncInterval: 20 * time.Millisecond, Key: akey}
	a, err := StartArchiver(db, arch, opts)
	if err != nil {
		t.Fatal(err)
	}
	type point struct {
		seq  uint64
		at   time.Time
		rows int
	}
	var points []point
	rows := 0
	for round := 0; round < 6; round++ {
		for i := 0; i < 20; i++ {
			mustExec(t, db, "INSERT INTO ev (v) VALUES (?)", strings.Repeat("x", 100))
			rows++
		}
		points = append(points, point{db.Seq(), time.Now(), rows})
		time.Sleep(15 * time.Millisecond)
		if round == 2 {
			// Stopping and restarting continues the chain without a new base.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := a.WaitFor(ctx, db.Seq()); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := a.Stop(); err != nil {
				t.Fatal(err)
			}
			if a, err = StartArchiver(db, arch, opts); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := a.WaitFor(ctx, db.Seq()); err != nil {
		t.Fatal(err)
	}
	if st := a.Status(); st.Bases != 0 {
		t.Fatalf("restarted archiver took a new base: %+v", st)
	}

	restore := func(name string, o RestoreOptions) (*bangdb.DB, RestoreResult) {
		t.Helper()
		o.Key, o.ArchiveKey = key, akey
		path := filepath.Join(dir, name)
		res, err := Restore(arch, path, o)
		if err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
		rdb, err := bangdb.OpenWithOptions(path, &bangdb.Options{EncryptionKey: key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rdb.Close() })
		return rdb, res
	}
	count := func(db *bangdb.DB) int {
		var n int
		db.QueryRow("SELECT count(*) FROM ev").Scan(&n)
		return n
	}
	for i, p := range points {
		rdb, res := restore("seq"+string(rune('a'+i))+".db", RestoreOptions{Seq: p.seq})
		if count(rdb) != p.rows || res.Seq != p.seq {
			t.Errorf("restore to seq %d: %d rows (want %d), result %+v", p.seq, count(rdb), p.rows, res)
		}
	}
	rdb, _ := restore("time.db", RestoreOptions{Time: points[3].at})
	if count(rdb) != points[3].rows {
		t.Errorf("restore to time: %d rows, want %d", count(rdb), points[3].rows)
	}
	rdb, res := restore("all.db", RestoreOptions{})
	if count(rdb) != rows || res.Seq != db.Seq() {
		t.Errorf("full restore: %d rows, %+v", count(rdb), res)
	}
	// A restored database is a new writable primary.
	mustExec(t, rdb, "INSERT INTO ev (v) VALUES ('after restore')")
	if rdb.Engine().Term() != 1 || rdb.Engine().DBID() == db.Engine().DBID() {
		t.Error("restored database did not start a new timeline")
	}
	if _, err := Restore(arch, filepath.Join(dir, "x.db"), RestoreOptions{Key: key}); err == nil {
		t.Error("restore of an encrypted archive without its key succeeded")
	}
	if _, err := Restore(arch, filepath.Join(dir, "y.db"), RestoreOptions{Key: key, ArchiveKey: akey, Seq: db.Seq() + 100}); err == nil {
		t.Error("restore past the end of the archive succeeded")
	}

	// VACUUM gives the database a new identity: the archiver starts a new
	// chain with a new base, and restores after it use that chain.
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO ev (v) VALUES ('after vacuum')")
	if err := a.WaitFor(ctx, db.Seq()); err != nil {
		dumpGoroutines(t)
		t.Fatalf("archiver after VACUUM: %v (status %+v, db seq %d)", err, a.Status(), db.Seq())
	}
	if st := a.Status(); st.Bases != 1 {
		t.Fatalf("after VACUUM: %+v", st)
	}
	rdb, _ = restore("vac.db", RestoreOptions{})
	if count(rdb) != rows+1 {
		t.Errorf("restore after VACUUM: %d rows", count(rdb))
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
}
