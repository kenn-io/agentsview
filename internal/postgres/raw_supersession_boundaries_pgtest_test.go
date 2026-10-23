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

type supersessionPublication struct {
	Hidden           bool
	Identity, Corpus int64
}

func supersessionPublicationAt(t *testing.T, q supersessionQuery) supersessionPublication {
	t.Helper()
	var state supersessionPublication
	require.NoError(t, q.QueryRowContext(t.Context(), `SELECT s.rollout_hidden,c.identity_revision,c.corpus_revision FROM sessions s CROSS JOIN raw_corpus_state c WHERE s.id='codex:portable' AND c.singleton=1`).Scan(&state.Hidden, &state.Identity, &state.Corpus))
	return state
}

// A real complete replacement withdraws membership; a tombstone deliberately
// does not. Keep the old served set to prove that missing candidates, rather
// than a scope change, remove the derived map.
func TestRawSupersessionAuthoritativeMembershipWithdrawal(t *testing.T) {
	f, group := supersessionFixture(t)
	legacySnapshot := func() string {
		var value string
		require.NoError(t, f.runtime.QueryRow(`SELECT jsonb_build_object('session',to_jsonb(s)-'rollout_hidden',
   'messages',(SELECT jsonb_agg(to_jsonb(m) ORDER BY ordinal) FROM messages m WHERE m.session_id=s.id),
   'pins',(SELECT jsonb_agg(to_jsonb(p) ORDER BY ordinal) FROM pinned_messages p WHERE p.session_id=s.id),
   'starred',EXISTS(SELECT 1 FROM starred_sessions x WHERE x.session_id=s.id))::text
   FROM sessions s WHERE id='codex:portable'`).Scan(&value))
		return value
	}
	original := legacySnapshot()
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	var receipt string
	require.NoError(t, f.runtime.QueryRow(`SELECT receipt FROM raw_source_heads`).Scan(&receipt))
	m, _ := f.accept(t, "device-a", "withdraw-last-member", receipt)
	empty := projectionOutcome("unused")
	empty.Outcome.Results = nil
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, empty))
	var active int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_session_branches WHERE active`).Scan(&active))
	assert.Zero(t, active)
	plan := applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	assert.Equal(t, []rawSupersessionChange{{Legacy: "codex:portable", Before: group}}, plan.Changes)
	assert.Equal(t, []rawLegacyVisibility{{Legacy: "codex:portable", Hidden: false}}, plan.Visibility)
	assert.True(t, plan.IdentityChanged)
	var maps int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&maps))
	assert.Zero(t, maps)
	assert.False(t, supersessionPublicationAt(t, f.runtime).Hidden)
	assert.Equal(t, original, legacySnapshot(), "retained content, labels, name, trash, star and pins survive")
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	restored, err := h.GetSessionFull(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, new("legacy title"), restored.DisplayName)
}

// Observe every phase within the caller's transaction. Final-state-only tests
// cannot catch an early visibility UPDATE that publishes before prelocking.
func TestRawSupersessionStagesVisibilityAndPublication(t *testing.T) {
	f, group := supersessionFixture(t)
	for _, phase := range []string{"new", "unchanged", "removal"} {
		t.Run(phase, func(t *testing.T) {
			served := map[string]bool{group: true}
			if phase == "removal" {
				served = nil
			}
			before := supersessionPublicationAt(t, f.runtime)
			tx, err := f.runtime.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			plan, err := prepareRawSupersessions(t.Context(), tx, served)
			require.NoError(t, err)
			assert.Equal(t, phase != "unchanged", plan.IdentityChanged)
			if phase == "unchanged" {
				assert.Empty(t, plan.Changes)
				assert.Empty(t, plan.Visibility)
			} else if phase == "new" {
				assert.Equal(t, []rawSupersessionChange{{Legacy: "codex:portable", After: group}}, plan.Changes)
				assert.Equal(t, []rawLegacyVisibility{{Legacy: "codex:portable", Hidden: true}}, plan.Visibility)
			} else {
				assert.Equal(t, []rawSupersessionChange{{Legacy: "codex:portable", Before: group}}, plan.Changes)
				assert.Equal(t, []rawLegacyVisibility{{Legacy: "codex:portable", Hidden: false}}, plan.Visibility)
			}
			assert.Equal(t, before, supersessionPublicationAt(t, tx), "after map preparation")
			carry, err := prepareRawLegacyCarry(t.Context(), tx, "codex:portable", group)
			require.NoError(t, err)
			assert.Equal(t, before, supersessionPublicationAt(t, tx), "after carry preparation")
			require.NoError(t, f.sink.applyRawLegacyCarry(t.Context(), tx, carry))
			assert.Equal(t, before, supersessionPublicationAt(t, tx), "after curation materialization")
			require.NoError(t, writeRawSupersessionCache(t.Context(), tx, plan))
			assert.Equal(t, before, supersessionPublicationAt(t, tx), "after cache writes")
			require.NoError(t, applyRawLegacyVisibility(t.Context(), tx, plan))
			after := supersessionPublicationAt(t, tx)
			if phase == "unchanged" {
				assert.Equal(t, before, after)
			} else {
				assert.Equal(t, phase == "new", after.Hidden)
				assert.Greater(t, after.Identity, before.Identity)
				assert.Greater(t, after.Corpus, before.Corpus)
			}
			require.NoError(t, tx.Rollback())
			assert.Equal(t, before, supersessionPublicationAt(t, f.runtime), "rollback restores publication and visibility")
		})
		if phase == "new" {
			applySupersessionFixture(t, f, map[string]bool{group: true}, true)
		}
	}
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
