package sync

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) reconcileHermesHistoryContext(ctx context.Context, candidate ingest.Candidate, options ingest.ContentOptions) (ingest.HistoryResult, error) {
	options.ToolResultImageAssetsDir = e.db.AssetsDir()
	store := e.historyStore()
	stored, err := store.GetSessionFull(ctx, candidate.Session.ID)
	if err != nil {
		return ingest.HistoryResult{}, err
	}
	if stored == nil {
		return ingest.ReconcileProviderHistory(ctx, candidate, nil, options)
	}
	messages, err := store.GetAllMessages(ctx, candidate.Session.ID)
	if err != nil {
		return ingest.HistoryResult{}, err
	}
	prior := &ingest.PriorSession{Session: *stored, Messages: messages}
	if candidate.Parsed.Replacement.Usage == parser.UsagePreserveArchived {
		type usageReader interface {
			GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
		}
		reader, ok := store.(usageReader)
		if !ok {
			return ingest.HistoryResult{}, fmt.Errorf("archive %T cannot read preserved usage for session %s", store, candidate.Session.ID)
		}
		prior.UsageEvents, err = reader.GetUsageEvents(ctx, candidate.Session.ID)
		if err != nil {
			return ingest.HistoryResult{}, err
		}
	}
	return ingest.ReconcileProviderHistory(ctx, candidate, prior, options)
}
