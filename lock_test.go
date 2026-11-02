package bangdb

import (
	"errors"
	"path/filepath"
	"testing"
)

// Only one handle may have a database file open: a second Open fails
// instead of corrupting the file, and succeeds once the first is closed.
func TestFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, a, "CREATE TABLE t (x)")
	if b, err := Open(path); !errors.Is(err, ErrLocked) {
		if err == nil {
			b.Close()
		}
		t.Fatalf("second Open: %v, want ErrLocked", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	exec(t, b, "INSERT INTO t VALUES (1)")
	b.Close()
}
