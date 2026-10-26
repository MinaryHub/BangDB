package store

import (
	"bytes"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/value"
)

func init() { Register(RowKind{}) }

// RowKind is the default kind: a B+tree keyed by the 8-byte rowid encoding
// with row records as values. Its name is "" (shown as "btree").
type RowKind struct{}

func (RowKind) Name() string { return "" }

func (RowKind) Create(v *pager.View) (uint32, error) { return btree.Create(v) }

func (RowKind) Open(v *pager.View, root uint32) TableStore { return &rowStore{btree.Open(v, root)} }

func (RowKind) Drop(v *pager.View, root uint32) error { return btree.Drop(v, root) }

type rowStore struct{ t *btree.Tree }

func (s *rowStore) Get(rowid int64) ([]any, bool, error) {
	data, ok, err := s.t.Get(value.EncodeRowID(rowid))
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := value.DecodeRow(data)
	return row, err == nil, err
}

func (s *rowStore) Put(rowid int64, row []any) error {
	return s.t.Put(value.EncodeRowID(rowid), value.EncodeRow(row))
}

func (s *rowStore) Delete(rowid int64) (bool, error) {
	return s.t.Delete(value.EncodeRowID(rowid))
}

func (s *rowStore) Scan(lo, hi int64, desc bool, fn func(int64, []any) (bool, error)) error {
	if lo > hi {
		return nil
	}
	loKey, hiKey := value.EncodeRowID(lo), value.EncodeRowID(hi)
	visit := func(key []byte, val func() ([]byte, error)) (bool, error) {
		data, err := val()
		if err != nil {
			return false, err
		}
		row, err := value.DecodeRow(data)
		if err != nil {
			return false, err
		}
		return fn(value.DecodeRowID(key), row)
	}
	if desc {
		c := s.t.SeekReverse(hiKey)
		for ; c.Valid() && bytes.Compare(c.Key(), loKey) >= 0; c.Prev() {
			if cont, err := visit(c.Key(), c.Value); err != nil || !cont {
				return err
			}
		}
		return c.Err()
	}
	c := s.t.Seek(loKey)
	for ; c.Valid() && bytes.Compare(c.Key(), hiKey) <= 0; c.Next() {
		if cont, err := visit(c.Key(), c.Value); err != nil || !cont {
			return err
		}
	}
	return c.Err()
}

func (s *rowStore) Last() (int64, bool, error) {
	k, ok, err := s.t.Last()
	if err != nil || !ok {
		return 0, false, err
	}
	return value.DecodeRowID(k), true, nil
}

func (s *rowStore) EstimateRows() (int64, error) { return s.t.Estimate() }

// Index stores are always B+trees (the executor relies on byte-ordered
// prefix scans).

// CreateIndex allocates an empty index store.
func CreateIndex(v *pager.View) (uint32, error) { return btree.Create(v) }

// OpenIndex returns the index store rooted at root.
func OpenIndex(v *pager.View, root uint32) IndexStore { return &indexStore{btree.Open(v, root)} }

// DropIndex frees an index store.
func DropIndex(v *pager.View, root uint32) error { return btree.Drop(v, root) }

type indexStore struct{ t *btree.Tree }

func (s *indexStore) Put(key []byte) error            { return s.t.Put(key, nil) }
func (s *indexStore) Delete(key []byte) (bool, error) { return s.t.Delete(key) }
func (s *indexStore) Seek(start []byte) Iter          { return s.t.Seek(start) }
func (s *indexStore) SeekReverse(upper []byte) Iter   { return revIter{s.t.SeekReverse(upper)} }
func (s *indexStore) EstimateRows() (int64, error)    { return s.t.Estimate() }

type revIter struct{ *btree.RevCursor }

func (r revIter) Next() { r.Prev() }
