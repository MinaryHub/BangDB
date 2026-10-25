package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/knoxxr/bangdb/internal/pager"
)

// Physical replication.
//
// Every commit produces a Frame: the full images of the pages it changed,
// including the header page 0 that carries the commit sequence number. A copy
// of the database is brought up to date by applying frames in sequence order.
// Because frames hold whole pages, replaying them is idempotent, which makes
// "fuzzy" snapshots possible: pages can be copied while commits continue, as
// long as every frame committed during the copy is replayed afterwards.
//
//	backup / new replica:  SnapshotSubscribe -> CopyPages -> replay frames up to endSeq
//	mirroring:             Subscribe(seq) -> apply each frame as it arrives

var (
	// ErrReadOnly is returned for writes on a replica.
	ErrReadOnly = errors.New("bangdb: database is a read-only replica")
	// ErrLagging is returned when the replication log no longer holds the
	// frames a subscriber needs; it must start over from a snapshot.
	ErrLagging = errors.New("bangdb: replication log no longer contains the requested sequence; a new snapshot is required")
	// ErrSeqGap is returned when a frame does not follow the local sequence.
	ErrSeqGap = errors.New("bangdb: replication frame out of sequence")
)

// PageImage is one page of a frame.
type PageImage struct {
	No   uint32
	Data []byte
}

// Frame is one committed transaction as page images. Pages are sorted by
// number and include the header page 0.
type Frame struct {
	Seq   uint64
	Pages []PageImage
}

func (f Frame) size() int { return len(f.Pages) * (pager.PageSize + 4) }

const (
	// DefaultReplicationLog is the log size retained for reconnecting replicas
	// once a primary has been started.
	DefaultReplicationLog = 64 << 20
	minHardLog            = 256 << 20
)

// replLog keeps recent frames in memory for subscribers.
type replLog struct {
	mu       sync.Mutex
	frames   []Frame // ascending, contiguous sequence numbers
	bytes    int
	retain   int    // bytes kept for future reconnects (0 = only for active subscribers)
	lastSeq  uint64 // sequence of the last commit seen
	subs     map[*Subscription]struct{}
	notify   chan struct{}
	disabled bool // set when the DB closes
}

func newReplLog(seq uint64) *replLog {
	return &replLog{lastSeq: seq, subs: map[*Subscription]struct{}{}, notify: make(chan struct{})}
}

// onCommit is called by the pager with the commit lock held.
func (l *replLog) onCommit(seq uint64, pages map[uint32][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSeq = seq
	if len(l.subs) == 0 && l.retain == 0 {
		l.frames, l.bytes = nil, 0
		return
	}
	f := Frame{Seq: seq, Pages: make([]PageImage, 0, len(pages))}
	for no, data := range pages {
		f.Pages = append(f.Pages, PageImage{no, data}) // page slices are immutable
	}
	sort.Slice(f.Pages, func(i, j int) bool { return f.Pages[i].No < f.Pages[j].No })
	l.frames = append(l.frames, f)
	l.bytes += f.size()
	l.trim()
	close(l.notify)
	l.notify = make(chan struct{})
}

func (l *replLog) trim() {
	hard := max(4*l.retain, minHardLog)
	for len(l.frames) > 0 {
		first := l.frames[0]
		needed := false
		for s := range l.subs {
			if s.next <= first.Seq {
				needed = true
				break
			}
		}
		if (needed || l.bytes <= l.retain) && l.bytes <= hard {
			return
		}
		l.frames = l.frames[1:]
		l.bytes -= first.size()
	}
}

// Subscription is a cursor over committed frames.
type Subscription struct {
	l    *replLog
	next uint64
}

// Next blocks until the next frame is available.
func (s *Subscription) Next(ctx context.Context) (Frame, error) {
	for {
		s.l.mu.Lock()
		if s.l.disabled {
			s.l.mu.Unlock()
			return Frame{}, pager.ErrClosed
		}
		if _, ok := s.l.subs[s]; !ok {
			s.l.mu.Unlock()
			return Frame{}, errors.New("bangdb: subscription closed")
		}
		if s.next <= s.l.lastSeq {
			if len(s.l.frames) == 0 || s.next < s.l.frames[0].Seq {
				s.l.mu.Unlock()
				return Frame{}, ErrLagging
			}
			f := s.l.frames[s.next-s.l.frames[0].Seq]
			s.next++
			s.l.mu.Unlock()
			return f, nil
		}
		ch := s.l.notify
		s.l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// LastSeq returns the newest committed sequence known to the log.
func (s *Subscription) LastSeq() uint64 {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	return s.l.lastSeq
}

// Close releases the subscription.
func (s *Subscription) Close() {
	s.l.mu.Lock()
	delete(s.l.subs, s)
	s.l.trim()
	s.l.mu.Unlock()
}

// SetReplicationLog sets how many bytes of recent frames are kept in memory
// for replicas that reconnect. A replica further behind needs a snapshot.
func (db *DB) SetReplicationLog(bytes int) {
	db.repl.mu.Lock()
	db.repl.retain = bytes
	db.repl.trim()
	db.repl.mu.Unlock()
}

// Seq returns the sequence number of the last committed transaction.
func (db *DB) Seq() uint64 {
	db.commitMu.RLock()
	defer db.commitMu.RUnlock()
	return db.p.Header().Seq
}

// DBID returns the identity shared by a database and its replicas.
func (db *DB) DBID() [16]byte {
	db.commitMu.RLock()
	defer db.commitMu.RUnlock()
	return db.p.Header().DBID
}

// Subscribe returns a cursor delivering every frame after seq. It fails with
// ErrLagging if those frames are no longer retained.
func (db *DB) Subscribe(seq uint64) (*Subscription, error) {
	l := db.repl
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq > l.lastSeq {
		return nil, ErrLagging // the subscriber is ahead of us: diverged history
	}
	if seq < l.lastSeq && (len(l.frames) == 0 || seq+1 < l.frames[0].Seq) {
		return nil, ErrLagging
	}
	s := &Subscription{l: l, next: seq + 1}
	l.subs[s] = struct{}{}
	return s, nil
}

// SnapshotInfo describes a snapshot in progress.
type SnapshotInfo struct {
	DBID      [16]byte
	BaseSeq   uint64 // sequence at which the copy started
	PageCount uint32 // pages to copy
}

// SnapshotSubscribe atomically records the current state and subscribes to
// every later frame. Follow it with CopyPages.
func (db *DB) SnapshotSubscribe() (*Subscription, SnapshotInfo, error) {
	db.commitMu.RLock()
	defer db.commitMu.RUnlock()
	if db.isClosed() {
		return nil, SnapshotInfo{}, pager.ErrClosed
	}
	h := db.p.Header()
	sub, err := db.Subscribe(h.Seq)
	if err != nil {
		return nil, SnapshotInfo{}, err
	}
	return sub, SnapshotInfo{DBID: h.DBID, BaseSeq: h.Seq, PageCount: h.PageCount}, nil
}

// CopyPages copies pages [0, count) of the committed state to fn. Commits are
// only paused for one batch at a time, so the copy is "fuzzy"; it returns the
// sequence up to which frames must be replayed to make it consistent.
func (db *DB) CopyPages(count uint32, fn func(no uint32, data []byte) error) (endSeq uint64, err error) {
	const batch = 256
	for start := uint32(0); start < count; start += batch {
		pages := make([][]byte, 0, batch)
		db.commitMu.RLock()
		if db.isClosed() {
			db.commitMu.RUnlock()
			return 0, pager.ErrClosed
		}
		for no := start; no < min(start+batch, count); no++ {
			b, err := db.p.ReadCommitted(no)
			if err != nil {
				db.commitMu.RUnlock()
				return 0, err
			}
			pages = append(pages, b)
		}
		endSeq = db.p.Header().Seq
		db.commitMu.RUnlock()
		for i, b := range pages {
			if err := fn(start+uint32(i), b); err != nil {
				return 0, err
			}
		}
	}
	if count == 0 {
		endSeq = db.Seq()
	}
	return endSeq, nil
}

// SetReadOnly turns replica mode on or off. In replica mode all writes fail
// with ErrReadOnly; only ApplyFrame and InstallSnapshot change the data.
func (db *DB) SetReadOnly(ro bool) { db.readOnly.Store(ro) }

// Promote turns a replica into a writable primary. The database gets a new
// identity so that other replicas re-synchronize from a fresh snapshot
// instead of mixing histories.
func (db *DB) Promote() error {
	db.readOnly.Store(false)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	tx.v.NewIdentity()
	return tx.Commit()
}

// Path returns the database file path ("" for in-memory databases).
func (db *DB) Path() string { return db.path }

// ReadOnly reports whether the database is in replica mode.
func (db *DB) ReadOnly() bool { return db.readOnly.Load() }

// ApplyFrame applies a frame from the primary. Frames at or below the local
// sequence are ignored (idempotent re-delivery).
func (db *DB) ApplyFrame(f Frame) error {
	if err := db.lockWrite(); err != nil {
		return err
	}
	defer db.unlockWrite()
	db.commitMu.Lock()
	defer db.commitMu.Unlock()
	if db.isClosed() {
		return pager.ErrClosed
	}
	if err := applyFrame(db.p, f); err != nil {
		return err
	}
	return db.reloadCatalogLocked()
}

func applyFrame(p *pager.Pager, f Frame) error {
	cur := p.Header().Seq
	if f.Seq <= cur {
		return nil
	}
	if f.Seq != cur+1 {
		return fmt.Errorf("%w: have %d, got %d", ErrSeqGap, cur, f.Seq)
	}
	pages := make(map[uint32][]byte, len(f.Pages))
	for _, pg := range f.Pages {
		if len(pg.Data) != pager.PageSize {
			return errors.New("bangdb: replication frame has a page of wrong size")
		}
		pages[pg.No] = pg.Data
	}
	if _, ok := pages[0]; !ok {
		return errors.New("bangdb: replication frame without header page")
	}
	return p.CommitRaw(pages)
}

func (db *DB) reloadCatalogLocked() error {
	cat, err := loadCatalog(db.p.NewView())
	if err != nil {
		return err
	}
	db.catMu.Lock()
	db.cat = cat
	db.catMu.Unlock()
	return nil
}

// SnapshotWriter builds a database copy from a snapshot stream: pages first
// (WritePage), then Finish, then frames (Apply) until Seq() reaches the
// snapshot's end sequence.
type SnapshotWriter struct {
	p    *pager.Pager
	path string // "" for in-memory
	tmp  string
}

// NewSnapshotWriter creates an empty copy at path ("" or ":memory:" = memory).
// The file is written to path+".snapshot" until it is installed.
func NewSnapshotWriter(path string) (*SnapshotWriter, error) {
	if path == "" || path == ":memory:" {
		return &SnapshotWriter{p: pager.OpenMemory()}, nil
	}
	tmp := path + ".snapshot"
	removeDBFiles(tmp)
	p, err := pager.Open(tmp)
	if err != nil {
		return nil, err
	}
	return &SnapshotWriter{p: p, path: path, tmp: tmp}, nil
}

func removeDBFiles(path string) {
	os.Remove(path)
	os.Remove(path + "-wal")
}

// WritePage stores one snapshot page.
func (w *SnapshotWriter) WritePage(no uint32, data []byte) error { return w.p.PutRaw(no, data) }

// Finish marks the end of the page copy; baseSeq is the snapshot's BaseSeq.
func (w *SnapshotWriter) Finish(baseSeq uint64) error { return w.p.FinishRaw(baseSeq) }

// Apply replays a frame committed during the copy.
func (w *SnapshotWriter) Apply(f Frame) error { return applyFrame(w.p, f) }

// Seq returns the sequence the copy has reached.
func (w *SnapshotWriter) Seq() uint64 { return w.p.Header().Seq }

// Abort discards the copy.
func (w *SnapshotWriter) Abort() {
	w.p.Close()
	if w.tmp != "" {
		removeDBFiles(w.tmp)
	}
}

// Close finishes a standalone copy (a backup file) and moves it to its
// final path.
func (w *SnapshotWriter) Close() error {
	if err := w.p.Close(); err != nil {
		return err
	}
	if w.tmp == "" {
		return nil
	}
	os.Remove(w.tmp + "-wal")
	return os.Rename(w.tmp, w.path)
}

// InstallSnapshot replaces this database's content with a completed copy.
// The copy must have been created for this database's own path.
func (db *DB) InstallSnapshot(w *SnapshotWriter) error {
	if w.path != db.path && !(w.path == "" && db.path == "") {
		return errors.New("bangdb: snapshot was built for a different path")
	}
	if err := db.lockWrite(); err != nil {
		return err
	}
	defer db.unlockWrite()
	db.commitMu.Lock()
	defer db.commitMu.Unlock()
	if db.isClosed() {
		return pager.ErrClosed
	}
	if w.tmp == "" {
		db.p.Close()
		db.p = w.p
	} else {
		if err := w.p.Close(); err != nil {
			return err
		}
		if err := db.p.Close(); err != nil {
			return err
		}
		os.Remove(w.tmp + "-wal")
		os.Remove(db.path + "-wal")
		if err := os.Rename(w.tmp, db.path); err != nil {
			return err
		}
		p, err := pager.Open(db.path)
		if err != nil {
			return err // db.p stays closed: later calls fail with ErrClosed
		}
		db.p = p
	}
	db.p.SetOnCommit(db.repl.onCommit)
	db.repl.mu.Lock()
	db.repl.lastSeq = db.p.Header().Seq
	db.repl.frames, db.repl.bytes = nil, 0
	db.repl.mu.Unlock()
	return db.reloadCatalogLocked()
}

// Backup writes a consistent copy of the database to path while the database
// stays fully usable: commits are paused only for short page batches. The
// result is a regular database file that can be opened directly.
func (db *DB) Backup(ctx context.Context, path string) error {
	if path == "" || path == ":memory:" || path == db.path {
		return errors.New("bangdb: invalid backup path")
	}
	sub, info, err := db.SnapshotSubscribe()
	if err != nil {
		return err
	}
	defer sub.Close()
	w, err := NewSnapshotWriter(path)
	if err != nil {
		return err
	}
	endSeq, err := db.CopyPages(info.PageCount, w.WritePage)
	if err == nil {
		err = w.Finish(info.BaseSeq)
	}
	for err == nil && w.Seq() < endSeq {
		var f Frame
		if f, err = sub.Next(ctx); err == nil {
			err = w.Apply(f)
		}
	}
	if err != nil {
		w.Abort()
		return err
	}
	return w.Close()
}
