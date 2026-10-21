package sync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

func (e *Engine) sessionArchiveProvenance(session *db.Session, native string) (*db.ArchiveSessionProvenance, error) {
	if e.archiveWriteProvenance == nil {
		return nil, nil
	}
	p, err := e.archiveWriteProvenance(session, native)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (e *Engine) writeAttachedStaged(ctx context.Context, prepared ingest.PreparedSession, pw pendingWrite, provenance *db.ArchiveSessionProvenance) error {
	observation, hasObservation := e.projectIdentityObservationForWrite(pw, prepared.Session)
	snapshotProject := pw.sess.Project
	write := db.SessionBatchWrite{
		Session: prepared.Session, Messages: prepared.Messages, UsageEvents: prepared.UsageEvents, PlanUsage: prepared.PlanUsage,
		ToolResultImages: &e.toolResultImages, ArchiveProvenance: provenance,
		IdentityObservation: identityObservationOrZero(observation, hasObservation), IdentitySnapshotProject: &snapshotProject,
		ReplaceMessages: true, DataVersion: dataVersionForWrite(pw), SkipSignalUpdates: e.disableSignalRecompute,
	}
	return e.writeStagedFullParse(ctx, prepared.Session, prepared.Messages, pw, &write)
}

// SyncAttachedSource checks completion and prepares one source under the sync
// lock, so a concurrent rebuild cannot make the selected pending work stale.
func (e *Engine) SyncAttachedSource(ctx context.Context, source db.ArchiveSourceProvenance, target string,
	prepare func() (EngineConfig, string, func() error, error), progress ProgressFunc,
) (stats SyncStats, retErr error) {
	retErr = e.RunExclusive(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.archiveStore == nil {
			pending, err := e.db.ArchiveSourcePending(ctx, source, target)
			if err != nil || !pending {
				return err
			}
		}
		cfg, path, cleanup, err := prepare()
		if err != nil {
			return err
		}
		if cfg.ArchiveWriteProvenance == nil || cleanup == nil {
			return errors.New("archive source ownership and cleanup are required")
		}
		baseline := e.db
		if old, ok := e.archiveStore.(*db.DB); ok {
			baseline = old
		}
		active, err := baseline.ArchiveSourceSessionIDs(ctx, source.Source)
		if err != nil {
			return err
		}
		seen := make(map[string]bool)
		policy := cfg.ArchiveSessionPolicy
		cfg.ArchiveSessionPolicy = func(ctx context.Context, session *db.Session, native string) (bool, error) {
			seen[session.ID] = true
			if policy != nil {
				keep, err := policy(ctx, session, native)
				if err != nil || !keep {
					return keep, err
				}
			}
			p, err := cfg.ArchiveWriteProvenance(session, native)
			if err != nil {
				return false, err
			}
			if baseline != e.db {
				if err := baseline.CheckExistingArchiveProvenance(ctx, session.ID, p); err != nil {
					return false, err
				}
			}
			return true, e.db.CheckArchiveSessionProvenance(ctx, *session, p)
		}
		cfg.ParserExclusionPolicy = func(ctx context.Context, ids []string) error {
			for _, id := range ids {
				existing, err := baseline.GetSessionFull(ctx, id)
				if err != nil {
					return err
				}
				if existing != nil {
					return errors.New("archive parser exclusion conflicts with an existing session")
				}
			}
			return nil
		}
		child := NewEngine(ctx, e.db, cfg)
		defer child.Close()
		child.archiveStore, child.archiveRebuildIndex = e.archiveStore, e.archiveRebuildIndex
		child.deferredSourceCwd, child.deferredAntigravityTitles = e.deferredSourceCwd, e.deferredAntigravityTitles
		if progress != nil {
			progress(Progress{Phase: PhaseSyncing, Detail: "Reading archived session"})
		}
		if err := child.ReparsePathsContext(ctx, []string{path}); err != nil {
			return err
		}
		stats = child.LastSyncStats()
		stats.nonContainerDiscovered = stats.filesDiscovered
		if stats.TotalSessions == 0 || !stats.ProcessingComplete() {
			return errors.New("archive source parse incomplete")
		}
		for _, id := range active {
			if !seen[id] {
				return fmt.Errorf("archive parser did not retain active session %s", id)
			}
		}
		if err := cleanup(); err != nil {
			return err
		}
		return e.db.CompleteArchiveSource(ctx, source, target)
	})
	if retErr == nil && stats.Synced > 0 {
		e.emit("sessions")
	}
	return stats, retErr
}
