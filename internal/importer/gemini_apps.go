package importer

import (
	"context"
	"errors"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ImportGeminiApps reads a Google Takeout Gemini Apps HTML export and upserts
// each Prompted activity through the shared import persistence path.
func ImportGeminiApps(
	ctx context.Context,
	store db.Store,
	root string,
	cb *ImportCallbacks,
	machine ...string,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(ctx, store, cb.indexing)
	defer func() {
		if err := fts.restore(ctx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	provider, ok := parser.NewProvider(
		parser.AgentGeminiApps, parser.ProviderConfig{},
	)
	if !ok {
		return stats, errors.New("gemini apps provider unavailable")
	}
	exporter, ok := provider.(parser.GeminiAppsExportParser)
	if !ok {
		return stats, errors.New("gemini apps provider does not support exports")
	}

	parseSummary, err := exporter.ParseGeminiAppsExport(
		root,
		func(result parser.ParseResult) error {
			if err := ctx.Err(); err != nil {
				return err
			}

			result.Session.Machine = resolvedImportMachine(
				result.Session.Machine, machine,
			)
			status, err := upsertConversation(
				ctx, store, result, fts,
			)
			stats.record(result.Session.ID, status, err)
			cb.progress(stats)
			return nil
		},
	)
	stats.Skipped += parseSummary.Skipped
	stats.Errors += parseSummary.Errors
	retErr = err
	return
}
