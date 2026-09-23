package sync

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/signals"
)

// Compatibility wrappers keep incremental maintenance and existing package
// tests on the shared normalized computation.
func computeSignalsFromMessages(
	session db.Session, messages []db.Message,
) db.SessionSignalUpdate {
	return ingest.ComputeSignalsFromMessages(session, messages)
}

func patchToolCallRowsWithContentFailures(
	rows []signals.ToolCallRow, messages []db.Message, failures map[string]bool,
) {
	ingest.PatchToolCallRowsWithContentFailures(rows, messages, failures)
}

func computeSignalsFromToolRows(
	session db.Session, messages []db.Message, rows []signals.ToolCallRow,
) db.SessionSignalUpdate {
	return ingest.ComputeSignalsFromToolRows(session, messages, rows)
}

func extractHeuristicMessages(messages []db.Message) []signals.HeuristicMessage {
	return ingest.ExtractHeuristicMessages(messages)
}

func extractToolCallRows(messages []db.Message) []signals.ToolCallRow {
	return ingest.ExtractToolCallRows(messages)
}

func extractContextTokens(messages []db.Message) []signals.ContextTokenRow {
	return ingest.ExtractContextTokens(messages)
}

func extractCompactBoundaryOrdinals(messages []db.Message) []int {
	return ingest.ExtractCompactBoundaryOrdinals(messages)
}

func extractMostCommonModel(messages []db.Message) string {
	return ingest.ExtractMostCommonModel(messages)
}

func extractLastMessageRole(messages []db.Message) (role, content string) {
	return ingest.ExtractLastMessageRole(messages)
}
