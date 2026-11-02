package bangdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScheduledBackups(t *testing.T) {
	durableOnly(t)
	dir := t.TempDir()
	key := bytes.Repeat([]byte{5}, 32)
	db, err := OpenWithOptions(filepath.Join(dir, "app.db"), &Options{EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "CREATE TABLE t (x INT); INSERT INTO t VALUES (1)")
	var mu sync.Mutex
	var made []string
	var errs []error
	stop, err := db.StartBackups(BackupSchedule{Dir: filepath.Join(dir, "bk"), Interval: 1100 * time.Millisecond, Keep: 2,
		OnBackup: func(p string, _ time.Duration) { mu.Lock(); made = append(made, p); mu.Unlock() },
		OnError:  func(e error) { mu.Lock(); errs = append(errs, e); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(made)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		exec(t, db, "INSERT INTO t VALUES (2)")
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(made) < 3 || len(errs) > 0 {
		t.Fatalf("backups %v, errors %v", made, errs)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "bk", "app-*.db"))
	if len(files) != 2 {
		t.Fatalf("kept %v, want the newest 2", files)
	}
	if err := db.VerifyBackup(context.Background(), files[1]); err != nil {
		t.Fatal(err)
	}
	// A damaged copy is reported.
	data, _ := os.ReadFile(files[1])
	data[4096*2+100] ^= 0xFF
	bad := filepath.Join(dir, "bad.db")
	os.WriteFile(bad, data, 0o644)
	if err := db.VerifyBackup(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "bad.db") {
		t.Fatalf("damaged backup: %v", err)
	}
}
