package pager

import "testing"

func TestObjectSlots(t *testing.T) {
	p := OpenMemory()
	v := p.NewView()
	v.SetObj(7, "a")
	if v.Obj(7) != "a" {
		t.Fatal("view does not see its own change")
	}
	if p.NewView().Obj(7) != nil {
		t.Fatal("uncommitted slot visible")
	}
	if err := p.Commit(v); err != nil { // a commit with only slot changes
		t.Fatal(err)
	}
	snap, _ := p.Snapshot()
	defer snap.Release()

	v2 := p.NewView()
	if v2.Obj(7) != "a" {
		t.Fatal("committed slot not visible to the next writer")
	}
	sp := v2.Savepoint()
	tok := v2.Token()
	v2.SetObj(7, "b")
	v2.SetObj(8, "c")
	v2.Restore(sp)
	if v2.Obj(7) != "a" || v2.Obj(8) != nil {
		t.Fatalf("restore: %v %v", v2.Obj(7), v2.Obj(8))
	}
	v2.Savepoint()
	if v2.Token() == tok {
		t.Fatal("savepoint must start a new mutation token")
	}
	v2.SetObj(7, nil)
	v2.SetObj(9, "d")
	if err := p.Commit(v2); err != nil {
		t.Fatal(err)
	}
	if o := snap.View().Obj(7); o != "a" {
		t.Fatalf("snapshot sees %v", o)
	}
	cur, _ := p.Snapshot()
	defer cur.Release()
	if cur.View().Obj(7) != nil || cur.View().Obj(9) != "d" {
		t.Fatal("latest commit's slots")
	}
	// Prepared but not yet flushed: writers see it, readers do not.
	v3 := p.NewView()
	v3.SetObj(9, "e")
	c, err := p.PrepareCommit(v3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewView().Obj(9) != "e" {
		t.Fatal("writer must build on the prepared commit")
	}
	s3, _ := p.Snapshot()
	if s3.View().Obj(9) != "d" {
		t.Fatal("reader saw an unpublished commit")
	}
	s3.Release()
	if err := p.Flush(c); err != nil {
		t.Fatal(err)
	}
	s4, _ := p.Snapshot()
	defer s4.Release()
	if s4.View().Obj(9) != "e" {
		t.Fatal("flushed commit not published")
	}
}
