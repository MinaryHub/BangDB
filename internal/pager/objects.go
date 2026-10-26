package pager

// Object slots let storage kinds keep data as Go values instead of pages
// (the in-memory table store). A slot holds an immutable value under a
// numeric id (by convention a page the kind allocated as its root). Slots
// follow the page rules: a view's changes are private until its commit is
// published, snapshots see the slots of their commit, savepoints roll them
// back, and the next writer builds on the latest prepared commit. Slots are
// not written to the WAL or the database file: they live as long as the
// Pager and are not part of backups or replication frames.

// Token identifies a mutation epoch of a view: objects created under the
// current token may be changed in place, older ones must be copied. A view
// gets a new token at every savepoint, so the state a savepoint can return
// to is never changed in place.
type Token struct{ _ byte }

type objUndo struct {
	id   uint32
	prev any
	had  bool
}

// Obj returns the value in slot id as seen by the view (nil if empty).
func (v *View) Obj(id uint32) any {
	if o, ok := v.dirtyObjs[id]; ok {
		return o
	}
	return v.baseObjs[id]
}

// SetObj stores o in slot id (nil empties it). The view takes ownership of
// o: once committed it must never change.
func (v *View) SetObj(id uint32, o any) {
	if v.spGen > 0 {
		if v.objSeen == nil {
			v.objSeen = map[uint32]struct{}{}
		}
		if _, ok := v.objSeen[id]; !ok {
			v.objSeen[id] = struct{}{}
			prev, had := v.dirtyObjs[id]
			v.objUndo = append(v.objUndo, objUndo{id, prev, had})
		}
	}
	if v.dirtyObjs == nil {
		v.dirtyObjs = map[uint32]any{}
	}
	v.dirtyObjs[id] = o
}

// Token returns the view's current mutation token.
func (v *View) Token() *Token {
	if v.token == nil {
		v.token = new(Token)
	}
	return v.token
}

func (v *View) objSavepoint() {
	v.token = nil
	v.objUndo = v.objUndo[:0]
	clear(v.objSeen)
}

func (v *View) objRestore() {
	for i := len(v.objUndo) - 1; i >= 0; i-- {
		u := v.objUndo[i]
		if u.had {
			v.dirtyObjs[u.id] = u.prev
		} else {
			delete(v.dirtyObjs, u.id)
		}
	}
	v.objUndo = v.objUndo[:0]
	clear(v.objSeen)
}

// mergedObjs returns the slots of the view's commit, or nil if the view
// changed none.
func (v *View) mergedObjs() map[uint32]any {
	if len(v.dirtyObjs) == 0 {
		return nil
	}
	m := make(map[uint32]any, len(v.baseObjs)+len(v.dirtyObjs))
	for id, o := range v.baseObjs {
		m[id] = o
	}
	for id, o := range v.dirtyObjs {
		if o == nil {
			delete(m, id)
		} else {
			m[id] = o
		}
	}
	return m
}

// Objects returns the published slots.
func (p *Pager) Objects() map[uint32]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slots
}

// SetObjects replaces the published slots (used when a rebuilt database
// takes over the objects of the one it replaces, before any other use).
func (p *Pager) SetObjects(m map[uint32]any) {
	p.mu.Lock()
	p.slots, p.prepSlots = m, m
	p.mu.Unlock()
}

// ChangedObjects returns the view's object slots with its changes applied,
// or nil if it changed none.
func (v *View) ChangedObjects() map[uint32]any { return v.mergedObjs() }
