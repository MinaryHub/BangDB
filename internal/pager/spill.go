package pager

import (
	"fmt"
	"os"
	"runtime"
)

// Transaction spill: a write view keeps the pages it changed in memory. A
// transaction that changes more than the spill limit (SetSpill) moves them
// to a temporary file, so its size is bounded by disk, not memory. At commit
// the spilled pages are copied from that file into the WAL in chunks.
//
// Spilling happens only at SpillPoint, which the caller invokes where no
// decoded page it holds is in the middle of a change (between rows).
//
// Spilled pages are sealed and, in an encrypted database, encrypted exactly
// as WAL frames, so the spill file holds no plaintext.

// DefaultSpillPages is the default spill limit: 64 MiB of changed pages.
const DefaultSpillPages = (64 << 20) / PageSize

const (
	spillChunk     = 4 << 20 // bytes written to the spill file or WAL at once
	spillReadCache = 1024    // spilled pages kept after being read back
)

// spillFile holds the spilled pages of one view.
type spillFile struct {
	f     *os.File
	path  string // removed on close (Windows cannot remove an open file)
	size  int64
	offs  map[uint32]int64 // page -> offset of its latest spilled image
	cache map[uint32][]byte
}

// SetSpill sets the directory for spill files ("" = os.TempDir()) and the
// number of changed pages a transaction keeps in memory before spilling
// (0 = DefaultSpillPages, negative = never spill).
func (p *Pager) SetSpill(dir string, pages int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pages == 0 {
		pages = DefaultSpillPages
	}
	p.spillDir, p.spillPages = dir, pages
}

// SetCommitWantsPages installs a test of whether the commit hook needs the
// commit's page images: when it does not, pages a transaction spilled are
// not read back for it.
func (p *Pager) SetCommitWantsPages(fn func() bool) {
	p.mu.Lock()
	p.wantPages = fn
	p.mu.Unlock()
}

// SpillPoint moves the view's changed pages to its spill file if there are
// more than the spill limit. Decoded pages (Lazy) are encoded first, so the
// caller must not be holding one it is still changing.
func (v *View) SpillPoint() error {
	p := v.p
	if p == nil || p.file == nil || len(v.dirty) < p.spillPages || p.spillPages <= 0 {
		return nil
	}
	return v.spillAll()
}

// Spilled returns the number of pages in the view's spill file.
func (v *View) Spilled() int {
	if v.sp == nil {
		return 0
	}
	return len(v.sp.offs)
}

func (v *View) spillAll() error {
	p := v.p
	if v.sp == nil {
		f, err := os.CreateTemp(p.spillDir, "bangdb-tx-*")
		if err != nil {
			return fmt.Errorf("bangdb: transaction spill file: %w", err)
		}
		v.sp = &spillFile{f: f, path: f.Name(), offs: map[uint32]int64{}}
		if runtime.GOOS != "windows" {
			os.Remove(f.Name()) // the open file stays usable and vanishes on close
			v.sp.path = ""
		}
		runtime.SetFinalizer(v.sp, (*spillFile).close)
		p.mu.Lock()
		p.counters.txSpills++
		p.mu.Unlock()
	}
	sp := v.sp
	buf := make([]byte, 0, spillChunk+PageSize)
	start := sp.size
	var done []uint32
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if _, err := sp.f.WriteAt(buf, start); err != nil {
			return fmt.Errorf("bangdb: transaction spill file: %w", err)
		}
		start += int64(len(buf))
		buf = buf[:0]
		return nil
	}
	for pg, b := range v.dirty {
		if pg == 0 {
			continue
		}
		if l, ok := v.lazy[pg]; ok {
			b = l.Encode()
		}
		off := start + int64(len(buf))
		buf = append(buf, b...)
		img := buf[len(buf)-PageSize:]
		seal(img)
		p.encryptInPlace(pg, img)
		done = append(done, pg)
		sp.offs[pg] = off
		if len(buf) >= spillChunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	sp.size = start
	for _, pg := range done {
		delete(v.dirty, pg)
		delete(v.lazy, pg)
		delete(v.owned, pg)
		delete(sp.cache, pg)
	}
	return nil
}

// readSpilled returns the spilled image of pg, if it has one.
func (v *View) readSpilled(pg uint32) ([]byte, bool, error) {
	sp := v.sp
	if sp == nil {
		return nil, false, nil
	}
	off, ok := sp.offs[pg]
	if !ok {
		return nil, false, nil
	}
	if b, ok := sp.cache[pg]; ok {
		return b, true, nil
	}
	b := make([]byte, PageSize)
	if _, err := sp.f.ReadAt(b, off); err != nil {
		return nil, true, fmt.Errorf("bangdb: transaction spill file: %w", err)
	}
	v.p.decrypt(pg, b)
	if err := verify(pg, b); err != nil {
		return nil, true, err
	}
	if sp.cache == nil || len(sp.cache) >= spillReadCache {
		sp.cache = map[uint32][]byte{}
	}
	sp.cache[pg] = b
	return b, true, nil
}

// rawSpilled appends the stored (sealed, possibly encrypted) image of pg.
func (sp *spillFile) rawSpilled(dst []byte, pg uint32) ([]byte, error) {
	n := len(dst)
	dst = append(dst, make([]byte, PageSize)...)
	if _, err := sp.f.ReadAt(dst[n:], sp.offs[pg]); err != nil {
		return nil, fmt.Errorf("bangdb: transaction spill file: %w", err)
	}
	return dst, nil
}

func (sp *spillFile) close() {
	if sp.f != nil {
		sp.f.Close()
		if sp.path != "" {
			os.Remove(sp.path)
		}
		sp.f = nil
	}
}

// Discard releases the view's spill file. The view must not be used for
// writing afterwards. Commit discards it itself.
func (v *View) Discard() {
	if v.sp != nil {
		runtime.SetFinalizer(v.sp, nil)
		v.sp.close()
		v.sp = nil
	}
}
