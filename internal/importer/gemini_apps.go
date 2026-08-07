package importer

import (
	"context"
	"errors"
	"fmt"
	"log"

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
	fts := newLazyFTS(store, cb.indexing)
	defer func() {
		if err := fts.restore(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	provider, ok := parser.NewProvider(
		parser.AgentGeminiApps, parser.ProviderConfig{},
	)
	if !ok {
		return stats, fmt.Errorf("gemini apps provider unavailable")
	}
	exporter, ok := provider.(parser.GeminiAppsExportParser)
	if !ok {
		return stats, fmt.Errorf(
			"gemini apps provider does not support exports",
		)
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
			if err != nil {
				stats.Errors++
				log.Printf(
					"import: skipping %s: %v",
					result.Session.ID, err,
				)
				cb.progress(stats)
				return nil
			}

			switch status {
			case importNew:
				stats.Imported++
			case importUpdated:
				stats.Updated++
			case importSkipped:
				stats.Skipped++
			}
			cb.progress(stats)
			return nil
		},
	)
	stats.Skipped += parseSummary.Skipped
	stats.Errors += parseSummary.Errors
	retErr = err
	return
}
