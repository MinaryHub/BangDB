package pager

import (
	"sync"
	"unsafe"
)

// objCache keeps objects decoded from page images (B+tree nodes), keyed by
// the identity of the image. Page images are never modified once they are
// readable, and an entry holds a reference to its image, so the address
// cannot be reused by another image while the entry exists.
type objCache struct {
	shards [16]objShard
	max    int // entries per shard; 0 disables the cache
}

type objShard struct {
	mu sync.Mutex
	m  map[*byte]objEntry
}

type objEntry struct {
	buf []byte
	obj any
}

func (c *objCache) setSize(entries int) {
	c.max = entries / len(c.shards)
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		s.m = map[*byte]objEntry{}
		s.mu.Unlock()
	}
}

func (c *objCache) get(b []byte, decode func([]byte) (any, error)) (any, error) {
	if c.max <= 0 || len(b) == 0 {
		return decode(b)
	}
	key := unsafe.SliceData(b)
	s := &c.shards[(uintptr(unsafe.Pointer(key))>>12)%uintptr(len(c.shards))]
	s.mu.Lock()
	e, ok := s.m[key]
	s.mu.Unlock()
	if ok && len(e.buf) == len(b) {
		return e.obj, nil
	}
	obj, err := decode(b)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.m) >= c.max {
		n := len(s.m) / 8 // evict an arbitrary eighth
		for k := range s.m {
			delete(s.m, k)
			if n--; n <= 0 {
				break
			}
		}
	}
	s.m[key] = objEntry{b, obj}
	s.mu.Unlock()
	return obj, nil
}

// lookup returns the cached object for b, if any.
func (c *objCache) lookup(b []byte) (any, bool) {
	if c.max <= 0 || len(b) == 0 {
		return nil, false
	}
	key := unsafe.SliceData(b)
	s := &c.shards[(uintptr(unsafe.Pointer(key))>>12)%uintptr(len(c.shards))]
	s.mu.Lock()
	e, ok := s.m[key]
	s.mu.Unlock()
	return e.obj, ok && len(e.buf) == len(b)
}

// Cached returns the object cached for page image b by Decoded, if any,
// without decoding or caching anything (for writers about to replace b).
func (v *View) Cached(b []byte) (any, bool) { return v.p.objs.lookup(b) }

// Decoded returns decode(b) for a page image b obtained from Read, reusing
// an earlier result for the same image. The result is shared: callers must
// not modify it.
func (v *View) Decoded(b []byte, decode func([]byte) (any, error)) (any, error) {
	return v.p.objs.get(b, decode)
}

// SetOwned records that obj is a private decoded form of the image b just
// written to page pg by this view, so a later TakeOwned can skip decoding.
func (v *View) SetOwned(pg uint32, b []byte, obj any) {
	if v.owned == nil {
		v.owned = map[uint32]ownedObj{}
	}
	v.owned[pg] = ownedObj{b, obj}
}

// TakeOwned returns (and forgets) the object recorded by SetOwned if b is
// still the image it was recorded for. The caller may modify it.
func (v *View) TakeOwned(pg uint32, b []byte) (any, bool) {
	o, ok := v.owned[pg]
	if !ok {
		return nil, false
	}
	delete(v.owned, pg)
	if len(o.buf) != len(b) || len(b) == 0 || unsafe.SliceData(o.buf) != unsafe.SliceData(b) {
		return nil, false
	}
	return o.obj, true
}

// SetCacheSize sets the memory used for cached file pages and decoded
// objects (about half each).
func (p *Pager) SetCacheSize(bytes int) {
	pages := max(bytes/2/PageSize, 16)
	p.mu.Lock()
	if p.file != nil {
		p.cacheMax = pages
	}
	p.mu.Unlock()
	p.objs.setSize(pages)
}
