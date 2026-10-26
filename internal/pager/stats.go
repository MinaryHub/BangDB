package pager

import "time"

// Stats are counters and gauges of a pager. Counters are totals since the
// pager was opened.
type Stats struct {
	Seq       uint64 // sequence of the latest durable commit
	PageCount uint32 // pages in the database

	Commits  uint64 // commits made durable
	WALSyncs uint64 // WAL fsyncs (group commit: one may cover many commits)

	WALPages     int   // page versions not yet checkpointed
	WALBytes     int64 // bytes used in the WAL file
	VersionBytes int   // page versions held in memory

	Checkpoints     uint64        // checkpoints that moved pages
	CheckpointPages uint64        // pages they moved
	CheckpointTime  time.Duration // time spent in them

	PageReads     uint64 // pages read from the database file or the WAL
	PageCacheHits uint64 // reads served from memory (page cache, versions)
	NodeHits      uint64 // decoded-node cache hits
	NodeMisses    uint64 // decoded-node cache misses (page decoded)

	Snapshots      int    // open read snapshots
	OldestSnapshot uint64 // sequence of the oldest open snapshot (0: none)
	SnapshotLag    uint64 // commits since the oldest open snapshot
	PendingCommits int    // commits prepared but not yet durable
}

type pagerCounters struct {
	commits, walSyncs            uint64
	checkpoints, checkpointPages uint64
	checkpointTime               time.Duration
	pageReads, pageCacheHits     uint64
}

// Stats returns the pager's statistics.
func (p *Pager) Stats() Stats {
	p.mu.Lock()
	c := p.counters
	s := Stats{
		Seq: p.hdr.Seq, PageCount: p.hdr.PageCount,
		Commits: c.commits, WALSyncs: c.walSyncs,
		WALPages: p.walPages, WALBytes: p.walSize, VersionBytes: p.verBytes,
		Checkpoints: c.checkpoints, CheckpointPages: c.checkpointPages, CheckpointTime: c.checkpointTime,
		PageReads: c.pageReads, PageCacheHits: c.pageCacheHits,
		PendingCommits: len(p.pending),
	}
	for seq, n := range p.readers {
		s.Snapshots += n
		if s.OldestSnapshot == 0 || seq < s.OldestSnapshot {
			s.OldestSnapshot = seq
		}
	}
	if s.Snapshots > 0 {
		s.SnapshotLag = p.hdr.Seq - s.OldestSnapshot
	}
	p.mu.Unlock()
	s.NodeHits, s.NodeMisses = p.objs.counts()
	return s
}
