// Package pager manages fixed-size pages on disk (or in memory) with
// multi-version snapshot reads and a write-ahead log.
//
// Design (similar to SQLite's WAL mode):
//   - A commit appends the changed pages to the WAL and fsyncs it once. The
//     pages then become visible as new versions tagged with the commit's
//     sequence number; the database file is not touched.
//   - A reader works on a Snapshot: it sees, for every page, the newest
//     version whose sequence is <= the snapshot's sequence, falling back to
//     the database file. Readers therefore never block commits and never see
//     partial commits.
//   - A checkpoint copies page versions into the database file, but only up to
//     the oldest active snapshot, so nothing a reader can still see is
//     overwritten. When no versions remain the WAL is reset.
//   - On open, complete commit groups in the WAL newer than the file are
//     replayed; a torn tail is ignored.
//
// There is a single writer at a time (enforced by the engine). Writer-side
// methods (PrepareCommit, Checkpoint, ...) must not run concurrently with each
// other; reads may run at any time.
package pager

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// PageSize is the size of every page in bytes.
const PageSize = 4096

// UsableSize is the part of a page available to its content; the last 4
// bytes hold a CRC32 checksum of the rest, verified whenever a page is read
// from disk.
const UsableSize = PageSize - 4

// MetaSlots is the number of user-defined uint32 slots in the header page.
const MetaSlots = 8

// FormatVersion is the on-disk format written by this version. Version 3 is
// the first stable format (page checksums); files of other versions are
// rejected with an explanatory error.
const FormatVersion = 3

const (
	magicPrefix  = "BangDB\x00" // followed by the format version byte
	walMagic     = "BWL2"
	commitMarker = 0xFFFFFFFF
	defaultCache = 2048 // clean file pages kept in memory (8 MiB)

	// CheckpointPages is the WAL size (in pages) after which the engine
	// should run a checkpoint.
	CheckpointPages = 1000
	// versionMemBudget bounds the WAL page images kept in memory; beyond it
	// versions are read back from the WAL file.
	versionMemBudget = 64 << 20
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
	copy(b, magicPrefix)
	b[len(magicPrefix)] = FormatVersion
	binary.BigEndian.PutUint32(b[8:], PageSize)
	binary.BigEndian.PutUint32(b[12:], h.PageCount)
	binary.BigEndian.PutUint32(b[16:], h.FreeHead)
	for i, m := range h.Meta {
		binary.BigEndian.PutUint32(b[20+i*4:], m)
	}
	binary.BigEndian.PutUint64(b[offSeq:], h.Seq)
	copy(b[offDBID:], h.DBID[:])
	seal(b)
	return b
}

// seal stores the page checksum.
func seal(b []byte) {
	binary.BigEndian.PutUint32(b[UsableSize:], crc32.ChecksumIEEE(b[:UsableSize]))
}

// verify checks a page read from disk.
func verify(pgno uint32, b []byte) error {
	if binary.BigEndian.Uint32(b[UsableSize:]) != crc32.ChecksumIEEE(b[:UsableSize]) {
		return fmt.Errorf("%w: checksum mismatch on page %d", ErrCorrupt, pgno)
	}
	return nil
}

func decodeHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < PageSize || string(b[:len(magicPrefix)]) != magicPrefix {
		return h, ErrCorrupt
	}
	if v := b[len(magicPrefix)]; v != FormatVersion {
		if v < FormatVersion {
			return h, fmt.Errorf("bangdb: database format version %d was written by a pre-release build and is not supported; recreate the database", v)
		}
		return h, fmt.Errorf("bangdb: database format version %d is newer than this build supports (%d)", v, FormatVersion)
	}
	if err := verify(0, b); err != nil {
		return h, err
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

// version is one committed image of a page that is not yet checkpointed.
type version struct {
	seq  uint64
	off  int64  // offset of the page image in the WAL (file mode)
	data []byte // in-memory image; nil if it must be read from the WAL
}

// Pager owns the committed database state.
type Pager struct {
	mu       sync.Mutex
	file     *os.File // nil for in-memory databases
	wal      *os.File
	unlock   func()
	base     map[uint32][]byte // checkpointed pages of in-memory databases
	cache    map[uint32][]byte // checkpointed file pages
	cacheMax int
	objs     objCache // decoded page images

	slots     map[uint32]any // object slots of the published commit
	prepSlots map[uint32]any // object slots of the latest prepared commit

	versions map[uint32][]version // uncheckpointed versions, ascending seq
	walPages int                  // number of versions
	walSize  int64                // bytes used in the WAL file
	verBytes int                  // bytes of version images held in memory
	hdr      Header               // header of the latest published (durable) commit
	meta     any                  // caller data published with hdr (the engine's catalog)
	prepHdr  Header               // header of the latest prepared commit (>= hdr)
	pending  []*Prepared          // prepared commits waiting for their WAL fsync
	flushMu  sync.Mutex           // one WAL fsync at a time (group commit)
	readers  map[uint64]int       // active snapshot sequences -> count

	err    error // sticky error after a failed commit
	closed bool

	// failpoint, when set by tests, is called at commit stages to simulate
	// crashes.
	failpoint func(stage string) error

	// onCommit, if set, receives every published page set (including the
	// header page 0) while the pager lock is held. Used for replication.
	onCommit func(seq uint64, pages map[uint32][]byte)
}

func newPager() *Pager {
	p := &Pager{versions: map[uint32][]version{}, readers: map[uint64]int{}}
	p.objs.setSize(defaultCache)
	return p
}

// OpenMemory creates an empty, purely in-memory pager.
func OpenMemory() *Pager {
	p := newPager()
	p.hdr = newHeader()
	p.prepHdr = p.hdr
	p.base = map[uint32][]byte{0: p.hdr.encode()}
	return p
}

// Open opens (creating if necessary) the database file at path. A sibling
// file path+"-wal" is used as the write-ahead log.
func Open(path string) (*Pager, error) {
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)
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
	p := newPager()
	p.file, p.wal, p.unlock = f, wal, unlock
	p.cache, p.cacheMax = map[uint32][]byte{}, defaultCache
	fail := func(err error) (*Pager, error) {
		p.closeFiles()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if st.Size() == 0 {
		p.hdr = newHeader()
		if _, err := f.WriteAt(p.hdr.encode(), 0); err != nil {
			return fail(err)
		}
		if err := f.Sync(); err != nil {
			return fail(err)
		}
		if created {
			if err := SyncDir(path); err != nil {
				return fail(err)
			}
		}
	} else if p.hdr, err = p.readFileHeader(); err != nil {
		return fail(err)
	}
	if err := p.recover(); err != nil {
		return fail(err)
	}
	p.prepHdr = p.hdr
	return p, nil
}

// SyncDir fsyncs the directory containing path so that a created or renamed
// file survives power loss.
func SyncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

func (p *Pager) readFileHeader() (Header, error) {
	buf := make([]byte, PageSize)
	if _, err := p.file.ReadAt(buf, 0); err != nil {
		return Header{}, ErrCorrupt
	}
	return decodeHeader(buf)
}

// recover applies committed WAL groups newer than the database file, then
// resets the WAL. A torn or corrupt group ends the replay.
func (p *Pager) recover() error {
	data, err := io.ReadAll(io.NewSectionReader(p.wal, 0, 1<<62))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	// Collect the final image of every page over the applicable groups.
	final := map[uint32][]byte{}
	next := p.hdr.Seq + 1
	for _, g := range parseWAL(data) {
		if g.seq < next {
			continue // already checkpointed
		}
		if g.seq != next {
			break
		}
		for _, fr := range g.frames {
			final[fr.pgno] = fr.data
		}
		next++
	}
	if len(final) > 0 {
		// The header goes last: if we crash before it is durable, the next
		// recovery replays the same groups again.
		if err := p.writePagesHeaderLast(final); err != nil {
			return err
		}
		if p.hdr, err = p.readFileHeader(); err != nil {
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

type walGroup struct {
	seq    uint64
	frames []frame
}

// parseWAL returns the complete, checksummed commit groups of a WAL image.
//
//	header:  "BWL2" pageSize(4)
//	group:   (pgno(4) page)*  0xFFFFFFFF seq(8) count(4) crc32(4)
func parseWAL(data []byte) []walGroup {
	if len(data) < 8 || string(data[:4]) != walMagic || binary.BigEndian.Uint32(data[4:]) != PageSize {
		return nil
	}
	var groups []walGroup
	var frames []frame
	off := 8
	crc := crc32.NewIEEE()
	for off+4 <= len(data) {
		pgno := binary.BigEndian.Uint32(data[off:])
		if pgno == commitMarker {
			if off+20 > len(data) {
				return groups
			}
			crc.Write(data[off+4 : off+12])
			seq := binary.BigEndian.Uint64(data[off+4:])
			n := binary.BigEndian.Uint32(data[off+12:])
			if int(n) != len(frames) || binary.BigEndian.Uint32(data[off+16:]) != crc.Sum32() {
				return groups
			}
			groups = append(groups, walGroup{seq, frames})
			frames = nil
			crc.Reset()
			off += 20
			continue
		}
		if off+4+PageSize > len(data) {
			return groups
		}
		crc.Write(data[off : off+4+PageSize])
		frames = append(frames, frame{pgno, data[off+4 : off+4+PageSize]})
		off += 4 + PageSize
	}
	return groups
}

// InMemory reports whether the pager has no backing file.
func (p *Pager) InMemory() bool { return p.file == nil }

// Close checkpoints everything (if no snapshot is open) and releases the
// files.
func (p *Pager) Close() error {
	p.mu.Lock()
	closed, broken := p.closed, p.err != nil
	p.mu.Unlock()
	if closed {
		return nil
	}
	var errs []error
	if !broken {
		errs = append(errs, p.flushPending(), p.Checkpoint())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	errs = append(errs, p.closeFiles())
	p.base, p.cache, p.versions = nil, nil, nil
	return errors.Join(errs...)
}

func (p *Pager) closeFiles() error {
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
	return errors.Join(errs...)
}

// SetOnCommit installs the commit hook (see Pager.onCommit).
func (p *Pager) SetOnCommit(fn func(seq uint64, pages map[uint32][]byte)) {
	p.mu.Lock()
	p.onCommit = fn
	p.mu.Unlock()
}

// Header returns the header of the latest published commit.
func (p *Pager) Header() Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdr
}

// readAt returns page pgno as of commit seq. The slice must not be modified.
func (p *Pager) readAt(pgno uint32, seq uint64) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if p.err != nil {
		return nil, p.err
	}
	vs := p.versions[pgno]
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].seq <= seq {
			return p.versionData(pgno, vs[i])
		}
	}
	if p.file == nil {
		b, ok := p.base[pgno]
		if !ok {
			return nil, fmt.Errorf("%w (missing page %d)", ErrCorrupt, pgno)
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
	if err := verify(pgno, b); err != nil {
		return nil, err
	}
	p.cachePut(pgno, b)
	return b, nil
}

// versionData must be called with p.mu held.
func (p *Pager) versionData(no uint32, v version) ([]byte, error) {
	if v.data != nil {
		return v.data, nil
	}
	b := make([]byte, PageSize)
	if _, err := p.wal.ReadAt(b, v.off); err != nil {
		return nil, fmt.Errorf("bangdb: read WAL: %w", err)
	}
	if err := verify(no, b); err != nil {
		return nil, err
	}
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

// ---- snapshots and views ----

// Snapshot pins the state of one commit for reading. It must be released.
type Snapshot struct {
	p        *Pager
	seq      uint64
	hdr      Header
	meta     any
	slots    map[uint32]any
	released bool
}

// Snapshot opens a read snapshot of the latest published commit.
func (p *Pager) Snapshot() (*Snapshot, error) { return p.SnapshotWith(nil) }

// SnapshotWith opens a snapshot and calls fn with its sequence while no
// commit can be published, e.g. to subscribe to all later commits.
func (p *Pager) SnapshotWith(fn func(seq uint64) error) (*Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if fn != nil {
		if err := fn(p.hdr.Seq); err != nil {
			return nil, err
		}
	}
	p.readers[p.hdr.Seq]++
	return &Snapshot{p: p, seq: p.hdr.Seq, hdr: p.hdr, meta: p.meta, slots: p.slots}, nil
}

// Meta returns the caller data published with the snapshot's commit.
func (s *Snapshot) Meta() any { return s.meta }

// SetMeta sets the caller data of the current published state (used once
// after opening, before any commit).
func (p *Pager) SetMeta(m any) {
	p.mu.Lock()
	p.meta = m
	p.mu.Unlock()
}

// Seq returns the commit sequence the snapshot sees.
func (s *Snapshot) Seq() uint64 { return s.seq }

// Header returns the header as of the snapshot.
func (s *Snapshot) Header() Header { return s.hdr }

// View returns a read view of the snapshot.
func (s *Snapshot) View() *View {
	return &View{p: s.p, hdr: s.hdr, seq: s.seq, baseObjs: s.slots} // dirty: created on first write
}

// Release unpins the snapshot so that checkpoints can proceed past it.
func (s *Snapshot) Release() {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	if s.released {
		return
	}
	s.released = true
	if s.p.readers[s.seq]--; s.p.readers[s.seq] <= 0 {
		delete(s.p.readers, s.seq)
	}
}

// NewView starts a write overlay on the latest prepared commit (which may
// not be durable yet: the writer builds on commits still waiting for their
// fsync). Only the single writer may use it; it needs no snapshot because
// nothing else can commit or checkpoint while the writer holds its lock.
func (p *Pager) NewView() *View {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &View{p: p, dirty: map[uint32][]byte{}, hdr: p.prepHdr, seq: p.prepHdr.Seq, baseObjs: p.prepSlots}
}

// PreparedHeader returns the header of the latest prepared commit.
func (p *Pager) PreparedHeader() Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prepHdr
}

// ---- commits ----

// Prepared is a commit written to the WAL but not yet durable or visible to
// readers; Flush makes it both.
type Prepared struct {
	pages map[uint32][]byte
	hdr   Header
	slots map[uint32]any // object slots after this commit; nil = unchanged
	// Meta is published together with the commit (Snapshot.Meta). It is
	// read under the pager lock; see PrepareCommit and SetPreparedMeta.
	Meta any
}

// Seq returns the commit's sequence number.
func (c *Prepared) Seq() uint64 { return c.hdr.Seq }

// PrepareCommit appends the view's changes to the WAL (without fsync) and
// assigns the next sequence number. It returns nil if there is nothing to
// commit. The next writer may build on it at once; Flush makes it durable
// and visible to readers, together with meta (nil keeps the previous
// commit's). Writer-side.
func (p *Pager) PrepareCommit(v *View, meta any) (*Prepared, error) {
	p.mu.Lock()
	cur, err := p.prepHdr, p.usable()
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	slots := v.mergedObjs()
	if len(v.dirty) == 0 && v.hdr == cur && slots == nil {
		return nil, nil
	}
	v.hdr.Seq = cur.Seq + 1
	v.put(0, v.hdr.encode())
	v.materialize()
	pages := v.dirty
	v.dirty = map[uint32][]byte{}
	v.dirtyObjs = nil
	p.publishOwned(v, pages)
	return p.prepare(pages, v.hdr, meta, slots)
}

// PrepareRaw prepares a page set received from a primary. It must contain
// page 0, whose header (including the sequence number) is taken verbatim.
func (p *Pager) PrepareRaw(pages map[uint32][]byte) (*Prepared, error) {
	p.mu.Lock()
	err := p.usable()
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	hdr, err := decodeHeader(pages[0])
	if err != nil {
		return nil, err
	}
	return p.prepare(pages, hdr, nil, nil)
}

// SetPreparedMeta sets the meta published with c, which must not have been
// flushed yet (for PrepareRaw, whose meta is derived from its own pages).
func (p *Pager) SetPreparedMeta(c *Prepared, meta any) {
	p.mu.Lock()
	c.Meta = meta
	p.mu.Unlock()
}

func (p *Pager) usable() error {
	if p.closed {
		return ErrClosed
	}
	return p.err
}

func (p *Pager) prepare(pages map[uint32][]byte, hdr Header, meta any, slots map[uint32]any) (*Prepared, error) {
	for _, b := range pages {
		seal(b) // the pages are owned by the commit and not yet visible
	}
	// Meta is set before the commit is queued: a concurrent Flush may
	// publish it at any time after that.
	c := &Prepared{pages: pages, hdr: hdr, Meta: meta, slots: slots}
	var offs map[uint32]int64
	end := p.walSize
	if p.file != nil {
		nos := make([]uint32, 0, len(pages))
		for no := range pages {
			nos = append(nos, no)
		}
		sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
		start := p.walSize
		buf := make([]byte, 0, 8+len(pages)*(4+PageSize)+20)
		if start == 0 {
			buf = append(buf, walMagic...)
			buf = binary.BigEndian.AppendUint32(buf, PageSize)
		}
		offs = make(map[uint32]int64, len(pages))
		crc := crc32.NewIEEE()
		for _, no := range nos {
			frameStart := len(buf)
			buf = binary.BigEndian.AppendUint32(buf, no)
			offs[no] = start + int64(len(buf))
			buf = append(buf, pages[no]...)
			crc.Write(buf[frameStart:])
		}
		buf = binary.BigEndian.AppendUint32(buf, commitMarker)
		tail := len(buf)
		buf = binary.BigEndian.AppendUint64(buf, hdr.Seq)
		crc.Write(buf[tail:])
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(pages)))
		buf = binary.BigEndian.AppendUint32(buf, crc.Sum32())
		if _, err := p.wal.WriteAt(buf, start); err != nil {
			p.wal.Truncate(start)
			return nil, err
		}
		end = start + int64(len(buf))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Register the versions now: the next writer reads them; readers do not
	// see them until the commit is published (their snapshots are older).
	for no, data := range pages {
		v := version{seq: hdr.Seq, data: data}
		if p.file != nil {
			v.off = offs[no]
			if p.verBytes+PageSize > versionMemBudget {
				v.data = nil
			} else {
				p.verBytes += PageSize
			}
		}
		p.versions[no] = append(p.versions[no], v)
		p.walPages++
	}
	p.walSize = end
	p.prepHdr = hdr
	if slots != nil {
		p.prepSlots = slots
	}
	p.pending = append(p.pending, c)
	return c, nil
}

// Flush makes c durable and visible. Commits are made durable in groups: one
// WAL fsync covers every commit prepared so far, and they are published in
// order. Safe to call from any goroutine.
func (p *Pager) Flush(c *Prepared) error {
	if c == nil {
		return nil
	}
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	p.mu.Lock()
	if p.hdr.Seq >= c.hdr.Seq {
		p.mu.Unlock()
		return nil // published by an earlier flush
	}
	err := p.usable()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.flushLocked()
}

// FlushAll makes every prepared commit durable and visible.
func (p *Pager) FlushAll() error { return p.flushPending() }

// flushPending makes every prepared commit durable (used on Close).
func (p *Pager) flushPending() error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	return p.flushLocked()
}

func (p *Pager) flushLocked() error {
	p.mu.Lock()
	batch := append([]*Prepared(nil), p.pending...)
	p.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if p.file != nil {
		err := p.wal.Sync()
		if err == nil && p.failpoint != nil {
			err = p.failpoint("wal-synced")
		}
		if err != nil {
			// Later commits were built on these; nothing can be published.
			p.mu.Lock()
			p.err = fmt.Errorf("bangdb: WAL sync failed, reopen the database: %w", err)
			p.mu.Unlock()
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range batch {
		p.hdr = c.hdr
		if c.Meta != nil {
			p.meta = c.Meta
		}
		if c.slots != nil {
			p.slots = c.slots
		}
		if p.onCommit != nil {
			p.onCommit(c.hdr.Seq, c.pages)
		}
	}
	// Drop references so the page images can be collected.
	for i := range batch {
		p.pending[i] = nil
	}
	p.pending = p.pending[len(batch):]
	if len(p.pending) == 0 {
		p.pending = nil
	}
	return nil
}

// Commit is PrepareCommit followed by Flush.
func (p *Pager) Commit(v *View) error {
	c, err := p.PrepareCommit(v, nil)
	if err != nil {
		return err
	}
	return p.Flush(c)
}

// CommitRaw is PrepareRaw followed by Flush.
func (p *Pager) CommitRaw(pages map[uint32][]byte) error {
	c, err := p.PrepareRaw(pages)
	if err != nil {
		return err
	}
	return p.Flush(c)
}

// NeedsCheckpoint reports whether the WAL has grown past CheckpointPages.
func (p *Pager) NeedsCheckpoint() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.walPages >= CheckpointPages
}

// WALPages returns the number of uncheckpointed page versions.
func (p *Pager) WALPages() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.walPages
}

// Checkpoint moves page versions into the database file (or the in-memory
// base), up to the oldest active snapshot. Writer-side: must not run
// concurrently with commits.
func (p *Pager) Checkpoint() error {
	p.mu.Lock()
	if err := p.usable(); err != nil {
		p.mu.Unlock()
		return err
	}
	target := p.hdr.Seq
	for s := range p.readers {
		target = min(target, s)
	}
	type item struct {
		no   uint32
		seq  uint64
		data []byte
	}
	var items []item
	for no, vs := range p.versions {
		for i := len(vs) - 1; i >= 0; i-- {
			if vs[i].seq <= target {
				items = append(items, item{no: no, seq: vs[i].seq})
				break
			}
		}
	}
	if len(items) == 0 {
		p.mu.Unlock()
		return nil
	}
	if p.file == nil {
		for _, it := range items {
			vs := p.versions[it.no]
			k := sort.Search(len(vs), func(i int) bool { return vs[i].seq > it.seq })
			p.base[it.no] = vs[k-1].data
			p.prune(it.no, it.seq)
		}
		p.mu.Unlock()
		return nil
	}
	for i := range items {
		vs := p.versions[items[i].no]
		k := sort.Search(len(vs), func(j int) bool { return vs[j].seq > items[i].seq })
		data, err := p.versionData(items[i].no, vs[k-1])
		if err != nil {
			p.mu.Unlock()
			return err
		}
		items[i].data = data
	}
	p.mu.Unlock()

	// No reader reads these pages from the file: each of them has a version
	// <= target in the WAL, and every snapshot is >= target.
	pages := make(map[uint32][]byte, len(items))
	for _, it := range items {
		pages[it.no] = it.data
	}
	if err := p.writePagesHeaderLast(pages); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range items {
		p.prune(it.no, it.seq)
		if _, ok := p.cache[it.no]; ok || len(p.cache) < p.cacheMax {
			p.cache[it.no] = it.data
		}
	}
	if len(p.versions) == 0 {
		// Everything is in the file; the file header now carries the latest
		// sequence, so stale WAL groups would be skipped even if the
		// truncation were lost in a crash.
		if err := p.wal.Truncate(0); err != nil {
			p.err = err
			return err
		}
		p.walSize, p.walPages, p.verBytes = 0, 0, 0
	}
	return nil
}

// writePagesHeaderLast writes pages to the database file so that a crash at
// any point leaves a recoverable file: all pages except the header are
// written and synced first, then the header (whose sequence number tells
// recovery which WAL groups are already in the file).
func (p *Pager) writePagesHeaderLast(pages map[uint32][]byte) error {
	nos := make([]uint32, 0, len(pages))
	for no := range pages {
		if no != 0 {
			nos = append(nos, no)
		}
	}
	sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
	for i, no := range nos {
		if _, err := p.file.WriteAt(pages[no], int64(no)*PageSize); err != nil {
			return err
		}
		if i == len(nos)/2 && p.failpoint != nil {
			if err := p.failpoint("checkpoint-write"); err != nil {
				return err
			}
		}
	}
	if err := p.file.Sync(); err != nil {
		return err
	}
	if hdr, ok := pages[0]; ok {
		if _, err := p.file.WriteAt(hdr, 0); err != nil {
			return err
		}
		if err := p.file.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// prune drops versions of page no with sequence <= seq. Requires p.mu.
func (p *Pager) prune(no uint32, seq uint64) {
	vs := p.versions[no]
	k := sort.Search(len(vs), func(i int) bool { return vs[i].seq > seq })
	for _, v := range vs[:k] {
		if p.file != nil && v.data != nil {
			p.verBytes -= PageSize
		}
	}
	p.walPages -= k
	if k == len(vs) {
		delete(p.versions, no)
	} else {
		p.versions[no] = append([]version(nil), vs[k:]...)
	}
}

// ---- snapshot copies (used to build backups and replicas) ----

// PutRaw writes a page directly, bypassing the WAL. It is only used while
// building a copy that nobody reads yet; FinishRaw makes it durable.
func (p *Pager) PutRaw(pgno uint32, data []byte) error {
	if len(data) != PageSize {
		return errors.New("pager: raw page of wrong size")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file == nil {
		p.base[pgno] = append([]byte(nil), data...)
		return nil
	}
	delete(p.cache, pgno)
	_, err := p.file.WriteAt(data, int64(pgno)*PageSize)
	return err
}

// FinishRaw completes a copy: it stamps the header with the given sequence
// number, syncs, and loads the header.
func (p *Pager) FinishRaw(seq uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b []byte
	if p.file == nil {
		b = p.base[0]
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
	if p.file == nil {
		p.base[0] = enc
	} else {
		if _, err := p.file.WriteAt(enc, 0); err != nil {
			return err
		}
		if err := p.file.Sync(); err != nil {
			return err
		}
		clear(p.cache)
	}
	p.hdr, p.prepHdr = hdr, hdr
	return nil
}

// ---- views ----

// View is a transactional overlay over one commit.
type View struct {
	p     *Pager
	dirty map[uint32][]byte
	hdr   Header
	seq   uint64 // commit whose state the view reads

	// Undo log for the active savepoint: the previous dirty entry of every
	// page touched since Savepoint was called.
	spGen    int
	undo     []undoEntry
	undoSeen map[uint32]struct{}

	lazy map[uint32]Lazy // pages held in decoded form (lazy.go)

	// Object slots (objects.go).
	baseObjs  map[uint32]any // slots of the commit the view reads; immutable
	dirtyObjs map[uint32]any // the view's changes (nil value = emptied)
	objUndo   []objUndo
	objSeen   map[uint32]struct{}
	token     *Token

	owned map[uint32]ownedObj // see SetOwned
}

type ownedObj struct {
	buf []byte
	obj any
}

type undoEntry struct {
	pg   uint32
	prev []byte
	had  bool
	lazy Lazy // the page was lazy: a copy of its state
}

// put sets a dirty page, creating the map of read views on first use.
func (v *View) put(pg uint32, b []byte) {
	if v.dirty == nil {
		v.dirty = map[uint32][]byte{}
	}
	v.dirty[pg] = b
	delete(v.lazy, pg)
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
	u := undoEntry{pg: pg, prev: prev, had: had}
	if l, ok := v.lazy[pg]; ok {
		u.lazy = l.Clone()
	}
	v.undo = append(v.undo, u)
}

// Read returns the page content as seen by this view. The returned slice
// must not be modified; use Write with a fresh buffer instead.
func (v *View) Read(pgno uint32) ([]byte, error) {
	if l, ok := v.lazy[pgno]; ok {
		b := l.Encode()
		v.put(pgno, b)
		v.SetOwned(pgno, b, l) // still the decoded form of the page
		return b, nil
	}
	if b, ok := v.dirty[pgno]; ok {
		return b, nil
	}
	if pgno >= v.hdr.PageCount {
		return nil, fmt.Errorf("bangdb: page %d out of range", pgno)
	}
	return v.p.readAt(pgno, v.seq)
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
	v.put(pgno, data)
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
		v.put(pg, make([]byte, PageSize))
		return pg, nil
	}
	pg := v.hdr.PageCount
	v.hdr.PageCount++
	v.track(pg)
	v.put(pg, make([]byte, PageSize))
	return pg, nil
}

// Free puts a page on the free list.
func (v *View) Free(pgno uint32) {
	b := make([]byte, PageSize)
	binary.BigEndian.PutUint32(b[0:4], v.hdr.FreeHead)
	v.track(pgno)
	v.put(pgno, b)
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

// FreeHead returns the first page of the free list as seen by the view.
func (v *View) FreeHead() uint32 { return v.hdr.FreeHead }

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
	v.objSavepoint()
	return Savepoint{hdr: v.hdr, gen: v.spGen}
}

// Restore rolls the view back to the most recent savepoint.
func (v *View) Restore(sp Savepoint) {
	if sp.gen != v.spGen {
		panic("pager: restore of a released savepoint")
	}
	for i := len(v.undo) - 1; i >= 0; i-- {
		u := v.undo[i]
		switch {
		case u.lazy != nil:
			v.WriteLazyNoTrack(u.pg, u.lazy)
		case u.had:
			v.put(u.pg, u.prev)
		default:
			delete(v.dirty, u.pg)
			delete(v.lazy, u.pg)
		}
	}
	v.undo = v.undo[:0]
	clear(v.undoSeen)
	v.objRestore()
	v.hdr = sp.hdr
}
