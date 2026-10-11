package sync

import (
	"context"
	"errors"
	"fmt"

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
		stats.recordDeferred(source.Path)
		e.invalidateVerifiedSource(source.Agent, source.Path)
		target := e.effectiveSourcePath(source.Path)
		ids, readErr := e.db.ListSessionIDsByFilePath(completionCtx, target, string(source.Agent))
		err = errors.Join(err, readErr)
		if readErr == nil {
			err = errors.Join(err, e.db.SetSessionDataVersionsContext(completionCtx, ids, max(db.CurrentDataVersion()-1, 0)))
		}
		_, clearErr := e.clearSkipPersistent(completionCtx, providerAgentSkipCacheKey(source.Path, source.Agent))
		err = errors.Join(err, clearErr, e.db.DeleteProviderStatHash(completionCtx, source.Agent, target))
	}
	if err != nil {
		stats.RecordFailed()
		return fmt.Errorf("validate Codex parent inventory: %w", err)
	}
	return nil
}
