//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type supersessionQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Capture every column and row, including immutable bytes and database IDs.
// Only the three session curation columns may differ. These SQL snapshots do
// not use the carry helper, payload decoder, matcher or content hash as oracle.
func rawCarryContentSnapshot(t *testing.T, q supersessionQuery) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, table := range []string{"sessions", "messages", "tool_calls", "tool_result_events", "usage_events", "session_plan_usage", "cursor_usage_events", "raw_content_revisions", "raw_message_dialogue", "raw_session_branches", "session_sources", "raw_source_projections", "raw_projection_generations", "raw_source_contributions", "raw_session_links", "raw_session_public_aliases", "raw_embedding_outbox"} {
		value := "to_jsonb(r)"
		predicate := ""
		if table == "sessions" {
			value += "-ARRAY['display_name','deleted_at','deletion_cause']"
			predicate = " WHERE provenance_kind='raw'"
		}
		var rows string
		require.NoError(t, q.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(v ORDER BY v::text),'[]'::jsonb)::text FROM (SELECT `+value+` AS v FROM `+table+` r`+predicate+`) content_rows`).Scan(&rows), table)
		snapshot[table] = rows
	}
	return snapshot
}

func TestRawSupersessionCarryPreservesRawContentAndProofs(t *testing.T) {
	f, group := supersessionFixture(t)
	m, _ := f.accept(t, "device-b", "content-proof", "")
	out := projectionOutcome("retained")
	out.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
	call := &out.Outcome.Results[0].Result.Messages[1].ToolCalls[0]
	call.SubagentSessionID = "codex:portable"
	call.ResultEvents = []parser.ParsedToolResultEvent{{ToolUseID: "call-1", SubagentSessionID: "codex:portable", Source: "tool_result", Status: "completed", Content: "retained result"}}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	_, err := f.runtime.Exec(`UPDATE sessions SET deleted_at=NULL WHERE id='codex:portable'`)
	require.NoError(t, err)
	var events, links, usages, input, output, proofs, outbox int
	require.NoError(t, f.runtime.QueryRow(`SELECT
 (SELECT count(*) FROM tool_result_events), (SELECT count(*) FROM raw_session_links),
 (SELECT count(*) FROM usage_events), (SELECT sum(input_tokens) FROM usage_events), (SELECT sum(output_tokens) FROM usage_events),
 (SELECT count(*) FROM session_sources WHERE physical_session_id IS NOT NULL), (SELECT count(*) FROM raw_embedding_outbox)`).Scan(&events, &links, &usages, &input, &output, &proofs, &outbox))
	assert.Equal(t, 1, events)
	assert.Equal(t, 2, links)
	assert.Equal(t, 2, usages)
	assert.Equal(t, 22, input)
	assert.Equal(t, 14, output)
	assert.Equal(t, 2, proofs)
	assert.Positive(t, outbox)
	before := rawCarryContentSnapshot(t, f.runtime)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	carry, err := prepareRawLegacyCarry(t.Context(), tx, "codex:portable", group)
	require.NoError(t, err)
	require.NoError(t, f.sink.applyRawLegacyCarry(t.Context(), tx, carry))
	assert.Equal(t, before, rawCarryContentSnapshot(t, tx))
	var named, trashed, starred, pinned int
	require.NoError(t, tx.QueryRow(`SELECT
 (SELECT count(*) FROM sessions WHERE provenance_kind='raw' AND display_name='legacy title'),
 (SELECT count(*) FROM sessions WHERE provenance_kind='raw' AND deleted_at IS NOT NULL),
 (SELECT count(*) FROM starred_sessions WHERE session_id<>'codex:portable'),
 (SELECT count(*) FROM pinned_messages WHERE session_id<>'codex:portable' AND ordinal=0 AND note='legacy note')`).Scan(&named, &trashed, &starred, &pinned))
	assert.Equal(t, 2, named)
	assert.Zero(t, trashed)
	assert.Equal(t, 2, starred)
	assert.Equal(t, 2, pinned)
	require.NoError(t, tx.Commit())
	assert.Equal(t, before, rawCarryContentSnapshot(t, f.runtime))
}
