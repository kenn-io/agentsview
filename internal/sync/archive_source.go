package sync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
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

// Check kept and excluded results before staging freshness or other session
// writes: one conflicting DAG member must leave every existing member unchanged.
func (e *Engine) validateSourceSessionOwnership(ctx context.Context, res processResult) error {
	baseline := e.db
	if old, ok := e.archiveStore.(*db.DB); ok {
		baseline = old
	}
	for _, result := range res.results {
		session, err := ingest.ConvertSessionContext(ctx, result.Session, nil)
		if err != nil {
			return err
		}
		if err := e.applyRemoteRewritesContext(ctx, &session, nil); err != nil {
			return err
		}
		provenance, err := e.sessionArchiveProvenance(&session, result.Session.ID)
		if err != nil {
			return err
		}
		if baseline != e.db && provenance != nil {
			if err := baseline.CheckExistingArchiveProvenance(ctx, session.ID, *provenance); err != nil {
				return err
			}
		}
		if err := e.db.CheckArchiveSessionProvenance(ctx, session, provenance); err != nil {
			return err
		}
	}
	if !e.sourceAllowsParserExclusions(res) {
		return nil
	}
	excluded := e.applyIDPrefixToSessionIDs(res.excludedSessionIDs)
	for _, id := range excluded {
		if id == "" {
			continue
		}
		// Exclusion deletes have no ownership grant, even for an attached
		// source. The delete transaction repeats this check.
		if err := e.db.CheckArchiveSessionProvenance(ctx, db.Session{ID: id}, nil); err != nil {
			return err
		}
	}
	if len(excluded) > 0 && e.parserExclusionPolicy != nil {
		return e.parserExclusionPolicy(ctx, excluded)
	}
	return nil
}

// filterReservedSourceMembers allows independent members of a shared source to
// progress while leaving a pushed snapshot untouched. Claude-format DAGs must
// still be admitted as a whole, and partial sources cannot prove freshness.
func (e *Engine) filterReservedSourceMembers(ctx context.Context, res *processResult, agent parser.AgentType) (bool, error) {
	if err := e.validateSourceSessionOwnership(ctx, *res); err == nil {
		return false, nil
	} else if !errors.Is(err, db.ErrSessionPushConflict) || res.incremental != nil {
		return false, err
	} else if sourceRequiresAtomicDAGCompletion(agent, len(res.results)) {
		return false, errors.New("atomic source contains a reserved session")
	}
	kept := make([]parser.ParseResult, 0, len(res.results))
	for _, result := range res.results {
		one := *res
		one.results = []parser.ParseResult{result}
		one.excludedSessionIDs = nil
		if err := e.validateSourceSessionOwnership(ctx, one); err != nil {
			if errors.Is(err, db.ErrSessionPushConflict) {
				continue
			}
			return false, err
		}
		kept = append(kept, result)
	}
	excluded := make([]string, 0, len(res.excludedSessionIDs))
	for _, id := range res.excludedSessionIDs {
		checkID := applyIDPrefixToID(e.idPrefix, id)
		if checkID == "" {
			excluded = append(excluded, id)
			continue
		}
		if err := e.db.CheckArchiveSessionProvenance(ctx, db.Session{ID: checkID}, nil); err != nil {
			if errors.Is(err, db.ErrSessionPushConflict) {
				continue
			}
			return false, err
		}
		excluded = append(excluded, id)
	}
	if len(kept) == len(res.results) && len(excluded) == len(res.excludedSessionIDs) {
		return false, db.ErrSessionPushConflict
	}
	if len(kept) == 0 && len(excluded) == 0 && len(res.sourceMissingMembers) == 0 {
		return true, nil
	}
	partial := *res
	partial.results = kept
	// Do not delete or tombstone members based on a partial source view.
	partial.excludedSessionIDs = nil
	partial.sourceMissingMembers = nil
	if err := e.validateSourceSessionOwnership(ctx, partial); err != nil {
		return false, err
	}
	partial.needsRetry = true
	partial.noCacheSkip = true
	partial.suppressPresenceSweep = true
	*res = partial
	return false, nil
}

func (e *Engine) writeAttachedStaged(ctx context.Context, prepared preparedSessionWrite, pw pendingWrite, provenance *db.ArchiveSessionProvenance) error {
	observation, hasObservation := e.projectIdentityObservationForWrite(pw, prepared.Session)
	snapshotProject := pw.sess.Project
	write := db.SessionBatchWrite{
		CodexForkState: prepared.codexForkState, StagedToolKeys: prepared.stagedToolKeys,
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
		// Restores take no sync lock; fence completion against one that lands
		// after this parse has skipped the trashed member.
		observed, err := e.db.ArchiveSourceCompletion(ctx, source)
		if err != nil {
			return err
		}
		cfg, path, cleanup, err := prepare()
		if err != nil {
			return err
		}
		if cfg.ArchiveWriteProvenance == nil || cleanup == nil {
			return errors.New("archive source ownership and cleanup are required")
		}
		// Only scoped bulk writes publish ownership with content.
		if !cfg.DiscardPendingWritesOnCancel {
			return errors.New("archive source writes require scoped bulk publication")
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
			return true, nil
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
		child.deferredSourceCwd, child.deferredNativeTitles = e.deferredSourceCwd, e.deferredNativeTitles
		if progress != nil {
			progress(Progress{Phase: PhaseSyncing, Detail: "Reading archived session"})
		}
		parseErr := child.ReparsePathsContext(ctx, []string{path})
		stats = child.LastSyncStats()
		stats.nonContainerDiscovered = stats.filesDiscovered
		if parseErr != nil {
			return parseErr
		}
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
		return e.db.CompleteArchiveSourceFrom(ctx, source, target, observed)
	})
	if stats.ChangedArchive() {
		e.emit("sessions")
	}
	return stats, retErr
}
