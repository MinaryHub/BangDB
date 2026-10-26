package pager

// Lazy pages: a writer may keep a page it changed in decoded form (a B+tree
// node) and leave the encoding to when the bytes are needed: at commit, or
// when Read is called for the page. A page changed many times in a
// transaction is then encoded once instead of after every change.

// Lazy is a page held in decoded form.
type Lazy interface {
	// Encode returns the page image (PageSize bytes).
	Encode() []byte
	// Clone returns an independent copy (kept by savepoints).
	Clone() Lazy
}

// WriteLazy sets page pg to l. The view owns l and may hand it back through
// PeekLazy and TakeLazy.
func (v *View) WriteLazy(pg uint32, l Lazy) {
	if pg == 0 {
		panic("pager: page 0 is reserved for the header")
	}
	v.track(pg)
	v.WriteLazyNoTrack(pg, l)
}

// WriteLazyNoTrack is WriteLazy without savepoint tracking (for Restore).
func (v *View) WriteLazyNoTrack(pg uint32, l Lazy) {
	if v.dirty == nil {
		v.dirty = map[uint32][]byte{}
	}
	if v.lazy == nil {
		v.lazy = map[uint32]Lazy{}
	}
	v.dirty[pg] = nil // dirty; the content is in v.lazy
	v.lazy[pg] = l
}

// PeekLazy returns the lazy form of pg, if any, for reading only.
func (v *View) PeekLazy(pg uint32) (Lazy, bool) {
	l, ok := v.lazy[pg]
	return l, ok
}

// TakeLazy returns the lazy form of pg for changing in place: the current
// state is first recorded for the active savepoint. The caller must write
// it back with WriteLazy.
func (v *View) TakeLazy(pg uint32) (Lazy, bool) {
	l, ok := v.lazy[pg]
	if ok {
		v.track(pg)
	}
	return l, ok
}

// materialize encodes every lazy page (before commit).
func (v *View) materialize() {
	for pg, l := range v.lazy {
		b := l.Encode()
		v.dirty[pg] = b
		v.SetOwned(pg, b, l) // shared with readers after the commit
	}
	v.lazy = nil
}
