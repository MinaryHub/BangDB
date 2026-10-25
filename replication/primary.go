package replication

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
)

// PrimaryOptions configures a primary.
type PrimaryOptions struct {
	// Token, if set, must be presented by replicas.
	Token string
	// LogSize is the number of bytes of recent commits kept in memory so that
	// briefly disconnected replicas can catch up without a snapshot.
	// Default: 64 MiB.
	LogSize int
	// Heartbeat is the idle interval between heartbeats. Default: 1s.
	Heartbeat time.Duration
}

// ReplicaInfo describes a connected replica as seen by the primary.
type ReplicaInfo struct {
	Addr      string
	Seq       uint64 // last sequence sent to it
	Since     time.Time
	Snapshots int // snapshots sent on this connection
}

// Primary serves the commit stream of a database to replicas.
type Primary struct {
	db     *engine.DB
	opts   PrimaryOptions
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	lns   []net.Listener
	conns map[net.Conn]*ReplicaInfo
	wg    sync.WaitGroup
}

// NewPrimary prepares db to serve replicas.
func NewPrimary(db *bangdb.DB, opts PrimaryOptions) *Primary {
	if opts.LogSize <= 0 {
		opts.LogSize = engine.DefaultReplicationLog
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = time.Second
	}
	e := db.Engine()
	e.SetReplicationLog(opts.LogSize)
	ctx, cancel := context.WithCancel(context.Background())
	return &Primary{db: e, opts: opts, ctx: ctx, cancel: cancel, conns: map[net.Conn]*ReplicaInfo{}}
}

// ListenAndServe listens on a TCP address and serves replicas.
func (p *Primary) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return p.Serve(ln)
}

// Serve accepts replica connections on ln until Close is called. Pass a
// tls.Listener for encrypted transport.
func (p *Primary) Serve(ln net.Listener) error {
	p.mu.Lock()
	p.lns = append(p.lns, ln)
	p.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			if p.ctx.Err() != nil {
				return nil
			}
			return err
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

// Replicas lists connected replicas.
func (p *Primary) Replicas() []ReplicaInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ReplicaInfo, 0, len(p.conns))
	for _, r := range p.conns {
		out = append(out, *r)
	}
	return out
}

// Close stops serving and disconnects all replicas.
func (p *Primary) Close() error {
	p.cancel()
	p.mu.Lock()
	for _, ln := range p.lns {
		ln.Close()
	}
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

func (p *Primary) handle(c net.Conn) {
	defer c.Close()
	info := &ReplicaInfo{Addr: c.RemoteAddr().String(), Since: time.Now()}
	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.conns[c] = info
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, c)
		p.mu.Unlock()
	}()

	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	h, err := readHello(c)
	if err != nil {
		c.Write([]byte{statusBadRequest})
		return
	}
	c.SetReadDeadline(time.Time{})
	if p.opts.Token != "" && subtle.ConstantTimeCompare([]byte(h.token), []byte(p.opts.Token)) != 1 {
		c.Write([]byte{statusUnauthorized})
		return
	}
	w := bufio.NewWriterSize(&deadlineWriter{c}, 256<<10)
	w.WriteByte(statusOK)

	// Stop streaming as soon as the replica disconnects.
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	go func() {
		var b [1]byte
		c.Read(b[:])
		cancel()
	}()

	var sub *engine.Subscription
	if h.dbid == p.db.DBID() {
		sub, err = p.db.Subscribe(h.seq)
		p.setInfo(info, func() { info.Seq = h.seq })
	}
	if sub == nil || err != nil {
		if sub, err = p.sendSnapshot(w, info); err != nil {
			return
		}
	}
	defer sub.Close()
	if err := w.Flush(); err != nil {
		return
	}
	for {
		tctx, tcancel := context.WithTimeout(ctx, p.opts.Heartbeat)
		f, err := sub.Next(tctx)
		tcancel()
		switch {
		case err == nil:
			if writeFrame(w, f) != nil {
				return
			}
			p.setInfo(info, func() { info.Seq = f.Seq })
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			w.WriteByte(msgHeartbeat)
			writeU64(w, sub.LastSeq())
		default:
			return // closed, lagging (replica reconnects and gets a snapshot), ...
		}
		if w.Flush() != nil {
			return
		}
	}
}

func (p *Primary) sendSnapshot(w *bufio.Writer, info *ReplicaInfo) (*engine.Subscription, error) {
	snap, err := p.db.OpenSnapshot(true)
	if err != nil {
		return nil, err
	}
	defer snap.Close()
	writeSnapshotBegin(w, snap.Info)
	err = snap.CopyPages(func(no uint32, data []byte) error { return writePage(w, no, data) })
	if err == nil {
		// The snapshot is consistent as of BaseSeq: no frames need to be
		// replayed before it can be installed.
		w.WriteByte(msgSnapEnd)
		err = writeU64(w, snap.Info.BaseSeq)
	}
	if err != nil {
		snap.Sub.Close()
		return nil, err
	}
	p.setInfo(info, func() { info.Snapshots++; info.Seq = snap.Info.BaseSeq })
	return snap.Sub, nil
}

func (p *Primary) setInfo(info *ReplicaInfo, fn func()) {
	p.mu.Lock()
	fn()
	p.mu.Unlock()
}

// deadlineWriter fails writes to replicas that stop reading.
type deadlineWriter struct{ c net.Conn }

func (d *deadlineWriter) Write(b []byte) (int, error) {
	d.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return d.c.Write(b)
}
