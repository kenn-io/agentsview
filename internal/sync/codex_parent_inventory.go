package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The collector may already have flushed a fork before another worker notices
// a new parent page. Revoke freshness before the pass reports completion.
func (e *Engine) finishCodexParentInventoryBatch(ctx context.Context, stats *SyncStats) error {
	sources, err := parser.ValidateCodexParentInventoryBatch(ctx)
	// Like collector flushes, demotion must finish even when parsing was canceled.
	completionCtx := context.WithoutCancel(ctx)
	for _, source := range sources {
		if e.archiveStore != nil {
			log.Printf("resync: deferred %s: Codex parent inventory changed", source.Path)
		}
		stats.recordDeferred(source.Path)
		e.invalidateVerifiedSource(source.Agent, source.Path)
		target := e.effectiveSourcePath(source.Path)
		ids, readErr := e.db.ListSessionIDsByFilePath(completionCtx, target, string(source.Agent))
		err = errors.Join(err, readErr)
		if readErr == nil {
			if e.archiveStore != nil {
				var preserve []string
				for _, id := range ids {
					stored, lookupErr := e.archiveStore.GetSessionFull(completionCtx, id)
					err = errors.Join(err, lookupErr)
					if lookupErr == nil && stored != nil {
						preserve = append(preserve, id)
					}
				}
				if len(preserve) > 0 {
					_, deleteErr := e.db.DeleteParserExcludedSessions(completionCtx, preserve)
					err = errors.Join(err, deleteErr)
					if deleteErr == nil {
						stats.deferredPreservedIDs = append(stats.deferredPreservedIDs, preserve...)
						ids = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(preserve, id) })
					}
				}
			}
			err = errors.Join(err, e.db.SetSessionDataVersionsContext(completionCtx, ids, max(db.CurrentDataVersion()-1, 0)))
		}
		_, clearErr := e.clearSkipPersistent(completionCtx, providerAgentSkipCacheKey(source.Path, source.Agent))
		err = errors.Join(err, clearErr, e.db.DeleteProviderStatHash(completionCtx, source.Agent, target))
	}
	if err != nil {
		stats.RecordFailed()
		if e.archiveStore != nil {
			stats.Aborted = true
		}
		return fmt.Errorf("validate Codex parent inventory: %w", err)
	}
	return nil
}
