package sync

import (
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// ErrUnifiedRebuildAborted reports that the atomic local and HTTP rebuild was
// discarded without a narrower preparation or contributor error. Callers must
// treat the operation as unsuccessful even though the active archive remains
// intact.
var ErrUnifiedRebuildAborted = errors.New(
	"unified local and HTTP rebuild aborted",
)

// RebuildContributor adds another configured sync source to an atomic full
// rebuild. Contributors run sequentially against the same temporary database.
type RebuildContributor struct {
	Name      string
	Config    EngineConfig
	Progress  func(Progress) Progress
	AfterSync func(*Engine, *db.DB) error
}

// RebuildOptions configures optional sources for an atomic full rebuild.
type RebuildOptions struct {
	Contributors []RebuildContributor
	// includePhaseDiagnostics is enabled only by the options entrypoint. The
	// legacy ResyncAll wrapper keeps both returned and in-flight stats free of
	// options-only diagnostics.
	includePhaseDiagnostics bool
}

// RebuildPhaseStats records observable bulk-write diagnostics for one source
// participating in a rebuild.
type RebuildPhaseStats struct {
	Contributor    string `json:"contributor"`
	PrepNanos      int64  `json:"prep_nanos"`
	ScanNanos      int64  `json:"scan_nanos"`
	WriteNanos     int64  `json:"write_nanos"`
	Batches        int64  `json:"batches"`
	BatchedWrites  int64  `json:"batched_writes"`
	WriteBatchSize int64  `json:"write_batch_size"`
}

// RebuildContributorError identifies a contributor whose lifecycle hook
// prevented the atomic rebuild from completing.
type RebuildContributorError struct {
	Contributor string
	Err         error
}

func (e *RebuildContributorError) Error() string {
	return fmt.Sprintf("rebuild contributor %q: %v", e.Contributor, e.Err)
}

func (e *RebuildContributorError) Unwrap() error { return e.Err }

type rebuildOperations struct {
	rebuildFTS func(*db.DB) error
	reopen     func(*db.DB) error
}

var productionRebuildOperations = rebuildOperations{
	rebuildFTS: func(database *db.DB) error { return database.RebuildFTS() },
	reopen:     func(database *db.DB) error { return database.Reopen() },
}

func (ops rebuildOperations) withDefaults() rebuildOperations {
	if ops.rebuildFTS == nil {
		ops.rebuildFTS = productionRebuildOperations.rebuildFTS
	}
	if ops.reopen == nil {
		ops.reopen = productionRebuildOperations.reopen
	}
	return ops
}

func phaseSnapshot(name string, stats *PhaseStats) RebuildPhaseStats {
	return RebuildPhaseStats{
		Contributor:    name,
		PrepNanos:      stats.PrepNanos.Load(),
		ScanNanos:      stats.ScanNanos.Load(),
		WriteNanos:     stats.WriteNanos.Load(),
		Batches:        stats.Batches.Load(),
		BatchedWrites:  stats.BatchedWrites.Load(),
		WriteBatchSize: stats.WriteBatchSize.Load(),
	}
}

func mergeSyncStats(dst *SyncStats, src SyncStats) {
	dst.TotalSessions += src.TotalSessions
	dst.Synced += src.Synced
	dst.Skipped += src.Skipped
	dst.Failed += src.Failed
	dst.OrphanedCopied += src.OrphanedCopied
	dst.Warnings = append(dst.Warnings, src.Warnings...)
	dst.Aborted = dst.Aborted || src.Aborted
	dst.RebuildPhases = append(dst.RebuildPhases, src.RebuildPhases...)
	dst.Anomalies.merge(src.Anomalies)
	dst.filesOK += src.filesOK
	dst.filesDiscovered += src.filesDiscovered
	dst.nonContainerDiscovered += src.nonContainerDiscovered
	dst.messagesIndexed += src.messagesIndexed
	dst.parserExcludedFiles += src.parserExcludedFiles
	dst.parserExcludedIDs = append(dst.parserExcludedIDs, src.parserExcludedIDs...)
	dst.cwdFilteredSessions += src.cwdFilteredSessions
	dst.cwdFilteredFiles += src.cwdFilteredFiles
}
