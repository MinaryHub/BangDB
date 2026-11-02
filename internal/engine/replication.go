package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/knoxxr/bangdb/internal/pager"
)

// Physical replication.
//
// Every commit produces a Frame: the full images of the pages it changed,
// including the header page 0 that carries the commit sequence number. A copy
// of the database is brought up to date by applying frames in sequence order.
// Full snapshots are read from a pager snapshot, so they are consistent and
// never pause commits.
//
//	backup:       OpenSnapshot -> CopyPages
//	new replica:  OpenSnapshot(subscribe) -> CopyPages -> apply frames after BaseSeq
//	mirroring:    Subscribe(seq) -> apply each frame as it arrives

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
	Time  time.Time // when the primary committed it (zero if unknown)
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
	f := Frame{Seq: seq, Time: time.Now(), Pages: make([]PageImage, 0, len(pages))}
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
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
	return db.p.Header().Seq
}

// DBID returns the identity shared by a database and its replicas.
func (db *DB) DBID() [16]byte {
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
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

// SnapshotInfo describes a snapshot.
type SnapshotInfo struct {
	DBID      [16]byte
	BaseSeq   uint64 // commit the snapshot shows
	PageCount uint32 // pages to copy
}

// Snapshot is a consistent, pinned view of one commit used to copy the whole
// database. Commits continue while it is open; the WAL cannot be
// checkpointed past it, so it should not be held longer than needed.
type Snapshot struct {
	Info SnapshotInfo
	// Sub delivers every commit after Info.BaseSeq (only if requested). It
	// stays valid after Close and must be closed by its owner.
	Sub *Subscription

	db   *DB
	snap *pager.Snapshot
}

// OpenSnapshot pins the latest commit. With subscribe, it also atomically
// subscribes to every later commit, which is how a replica is bootstrapped.
func (db *DB) OpenSnapshot(subscribe bool) (*Snapshot, error) {
	db.swapMu.RLock()
	if db.isClosed() {
		db.swapMu.RUnlock()
		return nil, pager.ErrClosed
	}
	var sub *Subscription
	ps, err := db.p.SnapshotWith(func(seq uint64) error {
		if !subscribe {
			return nil
		}
		var err error
		sub, err = db.Subscribe(seq)
		return err
	})
	if err != nil {
		db.swapMu.RUnlock()
		return nil, err
	}
	h := ps.Header()
	return &Snapshot{Info: SnapshotInfo{DBID: h.DBID, BaseSeq: ps.Seq(), PageCount: h.PageCount},
		Sub: sub, db: db, snap: ps}, nil
}

// CopyPages passes every page of the snapshot to fn in order.
func (s *Snapshot) CopyPages(fn func(no uint32, data []byte) error) error {
	v := s.snap.View()
	for no := uint32(0); no < s.Info.PageCount; no++ {
		b, err := v.Read(no)
		if err != nil {
			return err
		}
		if err := fn(no, b); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the snapshot (but not Sub).
func (s *Snapshot) Close() {
	if s.snap != nil {
		s.snap.Release()
		s.snap = nil
		s.db.swapMu.RUnlock()
	}
}

// SetReadOnly turns replica mode on or off. In replica mode all writes fail
// with ErrReadOnly; only ApplyFrame and InstallSnapshot change the data.
func (db *DB) SetReadOnly(ro bool) { db.readOnly.Store(ro) }

// Promote turns a replica into a writable primary. The database gets a new
// identity so that other replicas re-synchronize from a fresh snapshot
// instead of mixing histories.
//
// Promotion also starts a new term (see Term), which fences the old
// primary off from replicas that follow the new one.
func (db *DB) Promote() error {
	db.readOnly.Store(false)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	tx.v.NewIdentity()
	tx.v.SetMeta(metaTerm, tx.v.Meta(metaTerm)+1)
	return tx.Commit()
}

// metaTerm is the header meta slot holding the replication term.
const metaTerm = 2

// Term returns the replication term: 0 for a database that was never
// promoted, +1 for every promotion in its history. Replicas share their
// primary's term. A primary that meets a replica with a higher term has
// been replaced and stops accepting writes; applications can also pass the
// term to external systems as a fencing token.
func (db *DB) Term() uint64 {
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
	return uint64(db.p.Header().Meta[metaTerm])
}

// SetCommitWaiter installs fn, called after every local commit is durable
// and before the commit returns, with the commit's sequence number (nil
// removes it). Semi-synchronous replication uses it to wait for replicas.
func (db *DB) SetCommitWaiter(fn func(seq uint64)) {
	if fn == nil {
		db.commitWaiter.Store(nil)
		return
	}
	db.commitWaiter.Store(&fn)
}

// Path returns the database file path ("" for in-memory databases).
func (db *DB) Path() string { return db.path }

// ReadOnly reports whether the database is in replica mode.
func (db *DB) ReadOnly() bool { return db.readOnly.Load() }

// ApplyFrame applies a frame from the primary. Frames at or below the local
// sequence are ignored (idempotent re-delivery). Readers are not blocked.
func (db *DB) ApplyFrame(f Frame) error {
	if err := db.ApplyFrameNoSync(f); err != nil {
		return err
	}
	return db.FlushFrames()
}

// ApplyFrameNoSync is ApplyFrame without making the frame durable and
// visible: a replica applying a burst of frames calls FlushFrames once at
// the end (one WAL fsync for all of them, as group commit does).
func (db *DB) ApplyFrameNoSync(f Frame) error {
	if err := db.lockWrite(context.Background()); err != nil {
		return err
	}
	defer db.unlockWrite()
	if db.isClosed() {
		return pager.ErrClosed
	}
	pages, err := framePages(db.p, f)
	if err != nil || pages == nil {
		return err
	}
	c, err := db.p.PrepareRaw(pages)
	if err != nil {
		return err
	}
	cat, err := loadCatalog(db.p.NewView())
	if err != nil {
		return err
	}
	db.p.SetPreparedMeta(c, cat)
	db.prepared(cat)
	return nil
}

// FlushFrames makes the frames applied so far durable and visible.
func (db *DB) FlushFrames() error {
	db.swapMu.RLock()
	defer db.swapMu.RUnlock()
	if db.isClosed() {
		return pager.ErrClosed
	}
	return db.p.FlushAll()
}

// framePages validates f against the local sequence; nil means "already
// applied".
func framePages(p *pager.Pager, f Frame) (map[uint32][]byte, error) {
	cur := p.PreparedHeader().Seq
	if f.Seq <= cur {
		return nil, nil
	}
	if f.Seq != cur+1 {
		return nil, fmt.Errorf("%w: have %d, got %d", ErrSeqGap, cur, f.Seq)
	}
	pages := make(map[uint32][]byte, len(f.Pages))
	for _, pg := range f.Pages {
		if len(pg.Data) != pager.PageSize {
			return nil, errors.New("bangdb: replication frame has a page of wrong size")
		}
		pages[pg.No] = pg.Data
	}
	if _, ok := pages[0]; !ok {
		return nil, errors.New("bangdb: replication frame without header page")
	}
	return pages, nil
}

func applyFrame(p *pager.Pager, f Frame) error {
	pages, err := framePages(p, f)
	if err != nil || pages == nil {
		return err
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
	db.p.SetMeta(cat)
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
func NewSnapshotWriter(path string) (*SnapshotWriter, error) { return newSnapshotWriter(path, nil) }

// NewSnapshotWriter is the package function NewSnapshotWriter for a copy
// that replaces this database (a replica's resynchronization): the copy is
// encrypted with the database's key.
func (db *DB) NewSnapshotWriter(path string) (*SnapshotWriter, error) {
	return newSnapshotWriter(path, db.key)
}

func newSnapshotWriter(path string, key []byte) (*SnapshotWriter, error) {
	if path == "" || path == ":memory:" {
		return &SnapshotWriter{p: pager.OpenMemory()}, nil
	}
	tmp := path + ".snapshot"
	removeDBFiles(tmp)
	p, err := pager.OpenEncrypted(tmp, key)
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
	if err := renameFile(w.tmp, w.path); err != nil {
		return err
	}
	return pager.SyncDir(w.path)
}

// InstallSnapshot replaces this database's content with a completed copy.
// The copy must have been created for this database's own path.
func (db *DB) InstallSnapshot(w *SnapshotWriter) error {
	if w.path != db.path && !(w.path == "" && db.path == "") {
		return errors.New("bangdb: snapshot was built for a different path")
	}
	if err := db.lockWrite(context.Background()); err != nil {
		return err
	}
	defer db.unlockWrite()
	return db.installLocked(w)
}

// installLocked swaps in a completed copy; the caller holds the write lock.
// Subscribers are disconnected: their replicas must re-synchronize.
func (db *DB) installLocked(w *SnapshotWriter) error {
	db.swapMu.Lock() // waits for running readers of the old data
	defer db.swapMu.Unlock()
	if db.isClosed() {
		return pager.ErrClosed
	}
	if w.tmp == "" {
		db.p.Close()
		db.p = w.p
	} else {
		slots := w.p.Objects() // volatile stores carried by a VACUUM rebuild
		if err := w.p.Close(); err != nil {
			return err
		}
		if err := db.p.Close(); err != nil {
			return err
		}
		os.Remove(w.tmp + "-wal")
		os.Remove(db.path + "-wal")
		renameErr := renameFile(w.tmp, db.path)
		if renameErr == nil {
			renameErr = pager.SyncDir(db.path)
		}
		// Reopen the file: the new copy, or the old data if the swap
		// failed (then the copy is dropped and the error reported).
		p, err := pager.OpenEncrypted(db.path, db.key)
		if err != nil {
			return errors.Join(renameErr, err) // db.p stays closed: later calls fail with ErrClosed
		}
		if renameErr != nil {
			removeDBFiles(w.tmp)
			db.p = p
			db.p.SetOnCommit(db.repl.onCommit)
			db.p.SetCacheSize(db.cacheSize)
			return fmt.Errorf("bangdb: installing the new copy: %w", renameErr)
		}
		p.SetObjects(slots)
		db.p = p
	}
	db.p.SetOnCommit(db.repl.onCommit)
	db.p.SetCacheSize(db.cacheSize)
	db.repl.mu.Lock()
	db.repl.lastSeq = db.p.Header().Seq
	db.repl.frames, db.repl.bytes = nil, 0
	clear(db.repl.subs)
	close(db.repl.notify)
	db.repl.notify = make(chan struct{})
	db.repl.mu.Unlock()
	return db.reloadCatalogLocked()
}

// Backup writes a consistent copy of the database to path while the database
// stays fully usable: it copies a pinned snapshot, so neither readers nor
// writers wait. The result is a regular database file that can be opened
// directly.
func (db *DB) Backup(ctx context.Context, path string) error {
	return db.BackupWithKey(ctx, path, db.key)
}

// BackupWithKey is Backup with the copy encrypted with key (nil: a
// plaintext copy). With it a database can be encrypted, decrypted or given
// a new key: back it up with the new key and use the copy.
func (db *DB) BackupWithKey(ctx context.Context, path string, key []byte) error {
	if path == "" || path == ":memory:" || path == db.path {
		return errors.New("bangdb: invalid backup path")
	}
	snap, err := db.OpenSnapshot(false)
	if err != nil {
		return err
	}
	defer snap.Close()
	w, err := newSnapshotWriter(path, key)
	if err != nil {
		return err
	}
	err = snap.CopyPages(func(no uint32, data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return w.WritePage(no, data)
	})
	if err == nil {
		err = w.Finish(snap.Info.BaseSeq)
	}
	if err != nil {
		w.Abort()
		return err
	}
	return w.Close()
}

// renameFile replaces to with from. On Windows a just-written file can be
// held briefly by another program (indexer, antivirus), which makes the
// rename fail with "access denied": retry for a while.
func renameFile(from, to string) error {
	var err error
	for delay := 10 * time.Millisecond; ; delay *= 2 {
		if err = os.Rename(from, to); err == nil || runtime.GOOS != "windows" || delay > 2*time.Second {
			return err
		}
		time.Sleep(delay)
	}
}

// VerifyBackup opens the database file at path (a backup of this
// database, so with its encryption key) and runs the integrity check.
func (db *DB) VerifyBackup(ctx context.Context, path string) ([]string, error) {
	b, err := Open(path, &Options{EncryptionKey: db.key})
	if err != nil {
		return nil, err
	}
	problems, err := b.IntegrityCheck(ctx)
	if cerr := b.Close(); err == nil {
		err = cerr
	}
	// Opening created an (empty) WAL: keep the backup a single file.
	if st, serr := os.Stat(path + "-wal"); serr == nil && st.Size() == 0 {
		os.Remove(path + "-wal")
	}
	return problems, err
}
