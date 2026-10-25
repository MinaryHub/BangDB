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
	if err != nil || !bytes.Equal(b, page(7)) || v.Meta(3) != 42 {
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
	if !bytes.Equal(b, page(2)) {
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
	if !bytes.Equal(b, page(1)) {
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
	if got, _ := v.Read(a); !bytes.Equal(got, make([]byte, PageSize)) {
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
