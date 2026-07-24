package artifact

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

type queuedArtifactExportStore interface {
	PendingArtifactExports(context.Context, int) ([]db.ArtifactExportQueueItem, error)
	GetSessionFull(context.Context, string) (*db.Session, error)
	GetAllMessages(context.Context, string) ([]db.Message, error)
	GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
}

type queuedArtifactExport struct {
	Item        db.ArtifactExportQueueItem
	Session     *db.Session
	Messages    []db.Message
	UsageEvents []db.UsageEvent
}

// forEachQueuedArtifactExport loads only the bounded dirty batch and at most
// one complete session body at a time. A missing session represents a pending
// publication deletion and deliberately performs no message or usage reads.
func forEachQueuedArtifactExport(
	ctx context.Context,
	store queuedArtifactExportStore,
	limit int,
	visit func(queuedArtifactExport) error,
) error {
	if visit == nil {
		return errors.New("queued artifact export visitor is required")
	}
	items, err := store.PendingArtifactExports(ctx, limit)
	if err != nil {
		return fmt.Errorf("reading queued artifact exports: %w", err)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		work := queuedArtifactExport{Item: item}
		work.Session, err = store.GetSessionFull(ctx, item.SessionID)
		if err != nil {
			return fmt.Errorf("loading queued artifact session %s: %w", item.SessionID, err)
		}
		if work.Session != nil &&
			(work.Session.Machine != "local" || work.Session.DeletedAt != nil) {
			work.Session = nil
		}
		if work.Session != nil {
			work.Messages, err = store.GetAllMessages(ctx, item.SessionID)
			if err != nil {
				return fmt.Errorf("loading queued artifact messages %s: %w", item.SessionID, err)
			}
			work.UsageEvents, err = store.GetUsageEvents(ctx, item.SessionID)
			if err != nil {
				return fmt.Errorf("loading queued artifact usage %s: %w", item.SessionID, err)
			}
		}
		if err := visit(work); err != nil {
			return err
		}
	}
	return nil
}
