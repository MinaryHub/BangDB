package replication

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
)

// ReplicaOptions configures a replica.
type ReplicaOptions struct {
	// Token sent to the primary.
	Token string
	// Dial opens the connection to the primary. Default: TCP with a 5s
	// timeout. Use it to plug in TLS.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// ReadTimeout after which a silent primary is considered dead.
	// Default: 10s (the primary sends heartbeats every second).
	ReadTimeout time.Duration
}

// ReplicaStatus reports the state of a replica.
type ReplicaStatus struct {
	Connected  bool
	Seq        uint64 // last applied commit
	PrimarySeq uint64 // last commit known on the primary
	Snapshots  int    // snapshots installed since start
	Lag        uint64 // PrimarySeq - Seq: commits known on the primary but not applied
	Term       uint64 // the primary's term
	LastError  string
}

// Replica mirrors a primary into a local database, which is put in read-only
// mode. Queries can be run on the local database at any time.
type Replica struct {
	db     *engine.DB
	addr   string
	opts   ReplicaOptions
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	status  ReplicaStatus
	changed chan struct{}
	conn    net.Conn
}

// StartReplica starts mirroring primaryAddr into db in the background. It
// reconnects automatically. db becomes read-only until Promote is called.
func StartReplica(db *bangdb.DB, primaryAddr string, opts ReplicaOptions) *Replica {
	if opts.Dial == nil {
		opts.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 10 * time.Second
	}
	e := db.Engine()
	e.SetReadOnly(true)
	ctx, cancel := context.WithCancel(context.Background())
	r := &Replica{db: e, addr: primaryAddr, opts: opts, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), changed: make(chan struct{})}
	r.status.Seq = e.Seq()
	go r.run()
	return r
}

// Status returns a snapshot of the replica state.
func (r *Replica) Status() ReplicaStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	if s.PrimarySeq > s.Seq {
		s.Lag = s.PrimarySeq - s.Seq
	}
	return s
}

// WaitFor blocks until the replica has applied commit seq (e.g. the value of
// DB.Seq() on the primary after a write, for read-your-writes).
func (r *Replica) WaitFor(ctx context.Context, seq uint64) error {
	for {
		r.mu.Lock()
		cur, ch := r.status.Seq, r.changed
		r.mu.Unlock()
		if cur >= seq {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-r.done:
			return errors.New("replication: replica stopped")
		}
	}
}

// Stop disconnects from the primary. The database stays read-only.
func (r *Replica) Stop() {
	r.cancel()
	r.mu.Lock()
	if r.conn != nil {
		r.conn.Close()
	}
	r.mu.Unlock()
	<-r.done
}

// Promote stops replication and makes the local database a writable
// primary (failover). It gets a new identity, so other replicas pointed at
// it will re-synchronize with a snapshot.
func (r *Replica) Promote() error {
	r.Stop()
	return r.db.Promote()
}

func (r *Replica) update(fn func(s *ReplicaStatus)) {
	r.mu.Lock()
	fn(&r.status)
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *Replica) run() {
	defer close(r.done)
	backoff := 200 * time.Millisecond
	for r.ctx.Err() == nil {
		start := time.Now()
		err := r.session()
		r.update(func(s *ReplicaStatus) {
			s.Connected = false
			if err != nil && r.ctx.Err() == nil {
				s.LastError = err.Error()
			}
		})
		if time.Since(start) > 10*time.Second {
			backoff = 200 * time.Millisecond
		}
		select {
		case <-r.ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (r *Replica) session() error {
	conn, err := r.opts.Dial(r.ctx, r.addr)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
	defer conn.Close()
	if r.ctx.Err() != nil {
		return nil
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	localTerm := r.db.Term()
	if err := writeHello(conn, hello{dbid: r.db.DBID(), seq: r.db.Seq(), term: localTerm, token: r.opts.Token}); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(&deadlineReader{conn, r.opts.ReadTimeout}, 256<<10)
	status, err := rd.ReadByte()
	if err != nil {
		return err
	}
	switch status {
	case statusOK:
	case statusUnauthorized:
		return errors.New("replication: primary rejected the token")
	case statusStale:
		return errors.New("replication: the primary was replaced by a newer one (it has an older term) and is now fenced")
	default:
		return fmt.Errorf("replication: primary refused connection (status %d)", status)
	}
	term, err := readU64(rd)
	if err != nil {
		return err
	}
	if term < localTerm {
		return fmt.Errorf("replication: the primary's term %d is older than this database's term %d; refusing to follow a stale primary", term, localTerm)
	}
	r.update(func(s *ReplicaStatus) { s.Connected = true; s.LastError = ""; s.Term = term })
	// ack reports the applied sequence once the received data is used up
	// (acknowledgements of a burst of frames are coalesced).
	ack := func(seq uint64) error {
		if rd.Buffered() > 0 {
			return nil
		}
		var b [9]byte
		b[0] = msgAck
		binary.BigEndian.PutUint64(b[1:], seq)
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := conn.Write(b[:])
		return err
	}

	var snap *engine.SnapshotWriter // non-nil while a snapshot is being received
	var snapBase, snapEnd uint64
	snapDone := false
	defer func() {
		if snap != nil {
			snap.Abort()
		}
	}()
	install := func() error {
		if err := r.db.InstallSnapshot(snap); err != nil {
			return err
		}
		snap = nil
		seq := r.db.Seq()
		r.update(func(s *ReplicaStatus) { s.Seq = seq; s.Snapshots++ })
		return ack(seq)
	}
	for {
		typ, err := rd.ReadByte()
		if err != nil {
			if r.ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch typ {
		case msgSnapshot:
			var dbid [16]byte
			if _, err := io.ReadFull(rd, dbid[:]); err != nil {
				return err
			}
			if snapBase, err = readU64(rd); err != nil {
				return err
			}
			if _, err := readU32(rd); err != nil {
				return err
			}
			if snap != nil {
				snap.Abort()
			}
			if snap, err = r.db.NewSnapshotWriter(r.db.Path()); err != nil {
				return err
			}
			snapDone = false
		case msgPage:
			no, data, err := readPage(rd)
			if err != nil {
				return err
			}
			if snap == nil || snapDone {
				return errors.New("replication: unexpected snapshot page")
			}
			if err := snap.WritePage(no, data); err != nil {
				return err
			}
		case msgSnapEnd:
			if snapEnd, err = readU64(rd); err != nil {
				return err
			}
			if snap == nil {
				return errors.New("replication: unexpected snapshot end")
			}
			if err := snap.Finish(snapBase); err != nil {
				return err
			}
			snapDone = true
			if snap.Seq() >= snapEnd {
				if err := install(); err != nil {
					return err
				}
			}
		case msgFrame:
			f, err := readFrame(rd)
			if err != nil {
				return err
			}
			if snap != nil {
				// Commits made while the snapshot was copied complete it.
				if !snapDone {
					return errors.New("replication: frame inside snapshot pages")
				}
				if err := snap.Apply(f); err != nil {
					return err
				}
				if snap.Seq() >= snapEnd {
					if err := install(); err != nil {
						return err
					}
				}
				continue
			}
			if err := r.db.ApplyFrame(f); err != nil {
				return err
			}
			r.update(func(s *ReplicaStatus) {
				s.Seq = f.Seq
				s.PrimarySeq = max(s.PrimarySeq, f.Seq)
			})
			if err := ack(f.Seq); err != nil {
				return err
			}
		case msgHeartbeat:
			seq, err := readU64(rd)
			if err != nil {
				return err
			}
			r.update(func(s *ReplicaStatus) { s.PrimarySeq = seq })
			if snap == nil {
				if err := ack(r.db.Seq()); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("replication: unknown message type %q", typ)
		}
	}
}

type deadlineReader struct {
	c       net.Conn
	timeout time.Duration
}

func (d *deadlineReader) Read(b []byte) (int, error) {
	d.c.SetReadDeadline(time.Now().Add(d.timeout))
	return d.c.Read(b)
}
