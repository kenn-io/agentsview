package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A hub created before the ledger passes every older push probe; the
// ledger tables must still send the push down the full EnsureSchema path,
// or pushes would fail on the missing tables (compare the vector tables
// note in sync.go:395-399).
func TestSyncEnsureSchemaRunsDDLWhenLedgerTablesMissing(t *testing.T) {
	pg, state := newSchemaProbeDB(t, map[string][]string{
		"sessions": {"has_total_output_tokens", "has_peak_context_tokens"},
		"messages": {"has_context_tokens", "has_output_tokens"},
	})
	state.existingTables = map[string]bool{
		"model_pricing":                                   true,
		"model_pricing_bands":                             true,
		"genai_pricing":                                   true,
		"source_archives":                                 true,
		"source_project_identity_observations":            true,
		"source_project_identity_observation_scopes":      true,
		"source_session_project_identity_snapshots":       true,
		"source_session_project_identity_snapshot_scopes": true,
		"source_worktree_project_mappings":                true,
		"source_worktree_project_mapping_scopes":          true,
		"cursor_usage_events":                             true,
	}
	state.existingIndexes = map[string]bool{
		"idx_cursor_usage_events_dedup":   true,
		"idx_tool_result_events_terminal": true,
	}
	syncer := &Sync{pg: pg, schema: "agentsview"}

	require.NoError(t, syncer.EnsureSchema(t.Context()))

	executed := strings.ToLower(state.executedSQL())
	assert.Contains(t, executed, "create table if not exists ledger_segments",
		"a missing ledger table must fall back to the full schema DDL")
	assert.Contains(t, executed, "ledger_reject_mutation",
		"the append-only triggers are installed with the tables")
}
