package httpapi

import (
	"fmt"
	"io"
	"net/http"

	"github.com/knoxxr/bangdb"
)

// metrics serves the database statistics in the Prometheus text format.
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) error {
	s := h.db.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m := func(name, kind, help string, v float64) {
		fmt.Fprintf(w, "# HELP bangdb_%s %s\n# TYPE bangdb_%s %s\nbangdb_%s %g\n", name, help, name, kind, name, v)
	}
	fmt.Fprintf(w, "# HELP bangdb_info Build information.\n# TYPE bangdb_info gauge\nbangdb_info{version=%q} 1\n", bangdb.Version)
	m("statements_total", "counter", "Statements executed.", float64(s.Statements))
	m("queries_total", "counter", "Read-only statements executed.", float64(s.Queries))
	m("errors_total", "counter", "Statements that failed.", float64(s.Errors))
	m("slow_queries_total", "counter", "Calls slower than the slow query threshold.", float64(s.SlowQueries))
	m("tx_commits_total", "counter", "Write transactions committed.", float64(s.TxCommits))
	m("tx_rollbacks_total", "counter", "Write transactions rolled back.", float64(s.TxRollbacks))
	m("commit_seconds_total", "counter", "Time spent committing.", s.CommitTime.Seconds())
	m("lock_waits_total", "counter", "Writers that waited for the write lock.", float64(s.LockWaits))
	m("lock_wait_seconds_total", "counter", "Time writers waited for the write lock.", s.LockWaitTime.Seconds())
	m("busy_errors_total", "counter", "Write lock waits that timed out.", float64(s.BusyErrors))
	m("wal_syncs_total", "counter", "WAL fsyncs.", float64(s.WALSyncs))
	m("wal_pages", "gauge", "Page versions not yet checkpointed.", float64(s.WALPages))
	m("wal_bytes", "gauge", "Bytes used in the WAL file.", float64(s.WALBytes))
	m("checkpoints_total", "counter", "Checkpoints.", float64(s.Checkpoints))
	m("checkpoint_seconds_total", "counter", "Time spent in checkpoints.", s.CheckpointTime.Seconds())
	m("page_reads_total", "counter", "Pages read from disk.", float64(s.PageReads))
	m("page_cache_hits_total", "counter", "Page reads served from memory.", float64(s.PageCacheHits))
	m("node_cache_hits_total", "counter", "Decoded node cache hits.", float64(s.NodeHits))
	m("node_cache_misses_total", "counter", "Decoded node cache misses.", float64(s.NodeMisses))
	m("snapshots", "gauge", "Open read snapshots.", float64(s.Snapshots))
	m("snapshot_lag_commits", "gauge", "Commits since the oldest open snapshot.", float64(s.SnapshotLag))
	m("open_cursors", "gauge", "Streaming result sets not yet closed.", float64(s.OpenCursors))
	m("commit_seq", "gauge", "Sequence number of the last commit.", float64(s.Seq))
	m("pages", "gauge", "Pages in the database file.", float64(s.PageCount))
	if h.opts.ExtraMetrics != nil {
		h.opts.ExtraMetrics(w)
	}
	return nil
}

// WriteMetric writes one metric in the Prometheus text format (for
// Options.ExtraMetrics).
func WriteMetric(w io.Writer, name, kind, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", name, help, name, kind, name, v)
}
