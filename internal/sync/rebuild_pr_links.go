package sync

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/ingest"
)

// finalizeWithRebuildPRHistory uses saved checkout context before resolving
// hostless selectors. CopySessionMetadataFrom merges saved links afterward.
func (e *Engine) finalizeWithRebuildPRHistory(ctx context.Context, candidate ingest.Candidate, options ingest.ContentOptions) (ingest.PreparedSession, error) {
	if err := e.restoreRebuildPRContext(ctx, &candidate, &options); err != nil {
		return ingest.PreparedSession{}, err
	}
	return ingest.FinalizeRows(ctx, candidate, options)
}

func (e *Engine) restoreRebuildPRContext(ctx context.Context, candidate *ingest.Candidate, options *ingest.ContentOptions) error {
	if source, ok := e.archiveStore.(interface {
		GetArtifactExportSession(context.Context, string) (*db.Session, error)
	}); ok && options.ArchiveContent.UsageOnly() {
		previous, err := source.GetArtifactExportSession(ctx, candidate.Session.ID)
		if err != nil {
			return err
		}
		if previous != nil {
			candidate.Session.PRRepositoryChanged = candidate.Session.PRRepositoryChanged || previous.PRRepositoryChanged
		}
		if snapshots, ok := e.archiveStore.(interface {
			ListSessionProjectIdentitySnapshotsByID(context.Context, []string) (map[string]export.ProjectIdentityObservation, error)
		}); ok {
			identities, err := snapshots.ListSessionProjectIdentitySnapshotsByID(ctx, []string{candidate.Session.ID})
			if err != nil {
				return err
			}
			if identity, found := identities[candidate.Session.ID]; found {
				options.CheckoutRemote = identity.GitRemote
			}
		}
	}
	return nil
}
