//go:build pgtest

package postgres

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawProjectionPrefixCopiesRetainSourceHistory(t *testing.T) {
	t.Parallel()
	for _, longerFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(longerFirst), func(t *testing.T) {
			f := newProjectionFixture(t)
			short := projectionOutcome("same prompt")
			short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
			long := projectionOutcome("same prompt")
			var b rawsync.CanonicalManifest
			var accepted rawsync.CommitResult
			if longerFirst {
				b, accepted = f.accept(t, "device-b", "long", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
				// Rebuild the selected retained longer copy, not the incoming
				// shorter payload that this next projection inserts.
				retained, err := f.sink.Resolve(t.Context(), "codex:portable")
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), rawSourceProofDetachSQL, retained.SessionID)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, retained.SessionID)
				require.NoError(t, err)
			}
			a, shortAccepted := f.accept(t, "device-a", "short", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
			if !longerFirst {
				b, accepted = f.accept(t, "device-b", "long", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
			}
			h, err := NewHostedStore(t.Context(), f.dsn, f.schema, f.tenant, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "codex:portable", page.Sessions[0].ID)
			messages, err := h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			_, err = h.StarSession(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.sink.SetPin(t.Context(), f.alias(t, a), 0, true, "retained prefix"))
			a, _ = f.accept(t, "device-a", "same-short", shortAccepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
			var sources, captures int
			resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*),count(DISTINCT session_id) FROM session_sources WHERE physical_session_id=$1`, resolved.SessionID).Scan(&sources, &captures))
			assert.Equal(t, 2, sources)
			assert.Equal(t, 2, captures)
			// Removing the longer source must reveal the original shorter capture,
			// not leave the other device claiming content it never supplied.
			b, _ = f.accept(t, "device-b", "removed", accepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
			messages, err = h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "same prompt", messages[0].Content)
			stars, err := h.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, stars, "codex:portable")
			pins, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, new("retained prefix"), pins[0].Note)
		})
	}
}

func TestRawProjectionPrefixDoesNotChooseDivergentContinuation(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	for i := range 3 {
		outcome := projectionOutcome("shared prompt")
		if i == 0 {
			outcome.Outcome.Results[0].Result.Messages = outcome.Outcome.Results[0].Result.Messages[:1]
		} else if i == 2 {
			outcome.Outcome.Results[0].Result.Messages[1].Content = "different continuation"
		}
		m, _ := f.accept(t, fmt.Sprintf("device-%d", i), fmt.Sprintf("capture-%d", i), "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	}
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityAmbiguous, resolved.State)
	require.Len(t, resolved.Variants, 3)
	for _, alias := range resolved.Variants {
		variant, err := f.sink.Resolve(t.Context(), alias)
		require.NoError(t, err)
		assert.Equal(t, RawIdentityUnique, variant.State)
		var exists bool
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)`, variant.SessionID).Scan(&exists))
		assert.True(t, exists)
	}
}

func seedLegacyRawReviewCapture(t *testing.T, f projectionFixture, sessionID string) (rawderive.ParsedManifest, string) {
	t.Helper()
	ctx := t.Context()
	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	outcome := projectionOutcome(prompt)
	outcome.Outcome.Results[0].Result.Session.ID = sessionID
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = sessionID
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	outcome.Outcome.Results[0].Result.Session.Entrypoint = "sdk-cli"
	outcome.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent

	m, _ := f.accept(t, "device-a", "legacy-capture", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, m), m, outcome))

	var groupID, currentID string
	var currentPayload []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT r.group_id, r.session_id, r.payload
		 FROM raw_session_branches b
		 JOIN raw_content_revisions r ON r.session_id = b.captured_session_id
		 WHERE b.member_id = $1`, sessionID,
	).Scan(&groupID, &currentID, &currentPayload))

	prepared, err := decodeRawPayload(currentPayload)
	require.NoError(t, err)
	prepared.Session.IsAutomated = false
	prepared.Session.RelationshipType = string(parser.RelSubagent)

	// Encode without a trailing rawRelationshipProvenance record to match a
	// capture stored before this upgrade.
	var legacyBuf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&legacyBuf).Encode(prepared))
	legacyRev, err := rawContentRevision(prepared)
	require.NoError(t, err)
	legacyID := "raw-row-" + rawDigest("row-v1", groupID, legacyRev)
	require.NotEqual(t, currentID, legacyID)

	_, err = f.runtime.ExecContext(ctx,
		`INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) VALUES($1,$2,$3,$4)`,
		legacyID, groupID, legacyRev, legacyBuf.Bytes(),
	)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE raw_session_branches SET session_id=$1, captured_session_id=$1, content_revision=$2 WHERE member_id=$3`,
		legacyID, legacyRev, sessionID,
	)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE session_sources SET session_id=$1, physical_session_id=NULL, content_revision=$2 WHERE physical_session_id=$3`,
		legacyID, legacyRev, currentID,
	)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM sessions WHERE id=$1`, currentID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM raw_message_dialogue WHERE session_id=$1`, currentID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM raw_embedding_outbox WHERE session_id=$1`, currentID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM raw_content_revisions WHERE session_id=$1`, currentID)
	require.NoError(t, err)
	return outcome, legacyID
}

func TestRawProjectionUpgradeIdenticalHeadlessReviewResolvesUniquely(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	outcome, legacyID := seedLegacyRawReviewCapture(t, f, "review-unique")

	b, _ := f.accept(t, "device-b", "upgraded-capture", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, outcome))

	resolved, err := f.sink.Resolve(ctx, "review-unique")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State, "identical pre- and post-upgrade captures must resolve to one public session")
	assert.NotEqual(t, legacyID, resolved.SessionID, "selected revision should be the post-upgrade capture")

	h, err := newHostedAdapter(ctx, f.runtime, f.tenant)
	require.NoError(t, err)
	got, err := h.GetSessionFull(ctx, "review-unique")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.IsAutomated)
	assert.Empty(t, got.RelationshipType)

	var branchCount, distinctCaptured, distinctSelected int
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT count(*), count(DISTINCT captured_session_id), count(DISTINCT session_id)
		 FROM raw_session_branches WHERE member_id=$1 AND active`, "review-unique",
	).Scan(&branchCount, &distinctCaptured, &distinctSelected))
	assert.Equal(t, 2, branchCount)
	assert.Equal(t, 2, distinctCaptured, "each source retains its original captured revision")
	assert.Equal(t, 1, distinctSelected, "both branches converge on one selected revision")
}

func TestRawProjectionWithdrawalRestoresLegacyAutomatedReview(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	shortOutcome, legacyID := seedLegacyRawReviewCapture(t, f, "review-restore")

	longOutcome := shortOutcome
	longMessages := append([]parser.ParsedMessage(nil), shortOutcome.Outcome.Results[0].Result.Messages...)
	longMessages = append(longMessages,
		parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Check error paths too."},
		parser.ParsedMessage{Ordinal: 3, Role: parser.RoleAssistant, Content: "Checked."},
	)
	longOutcome.Outcome.Results = []parser.ParseResultOutcome{{Result: shortOutcome.Outcome.Results[0].Result}}
	longOutcome.Outcome.Results[0].Result.Messages = longMessages

	b, accepted := f.accept(t, "device-b", "longer-capture", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, longOutcome))

	h, err := newHostedAdapter(ctx, f.runtime, f.tenant)
	require.NoError(t, err)
	got, err := h.GetSessionFull(ctx, "review-restore")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.IsAutomated)
	assert.Equal(t, "subagent", got.RelationshipType)

	// Withdrawing the longer capture restores device-a's legacy one-turn review
	// directly from raw_content_revisions, which must re-evaluate automation.
	b, _ = f.accept(t, "device-b", "withdrawn", accepted.Receipt, parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, rawderive.ParsedManifest{
		Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true},
	}))

	resolved, err := f.sink.Resolve(ctx, "review-restore")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	assert.Equal(t, legacyID, resolved.SessionID)

	got, err = h.GetSessionFull(ctx, "review-restore")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.IsAutomated, "restored legacy review must be marked automated")
	assert.Empty(t, got.RelationshipType, "restored legacy review must not stay under Subagents")
}

func TestRawProjectionWithdrawalPreservesLegacyAgentSubagent(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	shortOutcome := projectionOutcome(prompt)
	shortOutcome.Outcome.Results[0].Result.Session.ID = "agent-legacy-child"
	shortOutcome.Outcome.Results[0].Result.Session.SourceSessionID = ""
	shortOutcome.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	shortOutcome.Outcome.Results[0].Result.Session.Entrypoint = "sdk-cli"
	shortOutcome.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent

	a, _ := f.accept(t, "device-a", "agent-legacy", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, a), a, shortOutcome))

	var groupID, currentID string
	var currentPayload []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT r.group_id, r.session_id, r.payload
		 FROM raw_session_branches b
		 JOIN raw_content_revisions r ON r.session_id = b.captured_session_id
		 WHERE b.member_id = $1`, "agent-legacy-child",
	).Scan(&groupID, &currentID, &currentPayload))

	prepared, err := decodeRawPayload(currentPayload)
	require.NoError(t, err)
	prepared.Session.ID = "agent-legacy-child"
	prepared.Session.SourceSessionID = ""
	prepared.Session.IsAutomated = false
	prepared.Session.RelationshipType = string(parser.RelSubagent)

	// Encode without a provenance trailer, matching a pre-upgrade capture whose
	// only child marker is its agent-* session ID.
	var legacyBuf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&legacyBuf).Encode(prepared))
	legacyRev, err := rawContentRevision(prepared)
	require.NoError(t, err)
	legacyID := "raw-row-" + rawDigest("row-v1", groupID, legacyRev)

	_, err = f.runtime.ExecContext(ctx,
		`INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) VALUES($1,$2,$3,$4)`,
		legacyID, groupID, legacyRev, legacyBuf.Bytes(),
	)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE raw_session_branches SET session_id=$1, captured_session_id=$1, content_revision=$2 WHERE member_id=$3`,
		legacyID, legacyRev, "agent-legacy-child",
	)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE session_sources SET session_id=$1, physical_session_id=NULL, content_revision=$2 WHERE physical_session_id=$3`,
		legacyID, legacyRev, currentID,
	)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM sessions WHERE id=$1`, currentID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM raw_message_dialogue WHERE session_id=$1`, currentID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM raw_embedding_outbox WHERE session_id=$1`, currentID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx, `DELETE FROM raw_content_revisions WHERE session_id=$1`, currentID)
	require.NoError(t, err)

	// Restore the legacy agent-* capture through writePayload and verify it
	// keeps Subagents even though its prompt matches an automation rule.
	tx, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	restored, err := loadRawPayload(ctx, tx, legacyID)
	require.NoError(t, err)
	require.NoError(t, f.sink.writePayload(ctx, tx, legacyID, groupID, legacyRev, restored))
	require.NoError(t, tx.Commit())

	var rel string
	var inferred bool
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT relationship_type, relationship_inferred FROM sessions WHERE id=$1`, legacyID,
	).Scan(&rel, &inferred))
	assert.Equal(t, "subagent", rel, "restored legacy agent-* subagent must stay in Subagents")
	assert.False(t, inferred)
}

func TestRawProjectionUpdatesGrokExplicitSubagentProvenance(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	outcome := projectionOutcome("Human prompt for Grok run.")
	outcome.Outcome.Results[0].Result.Session.ID = "grok:run-1"
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = "run-1"
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentGrok
	outcome.Outcome.Results[0].Result.Session.SessionKind = parser.SessionKindNonInteractive
	outcome.Outcome.Results[0].Result.Session.RelationshipType = parser.RelNone

	m1, r1 := f.accept(t, "device-a", "grok-inferred", "", parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, m1), m1, outcome))

	resolved, err := f.sink.Resolve(ctx, "grok:run-1")
	require.NoError(t, err)
	var inferred bool
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT relationship_inferred FROM sessions WHERE id=$1`, resolved.SessionID,
	).Scan(&inferred))
	assert.True(t, inferred, "initial non-interactive Grok capture starts inferred")

	// Re-project the same transcript after Grok's summary adds an explicit
	// subagent marker; content_revision matches, so provenance must still update.
	explicit := outcome
	explicit.Outcome.Results = []parser.ParseResultOutcome{{Result: outcome.Outcome.Results[0].Result}}
	explicit.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent
	m2, _ := f.accept(t, "device-a", "grok-explicit", r1.Receipt, parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, m2), m2, explicit))

	var rel string
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT relationship_type, relationship_inferred FROM sessions WHERE id=$1`, resolved.SessionID,
	).Scan(&rel, &inferred))
	assert.Equal(t, "subagent", rel)
	assert.False(t, inferred, "explicit subagent summary marker must clear relationship_inferred")

	var storedBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT payload FROM raw_content_revisions WHERE session_id=$1`, resolved.SessionID,
	).Scan(&storedBytes))
	stored, err := decodeRawPayload(storedBytes)
	require.NoError(t, err)
	assert.False(t, db.SessionRelationshipInferred(stored.Session), "stored raw payload must retain explicit provenance")
}
