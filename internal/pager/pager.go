// Package pager manages fixed-size pages on disk (or in memory) and provides
// copy-on-write transactional views with WAL-based crash recovery.
//
// Concurrency model:
//   - The Pager holds the last committed state (file + shared clean-page cache).
//   - A View is a private overlay of dirty pages on top of the committed state.
//     Read views never write; a single write view at a time is enforced by the
//     caller (engine), which also blocks readers while Commit applies pages.
package pager

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// PageSize is the size of every page in bytes.
const PageSize = 4096

// MetaSlots is the number of user-defined uint32 slots in the header page.
const MetaSlots = 8

const (
	magic        = "BangDB\x00\x01"
	walMagic     = "BWAL"
	commitMarker = 0xFFFFFFFF
	defaultCache = 2048 // clean pages kept in memory (8 MiB)
)

var (
	// ErrCorrupt is returned when the file does not look like a BangDB file.
	ErrCorrupt = errors.New("bangdb: database file is corrupt or not a BangDB file")
	// ErrClosed is returned when using a closed pager.
	ErrClosed = errors.New("bangdb: database is closed")
)

// Header is the content of page 0.
type Header struct {
	PageCount uint32 // number of pages in use, including page 0
	FreeHead  uint32 // first page of the free list (0 = empty)
	Meta      [MetaSlots]uint32
	Seq       uint64   // commit sequence number, +1 per commit
	DBID      [16]byte // random identity assigned at creation; replicas share it
}

const (
	offSeq  = 20 + MetaSlots*4
	offDBID = offSeq + 8
)

func newHeader() Header {
	h := Header{PageCount: 1}
	if _, err := rand.Read(h.DBID[:]); err != nil {
		panic(err)
	}
	return h
}

func (h *Header) encode() []byte {
	b := make([]byte, PageSize)
	copy(b, magic)
	binary.BigEndian.PutUint32(b[8:], PageSize)
	binary.BigEndian.PutUint32(b[12:], h.PageCount)
	binary.BigEndian.PutUint32(b[16:], h.FreeHead)
	for i, m := range h.Meta {
		binary.BigEndian.PutUint32(b[20+i*4:], m)
	}
	binary.BigEndian.PutUint64(b[offSeq:], h.Seq)
	copy(b[offDBID:], h.DBID[:])
	return b
}

func decodeHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < PageSize || string(b[:8]) != magic {
		return h, ErrCorrupt
	}
	if binary.BigEndian.Uint32(b[8:]) != PageSize {
		return h, fmt.Errorf("bangdb: unsupported page size %d", binary.BigEndian.Uint32(b[8:]))
	}
	h.PageCount = binary.BigEndian.Uint32(b[12:])
	h.FreeHead = binary.BigEndian.Uint32(b[16:])
	for i := range h.Meta {
		h.Meta[i] = binary.BigEndian.Uint32(b[20+i*4:])
	}
	h.Seq = binary.BigEndian.Uint64(b[offSeq:])
	copy(h.DBID[:], b[offDBID:])
	return h, nil
}

// Pager owns the committed database state.
type Pager struct {
	mu       sync.Mutex // guards cache, mem, hdr, err
	file     *os.File   // nil for in-memory databases
	wal      *os.File
	unlock   func()
	mem      map[uint32][]byte // page store for in-memory databases
	cache    map[uint32][]byte
	cacheMax int
	hdr      Header
	err      error // sticky error after a failed commit
	closed   bool

	// failpoint, when set by tests, is called at commit stages to simulate
	// crashes.
	failpoint func(stage string) error

	// onCommit, if set, receives every committed page set (including the
	// header page 0) while the commit lock is held. Used for replication.
	onCommit func(seq uint64, pages map[uint32][]byte)
}

// OpenMemory creates an empty, purely in-memory pager.
func OpenMemory() *Pager {
	h := newHeader()
	return &Pager{mem: map[uint32][]byte{0: h.encode()}, hdr: h}
}

// Open opens (creating if necessary) the database file at path. A sibling
// file path+"-wal" is used as the write-ahead log.
func Open(path string) (*Pager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	wal, err := os.OpenFile(path+"-wal", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		unlock()
		f.Close()
		return nil, err
	}
	p := &Pager{file: f, wal: wal, unlock: unlock, cache: map[uint32][]byte{}, cacheMax: defaultCache}
	if err := p.recover(); err != nil {
		p.Close()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		p.Close()
		return nil, err
	}
	if st.Size() == 0 {
		p.hdr = newHeader()
		if _, err := f.WriteAt(p.hdr.encode(), 0); err != nil {
			p.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			p.Close()
			return nil, err
		}
		return p, nil
	}
	buf := make([]byte, PageSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		p.Close()
		return nil, ErrCorrupt
	}
	if p.hdr, err = decodeHeader(buf); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// recover replays a fully committed WAL into the main file, then truncates it.
// A torn (incomplete or checksum-failing) WAL is discarded: the main file was
// never touched by that transaction.
func (p *Pager) recover() error {
	data, err := io.ReadAll(io.NewSectionReader(p.wal, 0, 1<<62))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	frames, ok := parseWAL(data)
	if ok {
		for _, fr := range frames {
			if _, err := p.file.WriteAt(fr.data, int64(fr.pgno)*PageSize); err != nil {
				return err
			}
		}
		if err := p.file.Sync(); err != nil {
			return err
		}
	}
	if err := p.wal.Truncate(0); err != nil {
		return err
	}
	return p.wal.Sync()
}

type frame struct {
	pgno uint32
	data []byte
}

func parseWAL(data []byte) ([]frame, bool) {
	if len(data) < 8 || string(data[:4]) != walMagic || binary.BigEndian.Uint32(data[4:]) != PageSize {
		return nil, false
	}
	var frames []frame
	off := 8
	crc := crc32.NewIEEE()
	for off+4 <= len(data) {
		pgno := binary.BigEndian.Uint32(data[off:])
		if pgno == commitMarker {
			if off+12 > len(data) {
				return nil, false
			}
			n := binary.BigEndian.Uint32(data[off+4:])
			sum := binary.BigEndian.Uint32(data[off+8:])
			return frames, int(n) == len(frames) && sum == crc.Sum32()
		}
		if off+4+PageSize > len(data) {
			return nil, false
		}
		crc.Write(data[off : off+4+PageSize])
		frames = append(frames, frame{pgno, data[off+4 : off+4+PageSize]})
		off += 4 + PageSize
	}
	return nil, false
}

// InMemory reports whether the pager has no backing file.
func (p *Pager) InMemory() bool { return p.file == nil }

// Close releases the file handles.
func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var errs []error
	if p.wal != nil {
		errs = append(errs, p.wal.Close())
	}
	if p.unlock != nil {
		p.unlock()
	}
	if p.file != nil {
		errs = append(errs, p.file.Close())
	}
	p.mem, p.cache = nil, nil
	return errors.Join(errs...)
}

// SetOnCommit installs the commit hook (see Pager.onCommit).
func (p *Pager) SetOnCommit(fn func(seq uint64, pages map[uint32][]byte)) {
	p.mu.Lock()
	p.onCommit = fn
	p.mu.Unlock()
}

// Header returns the committed header.
func (p *Pager) Header() Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdr
}

// ReadCommitted returns the committed content of a page; see readCommitted.
func (p *Pager) ReadCommitted(pgno uint32) ([]byte, error) { return p.readCommitted(pgno) }

// readCommitted returns the committed content of a page. The returned slice
// is shared and must not be modified.
func (p *Pager) readCommitted(pgno uint32) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if p.err != nil {
		return nil, p.err
	}
	if pgno >= p.hdr.PageCount {
		return nil, fmt.Errorf("bangdb: page %d out of range (%d pages)", pgno, p.hdr.PageCount)
	}
	if p.mem != nil {
		b, ok := p.mem[pgno]
		if !ok {
			return nil, ErrCorrupt
		}
		return b, nil
	}
	if b, ok := p.cache[pgno]; ok {
		return b, nil
	}
	b := make([]byte, PageSize)
	if _, err := p.file.ReadAt(b, int64(pgno)*PageSize); err != nil {
		return nil, fmt.Errorf("bangdb: read page %d: %w", pgno, err)
	}
	p.cachePut(pgno, b)
	return b, nil
}

func (p *Pager) cachePut(pgno uint32, b []byte) {
	if len(p.cache) >= p.cacheMax {
		// Random eviction: Go map iteration order is randomized.
		n := len(p.cache) / 8
		for k := range p.cache {
			delete(p.cache, k)
			if n--; n <= 0 {
				break
			}
		}
	}
	p.cache[pgno] = b
}

// NewView starts a new overlay on the committed state.
func (p *Pager) NewView() *View {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &View{p: p, dirty: map[uint32][]byte{}, hdr: p.hdr}
}

// Commit durably applies the view's changes and advances the commit
// sequence number. The caller must guarantee that no reader is accessing the
// pager concurrently and that v is the only writer.
func (p *Pager) Commit(v *View) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.err != nil {
		return p.err
	}
	if len(v.dirty) == 0 && v.hdr == p.hdr {
		return nil
	}
	v.hdr.Seq = p.hdr.Seq + 1
	v.dirty[0] = v.hdr.encode()
	if err := p.applyLocked(v.dirty, v.hdr); err != nil {
		return err
	}
	if p.onCommit != nil {
		p.onCommit(v.hdr.Seq, v.dirty)
	}
	v.dirty = map[uint32][]byte{}
	return nil
}

// CommitRaw durably applies a page set received from a primary (it must
// contain the header page 0). The header, including its sequence number, is
// taken verbatim from page 0.
func (p *Pager) CommitRaw(pages map[uint32][]byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.err != nil {
		return p.err
	}
	hdr, err := decodeHeader(pages[0])
	if err != nil {
		return err
	}
	if err := p.applyLocked(pages, hdr); err != nil {
		return err
	}
	if p.onCommit != nil {
		p.onCommit(hdr.Seq, pages) // lets replicas be backed up or cascaded
	}
	return nil
}

// PutRaw writes a page directly, bypassing the WAL. It is only used while
// building a snapshot copy that nobody reads yet; FinishRaw makes it durable.
func (p *Pager) PutRaw(pgno uint32, data []byte) error {
	if len(data) != PageSize {
		return errors.New("pager: raw page of wrong size")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mem != nil {
		p.mem[pgno] = append([]byte(nil), data...)
		return nil
	}
	delete(p.cache, pgno)
	_, err := p.file.WriteAt(data, int64(pgno)*PageSize)
	return err
}

// FinishRaw completes a snapshot copy: it stamps the header with the given
// sequence number, syncs, and loads the header.
func (p *Pager) FinishRaw(seq uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b []byte
	if p.mem != nil {
		b = p.mem[0]
	} else {
		b = make([]byte, PageSize)
		if _, err := p.file.ReadAt(b, 0); err != nil {
			return err
		}
	}
	hdr, err := decodeHeader(b)
	if err != nil {
		return err
	}
	hdr.Seq = seq
	enc := hdr.encode()
	if p.mem != nil {
		p.mem[0] = enc
	} else {
		if _, err := p.file.WriteAt(enc, 0); err != nil {
			return err
		}
		if err := p.file.Sync(); err != nil {
			return err
		}
		clear(p.cache)
	}
	p.hdr = hdr
	return nil
}

func (p *Pager) applyLocked(pages map[uint32][]byte, hdr Header) error {
	if p.mem != nil {
		for pg, b := range pages {
			p.mem[pg] = b
		}
		for pg := range p.mem {
			if pg >= hdr.PageCount {
				delete(p.mem, pg)
			}
		}
		p.hdr = hdr
		return nil
	}

	// 1. Write all pages + commit record to the WAL and fsync.
	buf := make([]byte, 0, 8+len(pages)*(4+PageSize)+12)
	buf = append(buf, walMagic...)
	buf = binary.BigEndian.AppendUint32(buf, PageSize)
	crc := crc32.NewIEEE()
	for pg, b := range pages {
		start := len(buf)
		buf = binary.BigEndian.AppendUint32(buf, pg)
		buf = append(buf, b...)
		crc.Write(buf[start:])
	}
	buf = binary.BigEndian.AppendUint32(buf, commitMarker)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(pages)))
	buf = binary.BigEndian.AppendUint32(buf, crc.Sum32())
	if err := p.wal.Truncate(0); err != nil {
		return err
	}
	if _, err := p.wal.WriteAt(buf, 0); err != nil {
		p.wal.Truncate(0)
		return err
	}
	if err := p.wal.Sync(); err != nil {
		p.wal.Truncate(0)
		return err
	}
	// From here on the transaction is durable. Failures below poison the
	// pager; reopening the database replays the WAL.
	if p.failpoint != nil {
		if err := p.failpoint("wal-synced"); err != nil {
			p.err = err
			return err
		}
	}
	for pg, b := range pages {
		if _, err := p.file.WriteAt(b, int64(pg)*PageSize); err != nil {
			p.err = fmt.Errorf("bangdb: checkpoint failed, reopen database to recover: %w", err)
			return p.err
		}
	}
	if err := p.file.Sync(); err != nil {
		p.err = fmt.Errorf("bangdb: checkpoint failed, reopen database to recover: %w", err)
		return p.err
	}
	if err := p.wal.Truncate(0); err != nil {
		p.err = err
		return err
	}
	for pg, b := range pages {
		if _, ok := p.cache[pg]; ok || len(p.cache) < p.cacheMax {
			p.cache[pg] = b
		}
	}
	p.hdr = hdr
	return nil
}

// View is a transactional overlay over the committed state.
type View struct {
	p     *Pager
	dirty map[uint32][]byte
	hdr   Header

	// Undo log for the active savepoint: the previous dirty entry of every
	// page touched since Savepoint was called.
	spGen    int
	undo     []undoEntry
	undoSeen map[uint32]struct{}
}

type undoEntry struct {
	pg   uint32
	prev []byte
	had  bool
}

func (v *View) track(pg uint32) {
	if v.spGen == 0 {
		return
	}
	if _, ok := v.undoSeen[pg]; ok {
		return
	}
	v.undoSeen[pg] = struct{}{}
	prev, had := v.dirty[pg]
	v.undo = append(v.undo, undoEntry{pg, prev, had})
}

// Read returns the page content as seen by this view. The returned slice
// must not be modified; use Write with a fresh buffer instead.
func (v *View) Read(pgno uint32) ([]byte, error) {
	if b, ok := v.dirty[pgno]; ok {
		return b, nil
	}
	if pgno >= v.hdr.PageCount {
		return nil, fmt.Errorf("bangdb: page %d out of range", pgno)
	}
	return v.p.readCommitted(pgno)
}

// Write replaces the content of a page. The view takes ownership of data.
func (v *View) Write(pgno uint32, data []byte) {
	if len(data) != PageSize {
		panic("pager: write of wrong page size")
	}
	if pgno == 0 {
		panic("pager: page 0 is reserved for the header")
	}
	v.track(pgno)
	v.dirty[pgno] = data
}

// Alloc returns a fresh zeroed page number, reusing the free list first.
func (v *View) Alloc() (uint32, error) {
	if v.hdr.FreeHead != 0 {
		pg := v.hdr.FreeHead
		b, err := v.Read(pg)
		if err != nil {
			return 0, err
		}
		v.hdr.FreeHead = binary.BigEndian.Uint32(b[0:4])
		v.track(pg)
		v.dirty[pg] = make([]byte, PageSize)
		return pg, nil
	}
	pg := v.hdr.PageCount
	v.hdr.PageCount++
	v.track(pg)
	v.dirty[pg] = make([]byte, PageSize)
	return pg, nil
}

// Free puts a page on the free list.
func (v *View) Free(pgno uint32) {
	b := make([]byte, PageSize)
	binary.BigEndian.PutUint32(b[0:4], v.hdr.FreeHead)
	v.track(pgno)
	v.dirty[pgno] = b
	v.hdr.FreeHead = pgno
}

// Meta returns a user metadata slot from the header.
func (v *View) Meta(i int) uint32 { return v.hdr.Meta[i] }

// SetMeta sets a user metadata slot in the header.
func (v *View) SetMeta(i int, val uint32) { v.hdr.Meta[i] = val }

// NewIdentity gives the database a new random DBID on commit. Used when a
// replica is promoted, so that copies of the old lineage cannot follow it
// without a fresh snapshot.
func (v *View) NewIdentity() {
	if _, err := rand.Read(v.hdr.DBID[:]); err != nil {
		panic(err)
	}
}

// PageCount returns the number of pages as seen by the view.
func (v *View) PageCount() uint32 { return v.hdr.PageCount }

// Dirty returns the number of modified pages in the view.
func (v *View) Dirty() int { return len(v.dirty) }

// Savepoint marks the current view state. Only the most recent savepoint
// can be restored; taking a new one releases the previous one. The cost is
// proportional to the pages modified after the savepoint, not to the size of
// the transaction.
type Savepoint struct {
	hdr Header
	gen int
}

// Savepoint starts a new undo log and returns its marker.
func (v *View) Savepoint() Savepoint {
	v.spGen++
	v.undo = v.undo[:0]
	if v.undoSeen == nil || len(v.undoSeen) > 256 {
		v.undoSeen = map[uint32]struct{}{}
	} else {
		clear(v.undoSeen)
	}
	return Savepoint{hdr: v.hdr, gen: v.spGen}
}

// Restore rolls the view back to the most recent savepoint.
func (v *View) Restore(sp Savepoint) {
	if sp.gen != v.spGen {
		panic("pager: restore of a released savepoint")
	}
	for i := len(v.undo) - 1; i >= 0; i-- {
		u := v.undo[i]
		if u.had {
			v.dirty[u.pg] = u.prev
		} else {
			delete(v.dirty, u.pg)
		}
	}
	v.undo = v.undo[:0]
	clear(v.undoSeen)
	v.hdr = sp.hdr
}
