package store

import (
	"bytes"
	"cmp"

	"github.com/knoxxr/bangdb/internal/memtree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/value"
)

func init() { Register(MemoryKind{}) }

// MemoryKind keeps rows and index entries as Go values in persistent
// B+trees (package memtree) instead of encoded pages: no page encoding or
// decoding, and no WAL writes for the data. Each store's tree lives in the
// pager object slot of its root page, so transactions, snapshots and
// savepoints work as for pages. The data is not durable: the table (and its
// schema) survives a restart, but comes back empty. It is not part of
// backups or replication either.
type MemoryKind struct{}

func (MemoryKind) Name() string   { return "memory" }
func (MemoryKind) Volatile() bool { return true }

// The root is an allocated (empty) page, which gives the store an identity
// that the catalog and page accounting already understand.
func (MemoryKind) Create(v *pager.View) (uint32, error) { return v.Alloc() }

func (MemoryKind) Drop(v *pager.View, root uint32) error {
	v.SetObj(root, nil)
	v.Free(root)
	return nil
}

func (k MemoryKind) CreateIndex(v *pager.View) (uint32, error)  { return k.Create(v) }
func (k MemoryKind) DropIndex(v *pager.View, root uint32) error { return k.Drop(v, root) }

func (MemoryKind) Open(v *pager.View, root uint32) TableStore { return memTable{v, root} }

func (MemoryKind) OpenIndex(v *pager.View, root uint32) IndexStore { return memIndex{v, root} }

// Rows are kept as encoded row records: byte slices hold no pointers, so
// the garbage collector does not have to trace every value of every row.
type rowTree = memtree.Tree[int64, []byte]

// memTable reads the tree from the view on every call, so that several
// handles on the same table in one view always agree.
type memTable struct {
	v    *pager.View
	root uint32
}

func (s memTable) tree() rowTree {
	if o := s.v.Obj(s.root); o != nil {
		return *o.(*rowTree)
	}
	return memtree.New[int64, []byte](cmp.Compare[int64])
}

func (s memTable) set(t rowTree) { s.v.SetObj(s.root, &t) }

func (s memTable) Get(rowid int64) ([]any, bool, error) {
	data, ok := s.tree().Get(rowid)
	if !ok {
		return nil, false, nil
	}
	row, err := value.DecodeRow(data)
	return row, err == nil, err
}

func (s memTable) Put(rowid int64, row []any) error {
	s.set(s.tree().Put(s.v.Token(), rowid, value.EncodeRow(row)))
	return nil
}

func (s memTable) Delete(rowid int64) (bool, error) {
	t, ok := s.tree().Delete(s.v.Token(), rowid)
	if ok {
		s.set(t)
	}
	return ok, nil
}

func (s memTable) Scan(lo, hi int64, desc bool, fn func(int64, []any) (bool, error)) error {
	if lo > hi {
		return nil
	}
	t := s.tree()
	if desc {
		for it := t.SeekReverse(hi); it.Valid() && it.Key() >= lo; it.Next() {
			if cont, err := fn(it.Key(), must(value.DecodeRow(it.Value()))); err != nil || !cont {
				return err
			}
		}
		return nil
	}
	for it := t.Seek(lo); it.Valid() && it.Key() <= hi; it.Next() {
		if cont, err := fn(it.Key(), must(value.DecodeRow(it.Value()))); err != nil || !cont {
			return err
		}
	}
	return nil
}

func (s memTable) Last() (int64, bool, error) {
	k, ok := s.tree().Last()
	return k, ok, nil
}

func (s memTable) EstimateRows() (int64, error) { return int64(s.tree().Len()), nil }

func must(row []any, err error) []any {
	if err != nil {
		panic(err) // rows are encoded by Put
	}
	return row
}

type keyTree = memtree.Tree[[]byte, struct{}]

type memIndex struct {
	v    *pager.View
	root uint32
}

func (s memIndex) tree() keyTree {
	if o := s.v.Obj(s.root); o != nil {
		return *o.(*keyTree)
	}
	return memtree.New[[]byte, struct{}](bytes.Compare)
}

func (s memIndex) set(t keyTree) { s.v.SetObj(s.root, &t) }

func (s memIndex) Put(key []byte) error {
	s.set(s.tree().Put(s.v.Token(), bytes.Clone(key), struct{}{}))
	return nil
}

func (s memIndex) Delete(key []byte) (bool, error) {
	t, ok := s.tree().Delete(s.v.Token(), key)
	if ok {
		s.set(t)
	}
	return ok, nil
}

func (s memIndex) Seek(start []byte) Iter {
	if start == nil {
		return memIter{s.tree().First()}
	}
	return memIter{s.tree().Seek(start)}
}

func (s memIndex) SeekReverse(upper []byte) Iter {
	if upper == nil {
		return memIter{s.tree().LastIter()}
	}
	return memIter{s.tree().SeekReverse(upper)}
}

func (s memIndex) EstimateRows() (int64, error) { return int64(s.tree().Len()), nil }

type memIter struct {
	*memtree.Iter[[]byte, struct{}]
}

func (memIter) Err() error { return nil }
