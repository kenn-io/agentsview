package sync

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

// finalizeWithRebuildPRHistory carries a saved usage-only repository change
// into a rebuild before resolving new hostless selectors. Saved links are
// merged afterward by CopySessionMetadataFrom.
func (e *Engine) finalizeWithRebuildPRHistory(ctx context.Context, candidate ingest.Candidate, options ingest.ContentOptions) (ingest.PreparedSession, error) {
	if source, ok := e.archiveStore.(interface {
		GetArtifactExportSession(context.Context, string) (*db.Session, error)
	}); ok && options.ArchiveContent.UsageOnly() {
		previous, err := source.GetArtifactExportSession(ctx, candidate.Session.ID)
		if err != nil {
			return ingest.PreparedSession{}, err
		}
		if previous != nil {
			candidate.Session.PRRepositoryChanged = candidate.Session.PRRepositoryChanged || previous.PRRepositoryChanged
		}
	}
	return ingest.FinalizeRows(ctx, candidate, options)
}
