package engine

import (
	"context"
	"sort"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
)

// vacuumBatch is the number of dirty pages after which VACUUM commits the
// copy it is building, bounding its memory use.
const vacuumBatch = 4096

// Vacuum rebuilds the database into a new, densely packed file and replaces
// the old one, returning free pages to the file system. Writers wait while
// it runs; readers continue on the old data until the swap, which waits for
// running queries to finish. The rebuilt database gets a new identity, so
// replicas re-synchronize from a fresh snapshot.
func (db *DB) Vacuum(ctx context.Context) error {
	if db.readOnly.Load() {
		return ErrReadOnly
	}
	if err := db.lockWrite(ctx); err != nil {
		return err
	}
	defer db.unlockWrite()
	if db.isClosed() {
		return pager.ErrClosed
	}
	if err := db.p.FlushAll(); err != nil {
		return err
	}
	w, err := db.NewSnapshotWriter(db.path)
	if err != nil {
		return err
	}
	if err := db.rebuild(ctx, w.p); err != nil {
		w.Abort()
		return err
	}
	return db.installLocked(w)
}

// rebuild copies every tree of the latest commit into the empty pager np.
func (db *DB) rebuild(ctx context.Context, np *pager.Pager) error {
	snap, err := db.p.Snapshot()
	if err != nil {
		return err
	}
	defer snap.Release()
	ov := snap.View()
	cat := snap.Meta().(*Catalog).clone()

	nv := np.NewView()
	copyTree := func(root uint32) (uint32, error) {
		b := btree.NewBuilder(nv)
		c := btree.Open(ov, root).Seek(nil)
		for n := 0; c.Valid(); c.Next() {
			if n++; n&255 == 0 && ctx.Err() != nil {
				return 0, interrupted(ctx.Err())
			}
			val, err := c.Value()
			if err != nil {
				return 0, err
			}
			if err := b.Add(c.Key(), val); err != nil {
				return 0, err
			}
			if nv.Dirty() >= vacuumBatch {
				if err := np.Commit(nv); err != nil {
					return 0, err
				}
				nv = np.NewView()
				b.SetView(nv)
			}
		}
		if err := c.Err(); err != nil {
			return 0, err
		}
		return b.Finish()
	}

	// Tables and indexes, in a stable order.
	var names []string
	for k := range cat.tables {
		names = append(names, k)
	}
	sort.Strings(names)
	// A volatile store's data is a committed (immutable) object: it moves
	// to a new root page and the new database shares it.
	copyStore := func(t *Table, root uint32) (uint32, error) {
		if !t.kind.Volatile() {
			return copyTree(root)
		}
		nr, err := nv.Alloc()
		if err == nil {
			nv.SetObj(nr, ov.Obj(root))
		}
		return nr, err
	}
	for _, k := range names {
		t := cat.tables[k]
		if t.Root, err = copyStore(t, t.Root); err != nil {
			return err
		}
		for _, ix := range t.indexes {
			if ix.Root, err = copyStore(t, ix.Root); err != nil {
				return err
			}
		}
	}
	// System trees: AUTOINCREMENT sequences and the catalog itself, whose
	// table and index entries are then rewritten with the new roots.
	for i := 0; i < pager.MetaSlots; i++ {
		nv.SetMeta(i, ov.Meta(i))
	}
	for _, slot := range []int{metaSequenceRoot, metaCatalogRoot} {
		if root := ov.Meta(slot); root != 0 {
			nr, err := copyTree(root)
			if err != nil {
				return err
			}
			nv.SetMeta(slot, nr)
		}
	}
	for _, k := range names {
		t := cat.tables[k]
		if err := saveEntry(nv, tableKey(t.Name), t); err != nil {
			return err
		}
		for _, ix := range t.indexes {
			if err := saveEntry(nv, indexKey(ix.Name), ix); err != nil {
				return err
			}
		}
	}
	nv.NewIdentity()
	if err := np.Commit(nv); err != nil {
		return err
	}
	if err := np.Checkpoint(); err != nil {
		return err
	}
	// Continue the sequence numbers of the old database.
	return np.FinishRaw(snap.Seq() + 1)
}
