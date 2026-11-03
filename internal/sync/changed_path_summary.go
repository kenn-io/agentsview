package sync

import (
	"fmt"
	"time"
)

// changedPathPassSummary describes the work one changed-path pass did, so a
// debug log shows whether watcher events name session sources at all and how
// much source content the pass re-read because stats could not prove a source
// unchanged.
type changedPathPassSummary struct {
	paths             int
	sources           int
	started           time.Time
	contentChecks     int64
	contentCheckBytes int64
}

func (e *Engine) beginChangedPathPass(paths []string) changedPathPassSummary {
	return changedPathPassSummary{
		paths:             len(paths),
		started:           time.Now(),
		contentChecks:     e.contentChecks.Load(),
		contentCheckBytes: e.contentCheckBytes.Load(),
	}
}

// finish turns the counter snapshots taken at the start of the pass into
// per-pass deltas and renders the log line. The caller holds syncMu, so no
// other changed-path pass moves the counters in between.
func (s changedPathPassSummary) finish(e *Engine, stats SyncStats) string {
	checks := e.contentChecks.Load() - s.contentChecks
	checkBytes := e.contentCheckBytes.Load() - s.contentCheckBytes
	return fmt.Sprintf(
		"sync: changed paths: %d path(s) matched %d source(s): "+
			"%d updated, %d unchanged, %d with errors; "+
			"%d source(s) re-read to check their content (%s); took %s",
		s.paths, s.sources, stats.Synced, stats.Skipped, stats.Failed,
		checks, formatByteCount(checkBytes),
		time.Since(s.started).Round(time.Millisecond),
	)
}

func formatByteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
