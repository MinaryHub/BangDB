package pager

import (
	"math/rand"
	"path/filepath"
	"testing"
)

// TestSnapshotModel runs random commits, snapshots, checkpoints and reopens
// against a simple model: every snapshot must keep seeing exactly the page
// contents of the commit it was opened at.
func TestSnapshotModel(t *testing.T) {
	for _, mode := range []string{"file", "memory"} {
		t.Run(mode, func(t *testing.T) { runSnapshotModel(t, mode) })
	}
}

func runSnapshotModel(t *testing.T, mode string) {
	path := filepath.Join(t.TempDir(), "m.db")
	open := func() *Pager {
		if mode == "memory" {
			return OpenMemory()
		}
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := open()
	defer func() { p.Close() }()
	rng := rand.New(rand.NewSource(42))

	type state map[uint32]byte
	states := map[uint64]state{p.Header().Seq: {}}
	cur := p.Header().Seq
	type snap struct {
		s   *Snapshot
		seq uint64
	}
	var snaps []snap
	verify := func(sn snap) {
		v := sn.s.View()
		for no, b := range states[sn.seq] {
			got, err := v.Read(no)
			if err != nil {
				t.Fatalf("snapshot %d page %d: %v", sn.seq, no, err)
			}
			if !sameContent(got, b) {
				t.Fatalf("snapshot %d page %d: got %d want %d (latest %d)", sn.seq, no, got[0], b, cur)
			}
		}
	}
	for step := 0; step < 3000; step++ {
		switch r := rng.Intn(100); {
		case r < 50: // commit
			v := p.NewView()
			next := state{}
			for k, b := range states[cur] {
				next[k] = b
			}
			for i := 0; i < 1+rng.Intn(4); i++ {
				var no uint32
				if len(next) == 0 || rng.Intn(5) == 0 {
					var err error
					if no, err = v.Alloc(); err != nil {
						t.Fatal(err)
					}
				} else {
					no = uint32(1 + rng.Intn(len(next)))
				}
				b := byte(rng.Intn(250) + 1)
				v.Write(no, page(b))
				next[no] = b
			}
			if err := p.Commit(v); err != nil {
				t.Fatal(err)
			}
			cur = p.Header().Seq
			states[cur] = next
		case r < 65:
			if len(snaps) < 6 {
				s, err := p.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				snaps = append(snaps, snap{s, s.Seq()})
			}
		case r < 75:
			if len(snaps) > 0 {
				i := rng.Intn(len(snaps))
				verify(snaps[i])
				snaps[i].s.Release()
				snaps = append(snaps[:i], snaps[i+1:]...)
			}
		case r < 90:
			if err := p.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		case r < 97:
			if len(snaps) > 0 {
				verify(snaps[rng.Intn(len(snaps))])
			}
		default:
			if len(snaps) == 0 && mode == "file" {
				p.Close()
				p = open()
				if p.Header().Seq != cur {
					t.Fatalf("reopen: seq %d want %d", p.Header().Seq, cur)
				}
				if p.WALPages() != 0 {
					t.Fatal("WAL not empty after clean close")
				}
			}
		}
	}
	for _, sn := range snaps {
		verify(sn)
		sn.s.Release()
	}
	if err := p.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if p.WALPages() != 0 {
		t.Fatalf("%d WAL pages left after checkpoint without readers", p.WALPages())
	}
	s, _ := p.Snapshot()
	verify(snap{s, cur})
	s.Release()
}

// TestCheckpointRespectsReaders checks that a checkpoint never overwrites a
// page an open snapshot still needs, and that recovery after a crash with a
// partly checkpointed WAL is correct.
func TestCheckpointRespectsReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	p, _ := Open(path)
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(1))
	p.Commit(v)

	old, _ := p.Snapshot()
	for b := byte(2); b <= 5; b++ {
		v = p.NewView()
		v.Write(pg, page(b))
		p.Commit(v)
		p.Checkpoint()
	}
	if got, _ := old.View().Read(pg); got[0] != 1 {
		t.Fatalf("old snapshot sees %d", got[0])
	}
	if p.WALPages() == 0 {
		t.Fatal("checkpoint ran past an open snapshot")
	}
	old.Release()
	p.Checkpoint()
	if p.WALPages() != 0 {
		t.Fatal("WAL not reset after the snapshot was released")
	}

	// Crash with some commits only in the WAL.
	v = p.NewView()
	v.Write(pg, page(6))
	p.Commit(v)
	s, _ := p.Snapshot() // blocks checkpointing of the next commits
	v = p.NewView()
	v.Write(pg, page(7))
	p.Commit(v)
	p.Checkpoint()
	_ = s
	p.failpoint = func(string) error { return errCrash }
	v = p.NewView()
	v.Write(pg, page(8))
	p.Commit(v) // durable in WAL, then "crash"
	p.closeFiles()

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got, _ := p.NewView().Read(pg)
	if got[0] != 8 {
		t.Fatalf("after recovery page = %d, want 8", got[0])
	}
}

// TestCrashDuringCheckpoint kills a checkpoint halfway through writing pages
// into the database file; recovery must still produce the latest state.
// (Regression: the header used to be written first, so recovery skipped the
// commits whose pages had not been written yet.)
func TestCrashDuringCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	p, _ := Open(path)
	want := map[uint32]byte{}
	for c := 0; c < 20; c++ {
		v := p.NewView()
		for i := 0; i < 10; i++ {
			var no uint32
			if c == 0 {
				no, _ = v.Alloc()
			} else {
				no = uint32(1 + (c*7+i*3)%10)
			}
			b := byte(c*10 + i + 1)
			v.Write(no, page(b))
			want[no] = b
		}
		if err := p.Commit(v); err != nil {
			t.Fatal(err)
		}
	}
	p.failpoint = func(stage string) error {
		if stage == "checkpoint-write" {
			return errCrash
		}
		return nil
	}
	if err := p.Checkpoint(); err == nil {
		t.Fatal("expected simulated crash")
	}
	p.closeFiles() // crash: no clean shutdown

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	v := p.NewView()
	for no, b := range want {
		got, err := v.Read(no)
		if err != nil {
			t.Fatalf("page %d after recovery: %v", no, err)
		}
		if !sameContent(got, b) {
			t.Fatalf("page %d after recovery holds %d, want %d", no, got[0], b)
		}
	}
}

func TestGroupCommitVisibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.db")
	p, _ := Open(path)
	defer p.Close()
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(1))
	p.Commit(v)

	// c1 is prepared but not flushed: the next writer builds on it, readers
	// do not see it.
	v = p.NewView()
	v.Write(pg, page(2))
	c1, err := p.PrepareCommit(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := p.NewView().Read(pg); !sameContent(got, 2) {
		t.Fatal("writer does not see the pending commit")
	}
	s, _ := p.Snapshot()
	if got, _ := s.View().Read(pg); !sameContent(got, 1) {
		t.Fatal("reader sees a commit that is not durable yet")
	}
	s.Release()
	v = p.NewView()
	v.Write(pg, page(3))
	c2, _ := p.PrepareCommit(v, nil)

	// Flushing c2 publishes c1 and c2 together, in order.
	var published []uint64
	p.SetOnCommit(func(seq uint64, _ map[uint32][]byte) { published = append(published, seq) })
	if err := p.Flush(c2); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(c1); err != nil { // already published: no-op
		t.Fatal(err)
	}
	if len(published) != 2 || published[0] != c1.Seq() || published[1] != c2.Seq() {
		t.Fatalf("published %v", published)
	}
	s, _ = p.Snapshot()
	defer s.Release()
	if got, _ := s.View().Read(pg); !sameContent(got, 3) {
		t.Fatal("reader does not see the flushed commits")
	}
}

func TestGroupCommitSyncFailure(t *testing.T) {
	p, _ := Open(filepath.Join(t.TempDir(), "f.db"))
	defer p.Close()
	v := p.NewView()
	pg, _ := v.Alloc()
	v.Write(pg, page(1))
	p.Commit(v)
	before := p.Header().Seq

	v = p.NewView()
	v.Write(pg, page(2))
	c1, _ := p.PrepareCommit(v, nil)
	v = p.NewView()
	v.Write(pg, page(3))
	c2, _ := p.PrepareCommit(v, nil)
	p.failpoint = func(string) error { return errCrash }
	if err := p.Flush(c1); err == nil {
		t.Fatal("expected sync failure")
	}
	if err := p.Flush(c2); err == nil {
		t.Fatal("a commit built on a failed one must fail too")
	}
	if p.Header().Seq != before {
		t.Fatal("nothing may be published after a failed sync")
	}
}
