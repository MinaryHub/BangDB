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
	// SyncReplicas makes replication semi-synchronous: every commit waits,
	// after it is durable locally, until this many replicas have applied
	// it, or SyncTimeout passes. On timeout the commit still succeeds (it
	// is durable on the primary) and Stats().SyncTimeouts counts it; the
	// next commits wait again. 0: asynchronous.
	SyncReplicas int
	// SyncTimeout bounds a semi-synchronous wait. Default: 1s.
	SyncTimeout time.Duration
	// OnFenced is called when a replica with a newer term connects: this
	// primary was replaced by a promoted replica, and it has been made
	// read-only.
	OnFenced func(newTerm uint64)
}

// ReplicaInfo describes a connected replica as seen by the primary.
type ReplicaInfo struct {
	Addr      string
	Seq       uint64        // last sequence sent to it
	AckSeq    uint64        // last sequence it reported as applied
	AckTime   time.Time     // when it last reported
	Lag       uint64        // commits it has not applied yet
	LagTime   time.Duration // age of the oldest commit it has not applied (0: none)
	Since     time.Time
	Snapshots int // snapshots sent on this connection

	pending []sentFrame // frames sent but not acknowledged
}

type sentFrame struct {
	seq uint64
	at  time.Time
}

// PrimaryStats counts semi-synchronous waits.
type PrimaryStats struct {
	SyncWaits    uint64        // commits that waited for replicas
	SyncTimeouts uint64        // waits that ended by timeout
	SyncWaitTime time.Duration // total time waited
	Fenced       bool          // a newer primary exists; this one is read-only
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
	acked chan struct{} // closed and replaced when a replica acknowledges
	stats PrimaryStats
}

// NewPrimary prepares db to serve replicas.
func NewPrimary(db *bangdb.DB, opts PrimaryOptions) *Primary {
	if opts.LogSize <= 0 {
		opts.LogSize = engine.DefaultReplicationLog
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = time.Second
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = time.Second
	}
	e := db.Engine()
	e.SetReplicationLog(opts.LogSize)
	ctx, cancel := context.WithCancel(context.Background())
	p := &Primary{db: e, opts: opts, ctx: ctx, cancel: cancel, conns: map[net.Conn]*ReplicaInfo{},
		acked: make(chan struct{})}
	if opts.SyncReplicas > 0 {
		e.SetCommitWaiter(p.waitAcked)
	}
	return p
}

// waitAcked blocks until SyncReplicas replicas have applied seq, or the
// sync timeout passes.
func (p *Primary) waitAcked(seq uint64) {
	start := time.Now()
	timer := time.NewTimer(p.opts.SyncTimeout)
	defer timer.Stop()
	for {
		p.mu.Lock()
		n := 0
		for _, r := range p.conns {
			if r.AckSeq >= seq {
				n++
			}
		}
		ch := p.acked
		if n >= p.opts.SyncReplicas || p.ctx.Err() != nil {
			p.stats.SyncWaits++
			p.stats.SyncWaitTime += time.Since(start)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			p.mu.Lock()
			p.stats.SyncWaits++
			p.stats.SyncTimeouts++
			p.stats.SyncWaitTime += time.Since(start)
			p.mu.Unlock()
			return
		}
	}
}

// Stats returns the primary's counters.
func (p *Primary) Stats() PrimaryStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// fence makes the database read-only: a replica follows a newer primary.
func (p *Primary) fence(term uint64) {
	p.db.SetReadOnly(true)
	p.mu.Lock()
	first := !p.stats.Fenced
	p.stats.Fenced = true
	p.mu.Unlock()
	if first && p.opts.OnFenced != nil {
		p.opts.OnFenced(term)
	}
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

// Replicas lists connected replicas with their replication lag.
func (p *Primary) Replicas() []ReplicaInfo {
	seq := p.db.Seq()
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ReplicaInfo, 0, len(p.conns))
	for _, r := range p.conns {
		info := *r
		info.pending = nil
		if seq > r.AckSeq {
			info.Lag = seq - r.AckSeq
		}
		if len(r.pending) > 0 {
			info.LagTime = now.Sub(r.pending[0].at)
		} else if info.Lag > 0 && !r.AckTime.IsZero() {
			info.LagTime = now.Sub(r.AckTime) // commits not even sent yet
		}
		out = append(out, info)
	}
	return out
}

// Close stops serving and disconnects all replicas.
func (p *Primary) Close() error {
	if p.opts.SyncReplicas > 0 {
		p.db.SetCommitWaiter(nil)
	}
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
	if term := p.db.Term(); h.term > term {
		// The replica follows a primary promoted after us: we are stale.
		c.Write([]byte{statusStale})
		p.fence(h.term)
		return
	}
	w := bufio.NewWriterSize(&deadlineWriter{c}, 256<<10)
	w.WriteByte(statusOK)
	writeU64(w, p.db.Term())

	// Read acknowledgements; stop streaming when the replica disconnects.
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	go func() {
		defer cancel()
		rd := bufio.NewReader(c)
		for {
			typ, err := rd.ReadByte()
			if err != nil || typ != msgAck {
				return
			}
			seq, err := readU64(rd)
			if err != nil {
				return
			}
			p.mu.Lock()
			info.AckSeq, info.AckTime = max(info.AckSeq, seq), time.Now()
			k := 0
			for k < len(info.pending) && info.pending[k].seq <= seq {
				k++
			}
			info.pending = info.pending[k:]
			close(p.acked)
			p.acked = make(chan struct{})
			p.mu.Unlock()
		}
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
			p.setInfo(info, func() {
				info.Seq = f.Seq
				at := f.Time
				if at.IsZero() {
					at = time.Now()
				}
				if len(info.pending) < 1<<16 {
					info.pending = append(info.pending, sentFrame{f.Seq, at})
				}
			})
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
