package pager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func page(b byte) []byte { return bytes.Repeat([]byte{b}, PageSize) }

func TestCommitAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(7))
	v.SetMeta(3, 42)
	if err := p.Commit(v); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	v = p.NewView()
	b, err := v.Read(pg)
	if err != nil || !sameContent(b, 7) || v.Meta(3) != 42 {
		t.Fatalf("data not persisted: %v", err)
	}
}

func TestRecoverFromWALAfterCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	p, _ := Open(path)
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(1))
	if err := p.Commit(v); err != nil {
		t.Fatal(err)
	}
	// Second transaction "crashes" right after the WAL is durable.
	p.failpoint = func(string) error { return errors.New("crash") }
	v = p.NewView()
	v.Write(pg, page(2))
	if err := p.Commit(v); err == nil {
		t.Fatal("expected simulated crash")
	}
	p.Close()

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	b, _ := p.NewView().Read(pg)
	if !sameContent(b, 2) {
		t.Fatal("committed transaction was not recovered from WAL")
	}
	if st, _ := os.Stat(path + "-wal"); st.Size() != 0 {
		t.Fatal("WAL not truncated after recovery")
	}
}

func TestTornWALIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	p, _ := Open(path)
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(1))
	p.Commit(v)
	var wal []byte
	p.failpoint = func(string) error {
		wal, _ = os.ReadFile(path + "-wal")
		return errors.New("crash")
	}
	v = p.NewView()
	v.Write(pg, page(9))
	p.Commit(v)
	p.Close()
	// Simulate a torn write: drop the commit record.
	os.WriteFile(path+"-wal", wal[:len(wal)-6], 0o644)

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	b, _ := p.NewView().Read(pg)
	if !sameContent(b, 1) {
		t.Fatal("torn transaction must not be applied")
	}
}

func TestSavepointAndFreeList(t *testing.T) {
	p := OpenMemory()
	v := p.NewView()
	a, _ := v.Alloc()
	b, _ := v.Alloc()
	sp := v.Savepoint()
	v.Write(a, page(5))
	v.Free(b)
	v.Restore(sp)
	if got, _ := v.Read(a); !sameContent(got, 0) {
		t.Fatal("restore did not undo write")
	}
	v.Free(b)
	c, _ := v.Alloc()
	if c != b {
		t.Fatalf("free page not reused: got %d want %d", c, b)
	}
}

func TestSecondProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected lock error on second open")
	}
}

var errCrash = errors.New("crash")

// sameContent compares a page's content (excluding its checksum) with a page
// filled with b.
func sameContent(got []byte, b byte) bool {
	return bytes.Equal(got[:UsableSize], page(b)[:UsableSize])
}

func TestChecksumDetectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	p, _ := Open(path)
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(7))
	p.Commit(v)
	p.Close()

	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	f.WriteAt([]byte{0x42}, int64(pg)*PageSize+100) // flip a byte on disk
	f.Close()

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.NewView().Read(pg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption not detected: %v", err)
	}
}

// Commits larger than a checkpoint chunk (and spilled ones) are checkpointed
// and recovered in chunks, encrypted or not.
func TestLargeCommitChunkedCheckpointAndRecovery(t *testing.T) {
	for _, key := range [][]byte{nil, bytes.Repeat([]byte{7}, 32)} {
		path := filepath.Join(t.TempDir(), "big.db")
		p, err := OpenEncrypted(path, key)
		if err != nil {
			t.Fatal(err)
		}
		p.SetSpill(t.TempDir(), 100)
		n := checkpointChunk*2 + 123
		write := func(fill byte) {
			v := p.NewView()
			for i := 0; i < n; i++ {
				pg := uint32(i + 1)
				if pg >= v.PageCount() {
					if got, _ := v.Alloc(); got != pg {
						t.Fatalf("alloc %d, want %d", got, pg)
					}
				}
				v.Write(pg, page(fill+byte(i%7)))
				if err := v.SpillPoint(); err != nil {
					t.Fatal(err)
				}
			}
			if v.Spilled() == 0 {
				t.Fatal("no spill")
			}
			if err := p.Commit(v); err != nil {
				t.Fatal(err)
			}
		}
		write(10)
		if err := p.Checkpoint(); err != nil { // chunked checkpoint
			t.Fatal(err)
		}
		// The second large commit "crashes" after its WAL is durable.
		p.failpoint = func(stage string) error {
			if stage == "wal-synced" {
				return nil
			}
			return errors.New("crash")
		}
		write(50)
		p.failpoint = func(string) error { return errors.New("crash") }
		p.Checkpoint() // fails at once: the WAL keeps the commit
		p.mu.Lock()
		p.err = errors.New("crashed") // Close must not checkpoint
		p.mu.Unlock()
		p.Close()

		p, err = OpenEncrypted(path, key) // chunked recovery
		if err != nil {
			t.Fatal(err)
		}
		v := p.NewView()
		for i := 0; i < n; i++ {
			b, err := v.Read(uint32(i + 1))
			if err != nil || !sameContent(b, 50+byte(i%7)) {
				t.Fatalf("page %d after recovery: %v", i+1, err)
			}
		}
		p.Close()
	}
}
