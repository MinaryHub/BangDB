package replication

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/knoxxr/bangdb"
)

// Encryption is a property of each file: an encrypted primary can feed a
// replica with another key (or none), through snapshot, streaming, restart
// and resynchronization.
func TestEncryptedReplication(t *testing.T) {
	dir := t.TempDir()
	open := func(name string, key []byte) *bangdb.DB {
		db, err := bangdb.OpenWithOptions(filepath.Join(dir, name), &bangdb.Options{EncryptionKey: key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	pkey, rkey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	primary := open("p.db", pkey)
	setup(t, primary)
	_, addr := startPrimary(t, primary, PrimaryOptions{LogSize: 1})

	enc := open("r1.db", rkey)
	plain := open("r2.db", nil)
	r1 := StartReplica(enc, addr, ReplicaOptions{})
	r2 := StartReplica(plain, addr, ReplicaOptions{})
	stop := writer(t, primary)
	time.Sleep(100 * time.Millisecond)
	stop()
	waitSynced(t, r1, primary)
	waitSynced(t, r2, primary)
	want := dump(t, primary)
	if dump(t, enc) != want || dump(t, plain) != want {
		t.Fatal("replicas differ from the primary")
	}
	r1.Stop()
	r2.Stop()
	enc.Close()
	plain.Close()

	// Each replica file keeps its own key.
	if _, err := bangdb.OpenWithOptions(filepath.Join(dir, "r1.db"), &bangdb.Options{EncryptionKey: pkey}); err == nil {
		t.Fatal("replica opened with the primary's key")
	}
	if db, err := bangdb.Open(filepath.Join(dir, "r2.db")); err != nil {
		t.Fatalf("plaintext replica: %v", err)
	} else {
		db.Close()
	}

	// Resynchronization (the tiny log forces a snapshot) keeps the key.
	for i := 0; i < 10; i++ {
		mustExec(t, primary, `INSERT INTO b (v) VALUES (?)`, 5000+i)
	}
	enc = open("r1.db", rkey)
	r1 = StartReplica(enc, addr, ReplicaOptions{})
	defer r1.Stop()
	waitSynced(t, r1, primary)
	if r1.Status().Snapshots != 1 || dump(t, enc) != dump(t, primary) {
		t.Fatalf("resync: %+v", r1.Status())
	}
	r1.Stop()
	enc.Close()
	if db, err := bangdb.OpenWithOptions(filepath.Join(dir, "r1.db"), &bangdb.Options{EncryptionKey: rkey}); err != nil {
		t.Fatalf("reopen after resync: %v", err)
	} else {
		db.Close()
	}
}
