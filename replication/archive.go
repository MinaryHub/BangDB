package replication

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
)

// Point-in-time recovery.
//
// An Archiver keeps, in a directory, base backups of a database and every
// commit made after them (the same frames replicas receive), in segment
// files. Restore rebuilds the database as of any archived commit or point
// in time: it copies the latest base backup before the target and replays
// the archived commits up to it.
//
// Files (DBID = database identity in hex; a new identity, after VACUUM or
// promotion, starts a new chain):
//
//	base-<DBID>-<seq>-<unixnano>.db   a backup as of commit seq (encrypted
//	                                  like the database)
//	seg-<DBID>-<firstSeq>.bfa         "BFA1" flags(1), then records
//	                                  len(4) payload crc32(4); a payload is
//	                                  a frame as on the wire, or with
//	                                  ArchiveOptions.Key nonce(12) +
//	                                  AES-GCM(frame)

// ArchiveOptions configures an Archiver.
type ArchiveOptions struct {
	// SegmentBytes starts a new segment file after this many bytes.
	// Default: 64 MiB.
	SegmentBytes int64
	// BaseEvery takes a new base backup after this many bytes of commits
	// were archived since the last one (bounding restore time). Default:
	// 1 GiB; negative: only when needed.
	BaseEvery int64
	// KeepBases deletes all but the newest KeepBases base backups, with the
	// segments only they needed. 0 keeps everything.
	KeepBases int
	// SyncInterval is the longest time an archived commit may stay
	// unsynced on disk. Default: 1s.
	SyncInterval time.Duration
	// Key encrypts segment records with AES-256-GCM (base backups use the
	// database's own encryption). Restore needs the same key.
	Key []byte
}

// ArchiveStatus describes an archiver.
type ArchiveStatus struct {
	Running   bool
	Seq       uint64 // last archived commit
	Bases     int    // base backups taken by this archiver
	LastError string
}

// Archiver continuously archives a database for point-in-time recovery.
type Archiver struct {
	db     *bangdb.DB
	e      *engine.DB
	dir    string
	opts   ArchiveOptions
	gcm    cipher.AEAD
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	status ArchiveStatus

	seg        *os.File
	segW       *bufio.Writer
	segBytes   int64
	sinceBase  int64
	dirty      bool
	pendingSeq uint64 // last commit written to the segment
}

// StartArchiver starts archiving db into dir (created if needed).
func StartArchiver(db *bangdb.DB, dir string, opts ArchiveOptions) (*Archiver, error) {
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = 64 << 20
	}
	if opts.BaseEvery == 0 {
		opts.BaseEvery = 1 << 30
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = time.Second
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &Archiver{db: db, e: db.Engine(), dir: dir, opts: opts, done: make(chan struct{})}
	if opts.Key != nil {
		var err error
		if a.gcm, err = newArchiveGCM(opts.Key); err != nil {
			return nil, err
		}
	}
	// Keep recent frames for the archiver even when no replica is attached.
	a.e.SetReplicationLog(engine.DefaultReplicationLog)
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.status.Running = true
	go a.run()
	return a, nil
}

func newArchiveGCM(key []byte) (cipher.AEAD, error) {
	if len(key) < 16 {
		return nil, errors.New("replication: archive key must be at least 16 bytes")
	}
	k := sha256.Sum256(append([]byte("bangdb archive\x00"), key...))
	b, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Status returns the archiver's state.
func (a *Archiver) Status() ArchiveStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// WaitFor blocks until commit seq is archived and synced.
func (a *Archiver) WaitFor(ctx context.Context, seq uint64) error {
	for {
		a.mu.Lock()
		cur, running := a.status.Seq, a.status.Running
		a.mu.Unlock()
		if cur >= seq {
			return nil
		}
		if !running {
			return errors.New("replication: archiver stopped")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Stop finishes the current segment and stops archiving.
func (a *Archiver) Stop() error {
	a.cancel()
	<-a.done
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status.LastError != "" {
		return errors.New(a.status.LastError)
	}
	return nil
}

func (a *Archiver) setErr(err error) {
	a.mu.Lock()
	a.status.LastError = err.Error()
	a.mu.Unlock()
}

func (a *Archiver) run() {
	defer close(a.done)
	defer func() {
		a.closeSegment()
		a.mu.Lock()
		a.status.Running = false
		a.mu.Unlock()
	}()
	backoff := 100 * time.Millisecond
	for a.ctx.Err() == nil {
		err := a.session()
		if a.ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 100 * time.Millisecond
			continue
		}
		a.setErr(err)
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// session archives until the subscription breaks (a new identity, lag, ...).
func (a *Archiver) session() error {
	a.closeSegment()
	dbid := a.e.DBID()
	var sub *engine.Subscription
	if last, ok := a.lastArchived(dbid); ok {
		if s, err := a.e.Subscribe(last); err == nil {
			sub = s
			a.mu.Lock()
			a.status.Seq = max(a.status.Seq, last)
			a.mu.Unlock()
		}
	}
	if sub == nil {
		// Nothing usable archived: start a chain with a base backup.
		snap, err := a.e.OpenSnapshot(true)
		if err != nil {
			return err
		}
		err = a.writeBase(snap)
		snap.Close()
		if err != nil {
			snap.Sub.Close()
			return err
		}
		sub, dbid = snap.Sub, snap.Info.DBID
		a.mu.Lock()
		a.status.Seq = snap.Info.BaseSeq
		a.mu.Unlock()
	}
	defer sub.Close()
	for {
		ctx, cancel := context.WithTimeout(a.ctx, a.opts.SyncInterval)
		f, err := sub.Next(ctx)
		cancel()
		switch {
		case err == nil:
			if err := a.append(dbid, f); err != nil {
				return err
			}
			continue
		case errors.Is(err, context.DeadlineExceeded) && a.ctx.Err() == nil:
			if err := a.sync(); err != nil {
				return err
			}
			if a.opts.BaseEvery > 0 && a.sinceBase >= a.opts.BaseEvery {
				if err := a.newBase(); err != nil {
					return err
				}
			}
			continue
		}
		if a.ctx.Err() != nil {
			return a.sync()
		}
		// Lagging, or the database got a new identity (VACUUM, promotion):
		// start over, with a new base backup if needed.
		return a.sync()
	}
}

// writeBase stores a base backup from snap.
func (a *Archiver) writeBase(snap *engine.Snapshot) error {
	name := fmt.Sprintf("base-%s-%020d-%d.db", hex.EncodeToString(snap.Info.DBID[:]), snap.Info.BaseSeq, time.Now().UnixNano())
	path := filepath.Join(a.dir, name)
	w, err := a.e.NewSnapshotWriter(path)
	if err != nil {
		return err
	}
	err = snap.CopyPages(func(no uint32, data []byte) error {
		if err := a.ctx.Err(); err != nil {
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
	if err := w.Close(); err != nil {
		return err
	}
	a.sinceBase = 0
	a.mu.Lock()
	a.status.Bases++
	a.mu.Unlock()
	return a.prune()
}

// newBase takes another base backup while archiving continues.
func (a *Archiver) newBase() error {
	snap, err := a.e.OpenSnapshot(false)
	if err != nil {
		return err
	}
	defer snap.Close()
	return a.writeBase(snap)
}

func (a *Archiver) append(dbid [16]byte, f engine.Frame) error {
	if a.seg == nil || a.segBytes >= a.opts.SegmentBytes {
		if err := a.closeSegment(); err != nil {
			return err
		}
		name := fmt.Sprintf("seg-%s-%020d.bfa", hex.EncodeToString(dbid[:]), f.Seq)
		file, err := os.OpenFile(filepath.Join(a.dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		a.seg, a.segW, a.segBytes = file, bufio.NewWriterSize(file, 256<<10), 0
		flags := byte(0)
		if a.gcm != nil {
			flags = 1
		}
		a.segW.WriteString("BFA1")
		a.segW.WriteByte(flags)
		a.segBytes = 5
	}
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	writeFrame(bw, f)
	bw.Flush()
	payload := buf.Bytes()
	if a.gcm != nil {
		nonce := make([]byte, a.gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		payload = a.gcm.Seal(nonce, nonce, payload, nil)
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(payload)))
	a.segW.Write(n[:])
	a.segW.Write(payload)
	binary.BigEndian.PutUint32(n[:], crc32.ChecksumIEEE(payload))
	if _, err := a.segW.Write(n[:]); err != nil {
		return err
	}
	a.segBytes += int64(len(payload) + 8)
	a.sinceBase += int64(len(payload))
	a.dirty = true
	a.pendingSeq = f.Seq
	return nil
}

// sync makes the archived commits durable and reports them.
func (a *Archiver) sync() error {
	if a.seg == nil || !a.dirty {
		return nil
	}
	if err := a.segW.Flush(); err != nil {
		return err
	}
	if err := a.seg.Sync(); err != nil {
		return err
	}
	a.dirty = false
	a.mu.Lock()
	a.status.Seq = max(a.status.Seq, a.pendingSeq)
	a.mu.Unlock()
	return nil
}

func (a *Archiver) closeSegment() error {
	if a.seg == nil {
		return nil
	}
	err := a.sync()
	if cerr := a.seg.Close(); err == nil {
		err = cerr
	}
	a.seg, a.segW = nil, nil
	return err
}

// lastArchived returns the last commit archived for dbid, if a chain
// exists (a base and the segments after it).
func (a *Archiver) lastArchived(dbid [16]byte) (uint64, bool) {
	files, err := listArchive(a.dir)
	if err != nil {
		return 0, false
	}
	var last uint64
	found := false
	for _, b := range files.bases {
		if b.dbid == dbid {
			last, found = max(last, b.seq), true
		}
	}
	if !found {
		return 0, false
	}
	for _, s := range files.segments {
		if s.dbid != dbid {
			continue
		}
		err := readSegment(s.path, a.gcm, func(f engine.Frame) (bool, error) {
			last = max(last, f.Seq)
			return true, nil
		})
		if err != nil && !errors.Is(err, errTornRecord) {
			return 0, false
		}
	}
	return last, true
}

// prune applies KeepBases.
func (a *Archiver) prune() error {
	if a.opts.KeepBases <= 0 {
		return nil
	}
	files, err := listArchive(a.dir)
	if err != nil {
		return err
	}
	if len(files.bases) <= a.opts.KeepBases {
		return nil
	}
	// Bases are sorted by time; keep the newest.
	keep := files.bases[len(files.bases)-a.opts.KeepBases:]
	oldest := keep[0]
	for _, b := range files.bases[:len(files.bases)-a.opts.KeepBases] {
		os.Remove(b.path)
	}
	// Segments of chains without a kept base go; in the oldest kept
	// base's chain, so do segments that end before that base (the next
	// segment of the chain starts at or before the base's next commit).
	chains := map[[16]byte]bool{}
	for _, b := range keep {
		chains[b.dbid] = true
	}
	segs := files.segments
	for i, sg := range segs {
		if !chains[sg.dbid] {
			os.Remove(sg.path)
			continue
		}
		if sg.dbid == oldest.dbid && i+1 < len(segs) && segs[i+1].dbid == sg.dbid && segs[i+1].first <= oldest.seq+1 {
			os.Remove(sg.path)
		}
	}
	return nil
}

// ---- archive files ----

type baseFile struct {
	path string
	dbid [16]byte
	seq  uint64
	time time.Time
}

type segFile struct {
	path  string
	dbid  [16]byte
	first uint64
}

type archiveFiles struct {
	bases    []baseFile // by time
	segments []segFile  // by dbid, then first sequence
}

func listArchive(dir string) (archiveFiles, error) {
	var out archiveFiles
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "base-") && strings.HasSuffix(name, ".db"):
			parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, "base-"), ".db"), "-")
			if len(parts) != 3 {
				continue
			}
			id, ok := parseDBID(parts[0])
			seq, err1 := strconv.ParseUint(parts[1], 10, 64)
			ns, err2 := strconv.ParseInt(parts[2], 10, 64)
			if !ok || err1 != nil || err2 != nil {
				continue
			}
			out.bases = append(out.bases, baseFile{filepath.Join(dir, name), id, seq, time.Unix(0, ns)})
		case strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".bfa"):
			parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".bfa"), "-")
			if len(parts) != 2 {
				continue
			}
			id, ok := parseDBID(parts[0])
			first, err := strconv.ParseUint(parts[1], 10, 64)
			if !ok || err != nil {
				continue
			}
			out.segments = append(out.segments, segFile{filepath.Join(dir, name), id, first})
		}
	}
	sort.Slice(out.bases, func(i, j int) bool { return out.bases[i].time.Before(out.bases[j].time) })
	sort.Slice(out.segments, func(i, j int) bool {
		a, b := out.segments[i], out.segments[j]
		if c := bytes.Compare(a.dbid[:], b.dbid[:]); c != 0 {
			return c < 0
		}
		return a.first < b.first
	})
	return out, nil
}

func parseDBID(s string) ([16]byte, bool) {
	var id [16]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return id, false
	}
	copy(id[:], b)
	return id, true
}

// errTornRecord marks an incomplete last record (a crash while appending).
var errTornRecord = errors.New("replication: torn archive record")

// readSegment passes the frames of a segment to fn until it returns false.
func readSegment(path string, gcm cipher.AEAD, fn func(engine.Frame) (bool, error)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	r := bufio.NewReaderSize(file, 256<<10)
	head := make([]byte, 5)
	if _, err := io.ReadFull(r, head); err != nil {
		return errTornRecord // created but nothing synced yet
	}
	if string(head[:4]) != "BFA1" {
		return fmt.Errorf("replication: %s is not an archive segment", path)
	}
	if (head[4] == 1) != (gcm != nil) {
		if gcm == nil {
			return fmt.Errorf("replication: %s is encrypted; the archive key is required", path)
		}
		return fmt.Errorf("replication: %s is not encrypted", path)
	}
	for {
		var n [4]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return errTornRecord
		}
		size := binary.BigEndian.Uint32(n[:])
		if size > 1<<30 {
			return errTornRecord
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return errTornRecord
		}
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return errTornRecord
		}
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(n[:]) {
			return errTornRecord
		}
		if gcm != nil {
			if len(payload) < gcm.NonceSize() {
				return errTornRecord
			}
			if payload, err = gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], nil); err != nil {
				return fmt.Errorf("replication: %s: wrong archive key or tampered record", path)
			}
		}
		br := bytes.NewReader(payload)
		if typ, err := br.ReadByte(); err != nil || typ != msgFrame {
			return fmt.Errorf("replication: %s: bad record", path)
		}
		f, err := readFrame(br)
		if err != nil {
			return fmt.Errorf("replication: %s: %w", path, err)
		}
		if cont, err := fn(f); err != nil || !cont {
			return err
		}
	}
}

// ---- restore ----

// RestoreOptions selects the point to restore to. With neither Seq nor
// Time set, everything archived is restored.
type RestoreOptions struct {
	Seq  uint64    // restore up to and including this commit
	Time time.Time // restore the commits made at or before this time
	// Key is the database encryption key of the archived database (base
	// backups are encrypted with it); the restored file uses it too.
	Key []byte
	// ArchiveKey is ArchiveOptions.Key, if the segments are encrypted.
	ArchiveKey []byte
}

// RestoreResult describes a restore.
type RestoreResult struct {
	BaseSeq uint64    // commit of the base backup used
	Seq     uint64    // last commit replayed
	Time    time.Time // its commit time (zero for the base alone)
}

// Restore rebuilds the archived database in dir at path (which must not
// exist) as of the point given by opts. The restored database is a new,
// writable primary: it gets a new identity and the next term, like a
// promoted replica.
func Restore(dir, path string, opts RestoreOptions) (RestoreResult, error) {
	var res RestoreResult
	if _, err := os.Stat(path); err == nil {
		return res, fmt.Errorf("replication: restore target %s already exists", path)
	}
	var gcm cipher.AEAD
	if opts.ArchiveKey != nil {
		var err error
		if gcm, err = newArchiveGCM(opts.ArchiveKey); err != nil {
			return res, err
		}
	}
	files, err := listArchive(dir)
	if err != nil {
		return res, err
	}
	// The newest base at or before the target.
	var base *baseFile
	for i := range files.bases {
		b := &files.bases[i]
		if opts.Seq > 0 && b.seq > opts.Seq || !opts.Time.IsZero() && b.time.After(opts.Time) {
			continue
		}
		if base == nil || b.time.After(base.time) {
			base = b
		}
	}
	if base == nil {
		return res, errors.New("replication: no base backup at or before the requested point")
	}
	if err := copyFile(base.path, path); err != nil {
		return res, err
	}
	db, err := bangdb.OpenWithOptions(path, &bangdb.Options{EncryptionKey: opts.Key})
	if err != nil {
		os.Remove(path)
		return res, err
	}
	fail := func(err error) (RestoreResult, error) {
		db.Close()
		os.Remove(path)
		os.Remove(path + "-wal")
		return res, err
	}
	e := db.Engine()
	e.SetReadOnly(true)
	res.BaseSeq, res.Seq = base.seq, base.seq
	done := false
	for _, s := range files.segments {
		if done || s.dbid != base.dbid {
			continue
		}
		err := readSegment(s.path, gcm, func(f engine.Frame) (bool, error) {
			if f.Seq <= res.Seq {
				return true, nil
			}
			if opts.Seq > 0 && f.Seq > opts.Seq || !opts.Time.IsZero() && f.Time.After(opts.Time) {
				done = true
				return false, nil
			}
			if f.Seq != res.Seq+1 {
				return false, fmt.Errorf("replication: archive is missing commits %d to %d", res.Seq+1, f.Seq-1)
			}
			if err := e.ApplyFrame(f); err != nil {
				return false, err
			}
			res.Seq, res.Time = f.Seq, f.Time
			return true, nil
		})
		if err != nil && !errors.Is(err, errTornRecord) {
			return fail(err)
		}
	}
	if opts.Seq > 0 && res.Seq < opts.Seq {
		return fail(fmt.Errorf("replication: the archive ends at commit %d, before %d", res.Seq, opts.Seq))
	}
	if err := e.Promote(); err != nil {
		return fail(err)
	}
	return res, db.Close()
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
