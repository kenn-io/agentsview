//go:build pgtest

package postgres

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
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

func seedLegacyRawReviewCapture(t *testing.T, f projectionFixture, sessionID, sourceSessionID string) (rawderive.ParsedManifest, string) {
	t.Helper()
	ctx := t.Context()
	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	outcome := projectionOutcome(prompt)
	outcome.Outcome.Results[0].Result.Session.ID = sessionID
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = sourceSessionID
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
	prepared.Session.ID = sessionID
	prepared.Session.SourceSessionID = sourceSessionID
	prepared.Session.IsAutomated = false
	prepared.Session.RelationshipType = string(parser.RelSubagent)
	prepared.Signals = ingest.RefreshSignalRecencyAt(
		prepared.Session, prepared.Messages, prepared.Signals,
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	)
	require.Equal(t, "completed", prepared.Signals.Outcome)
	require.Equal(t, "medium", prepared.Signals.OutcomeConfidence)

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

	outcome, legacyID := seedLegacyRawReviewCapture(t, f, "review-unique", "review-unique")

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

func withExtraTurn(o rawderive.ParsedManifest, user, assistant string) rawderive.ParsedManifest {
	next := o
	msgs := append([]parser.ParsedMessage(nil), o.Outcome.Results[0].Result.Messages...)
	msgs = append(msgs,
		parser.ParsedMessage{Ordinal: len(msgs), Role: parser.RoleUser, Content: user},
		parser.ParsedMessage{Ordinal: len(msgs) + 1, Role: parser.RoleAssistant, Content: assistant},
	)
	next.Outcome.Results = []parser.ParseResultOutcome{{Result: o.Outcome.Results[0].Result}}
	next.Outcome.Results[0].Result.Messages = msgs
	return next
}

func TestRawProjectionWithdrawalRestoresLegacyAutomatedReview(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	shortOutcome, legacyID := seedLegacyRawReviewCapture(t, f, "review-restore", "review-restore")
	longOutcome := withExtraTurn(shortOutcome, "Check error paths too.", "Checked.")

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
	assert.Equal(t, "unknown", got.Outcome, "restored automated review must refresh outcome from completed to unknown")
	assert.Equal(t, "low", got.OutcomeConfidence, "restored automated review must refresh confidence from medium to low")
}

func TestRawProjectionWithdrawalPreservesLegacyAgentSubagent(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	// SourceSessionID has no agent-* prefix, so only the original Session.ID
	// marks this legacy capture as an explicit Claude subagent.
	shortOutcome, legacyID := seedLegacyRawReviewCapture(t, f, "agent-legacy-child", "shared-child")
	longOutcome := withExtraTurn(shortOutcome, "Follow-up turn.", "Done.")

	b, accepted := f.accept(t, "device-b", "longer-agent", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, longOutcome))

	// Withdrawing device-b restores device-a's legacy agent-* capture through
	// materializeGroup and writePayload; it must stay in Subagents even though
	// its prompt matches an automation rule.
	b, _ = f.accept(t, "device-b", "withdrawn-agent", accepted.Receipt, parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, rawderive.ParsedManifest{
		Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true},
	}))

	resolved, err := f.sink.Resolve(ctx, "agent-legacy-child")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	assert.Equal(t, legacyID, resolved.SessionID)

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

func TestRawProjectionExplicitProvenanceSplitsMergedGrokCapture(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	shortOutcome := projectionOutcome("Human prompt for Grok run.")
	shortOutcome.Outcome.Results[0].Result.Session.ID = "grok:split-1"
	shortOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "split-1"
	shortOutcome.Outcome.Results[0].Result.Session.Agent = parser.AgentGrok
	shortOutcome.Outcome.Results[0].Result.Session.SessionKind = parser.SessionKindNonInteractive
	shortOutcome.Outcome.Results[0].Result.Session.RelationshipType = parser.RelNone

	a, ar := f.accept(t, "device-a", "grok-short-inferred", "", parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, a), a, shortOutcome))

	longOutcome := withExtraTurn(shortOutcome, "Second turn.", "Second reply.")

	b, _ := f.accept(t, "device-b", "grok-long-inferred", "", parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, longOutcome))

	resolved, err := f.sink.Resolve(ctx, "grok:split-1")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State, "inferred shorter copy merges into inferred longer copy")

	var beforeCorpus, beforeIdentity int64
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT corpus_revision, identity_revision FROM raw_corpus_state WHERE singleton=1`,
	).Scan(&beforeCorpus, &beforeIdentity))

	// Re-project device-a's shorter copy with an explicit subagent marker.
	// Its content revision is unchanged, so syncRawRelationshipProvenance must
	// trigger group reconciliation to split the explicit child from device-b's
	// inferred longer copy and publish the identity change.
	explicitShort := shortOutcome
	explicitShort.Outcome.Results = []parser.ParseResultOutcome{{Result: shortOutcome.Outcome.Results[0].Result}}
	explicitShort.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent
	a2, _ := f.accept(t, "device-a", "grok-short-explicit", ar.Receipt, parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, a2), a2, explicitShort))

	resolved, err = f.sink.Resolve(ctx, "grok:split-1")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityAmbiguous, resolved.State, "explicit shorter child must split from inferred longer copy")
	require.Len(t, resolved.Variants, 2)

	var afterCorpus, afterIdentity int64
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT corpus_revision, identity_revision FROM raw_corpus_state WHERE singleton=1`,
	).Scan(&afterCorpus, &afterIdentity))
	assert.Greater(t, afterCorpus, beforeCorpus)
	assert.Greater(t, afterIdentity, beforeIdentity)

	var explicitSessionID string
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT session_id FROM raw_session_branches WHERE member_id=$1 AND source_id=$2`,
		"grok:split-1", rawSourceID(a2),
	).Scan(&explicitSessionID))
	var rel string
	var inferred bool
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT relationship_type, relationship_inferred FROM sessions WHERE id=$1`, explicitSessionID,
	).Scan(&rel, &inferred))
	assert.Equal(t, "subagent", rel)
	assert.False(t, inferred)
}

func TestRawProjectionLegacyGrokPreservesExplicitStatusOnInferredUpload(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	outcome := projectionOutcome("Human prompt for legacy Grok child.")
	outcome.Outcome.Results[0].Result.Session.ID = "grok:legacy-1"
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = "legacy-1"
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentGrok
	outcome.Outcome.Results[0].Result.Session.SessionKind = parser.SessionKindNonInteractive
	outcome.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent

	a, _ := f.accept(t, "device-a", "grok-legacy", "", parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, a), a, outcome))

	resolved, err := f.sink.Resolve(ctx, "grok:legacy-1")
	require.NoError(t, err)
	var currentPayload []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT payload FROM raw_content_revisions WHERE session_id=$1`, resolved.SessionID,
	).Scan(&currentPayload))

	// Strip the provenance trailer to match a legacy pre-upgrade Grok capture
	// where recovery conservatively treats the child role as explicit.
	prepared, err := decodeRawPayload(currentPayload)
	require.NoError(t, err)
	var legacyBuf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&legacyBuf).Encode(prepared))
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE raw_content_revisions SET payload=$2 WHERE session_id=$1`, resolved.SessionID, legacyBuf.Bytes(),
	)
	require.NoError(t, err)

	// Uploading an identical non-automated copy without the explicit marker
	// must not overwrite the legacy Grok child's preserved explicit status.
	inferredCopy := outcome
	inferredCopy.Outcome.Results = []parser.ParseResultOutcome{{Result: outcome.Outcome.Results[0].Result}}
	inferredCopy.Outcome.Results[0].Result.Session.RelationshipType = parser.RelNone
	b, _ := f.accept(t, "device-b", "grok-inferred-copy", "", parser.AgentGrok)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, inferredCopy))

	var inferred bool
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT relationship_inferred FROM sessions WHERE id=$1`, resolved.SessionID,
	).Scan(&inferred))
	assert.False(t, inferred, "legacy Grok child must keep relationship_inferred=false")

	var storedBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT payload FROM raw_content_revisions WHERE session_id=$1`, resolved.SessionID,
	).Scan(&storedBytes))
	stored, err := decodeRawPayload(storedBytes)
	require.NoError(t, err)
	assert.False(t, db.SessionRelationshipInferred(stored.Session))
}

func TestHostedTenantUpgradeRefreshesSignalsForReclassifiedRawReview(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	outcome := projectionOutcome(prompt)
	outcome.Outcome.Results[0].Result.Session.ID = "review-upgrade-signals"
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = "review-upgrade-signals"
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	outcome.Outcome.Results[0].Result.Session.Entrypoint = "sdk-cli"
	outcome.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent
	outcome.Outcome.Results[0].Result.Messages[1].ToolCalls = nil

	m, _ := f.accept(t, "device-a", "upgrade-signals", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, m), m, outcome))

	resolved, err := f.sink.Resolve(ctx, "review-upgrade-signals")
	require.NoError(t, err)

	// Simulate pre-upgrade state where this two-message review was stored as
	// non-automated with completed/medium outcome and a scored health grade
	// in both sessions and the retained raw payload and recency overlay.
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE sessions
		 SET is_automated = FALSE,
		     relationship_type = 'subagent',
		     parser_relationship_type = NULL,
		     relationship_inferred = FALSE,
		     outcome = 'completed',
		     outcome_confidence = 'medium',
		     health_score = 100,
		     health_grade = 'A'
		 WHERE id = $1`, resolved.SessionID,
	)
	require.NoError(t, err)
	var rawPayloadBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT payload FROM raw_content_revisions WHERE session_id = $1`, resolved.SessionID,
	).Scan(&rawPayloadBytes))
	preUpgradePayload, err := decodeRawPayload(rawPayloadBytes)
	require.NoError(t, err)
	preUpgradePayload.Session.IsAutomated = false
	preUpgradeBytes, err := encodeRawPayload(preUpgradePayload)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE raw_content_revisions
		 SET payload = $2,
		     recency_state = '{"Outcome":"completed","OutcomeConfidence":"medium","HealthScore":100,"HealthGrade":"A"}'::jsonb
		 WHERE session_id = $1`, resolved.SessionID, preUpgradeBytes,
	)
	require.NoError(t, err)
	var beforeRecencyBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT recency_state FROM raw_content_revisions WHERE session_id = $1`, resolved.SessionID,
	).Scan(&beforeRecencyBytes))
	beforeRecency, err := decodeRawRecency(beforeRecencyBytes)
	require.NoError(t, err)
	require.Equal(t, rawRecencyState{
		Outcome:           "completed",
		OutcomeConfidence: "medium",
		HealthScore:       new(100),
		HealthGrade:       new("A"),
	}, beforeRecency)
	_, err = f.admin.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key = $1`, db.ClassifierHashKey)
	require.NoError(t, err)

	require.NoError(t, EnsureHostedTenant(ctx, f.admin, f.schema, f.tenant))

	var isAutomated bool
	var rel, gotOutcome, gotConfidence string
	var gotHealthScore *int
	var gotHealthGrade *string
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT is_automated, relationship_type, outcome, outcome_confidence, health_score, health_grade
		 FROM sessions WHERE id = $1`, resolved.SessionID,
	).Scan(&isAutomated, &rel, &gotOutcome, &gotConfidence, &gotHealthScore, &gotHealthGrade))
	assert.True(t, isAutomated)
	assert.Empty(t, rel)
	assert.Equal(t, "unknown", gotOutcome, "upgraded raw review must refresh outcome to unknown")
	assert.Equal(t, "low", gotConfidence, "upgraded raw review must refresh confidence to low")
	assert.Nil(t, gotHealthScore, "upgraded raw review without other signals must clear stale health_score")
	assert.Nil(t, gotHealthGrade, "upgraded raw review without other signals must clear stale health_grade")

	var recencyBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT recency_state FROM raw_content_revisions WHERE session_id = $1`, resolved.SessionID,
	).Scan(&recencyBytes))
	recency, err := decodeRawRecency(recencyBytes)
	require.NoError(t, err)
	assert.Equal(t, "unknown", recency.Outcome)
	assert.Equal(t, "low", recency.OutcomeConfidence)
	assert.Nil(t, recency.HealthScore)
	assert.Nil(t, recency.HealthGrade)
}

func TestRawProjectionRestoreRefreshesStaleRecencyAfterPatternRestored(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	short := projectionOutcome(prompt)
	short.Outcome.Results[0].Result.Session.ID = "review-restore-recency"
	short.Outcome.Results[0].Result.Session.SourceSessionID = "review-restore-recency"
	short.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	short.Outcome.Results[0].Result.Session.Entrypoint = "sdk-cli"
	short.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent
	short.Outcome.Results[0].Result.Messages[1].ToolCalls = nil

	a, _ := f.accept(t, "device-a", "short-review", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, a), a, short))
	shortResolved, err := f.sink.Resolve(ctx, "review-restore-recency")
	require.NoError(t, err)

	// Simulate an intermediate audit while a pattern was temporarily removed:
	// recency_state was overwritten to completed/medium/100/A while the
	// immutable gob payload kept IsAutomated=true.
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE raw_content_revisions
		 SET recency_state = '{"Outcome":"completed","OutcomeConfidence":"medium","HealthScore":100,"HealthGrade":"A"}'::jsonb
		 WHERE session_id = $1`, shortResolved.SessionID,
	)
	require.NoError(t, err)

	// Supersede the short capture with a longer capture from device-b so the
	// short row is dematerialized from sessions while its revision stays retained.
	longer := projectionOutcome(prompt)
	longer.Outcome.Results[0].Result.Session.ID = "review-restore-recency"
	longer.Outcome.Results[0].Result.Session.SourceSessionID = "review-restore-recency"
	longer.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	longer.Outcome.Results[0].Result.Session.Entrypoint = "sdk-cli"
	longer.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent
	longer.Outcome.Results[0].Result.Messages[1].ToolCalls = nil
	longer.Outcome.Results[0].Result.Messages = append(
		longer.Outcome.Results[0].Result.Messages,
		parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Follow-up question."},
		parser.ParsedMessage{Ordinal: 3, Role: parser.RoleAssistant, Content: "Follow-up answer."},
	)
	longer.Outcome.Results[0].Result.Session.MessageCount = 4
	longer.Outcome.Results[0].Result.Session.UserMessageCount = 2

	b, br := f.accept(t, "device-b", "longer-review", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, longer))

	// Withdraw device-b so writePayload rematerializes the retained short capture
	// even though gob IsAutomated matches refreshRawSessionAutomation.
	empty := longer
	empty.Outcome.Results = nil
	b, _ = f.accept(t, "device-b", "withdraw-longer", br.Receipt, parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, empty))

	restored, err := f.sink.Resolve(ctx, "review-restore-recency")
	require.NoError(t, err)
	require.Equal(t, shortResolved.SessionID, restored.SessionID)

	var isAutomated bool
	var gotOutcome, gotConfidence string
	var gotHealthScore *int
	var gotHealthGrade *string
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT is_automated, outcome, outcome_confidence, health_score, health_grade
		 FROM sessions WHERE id = $1`, restored.SessionID,
	).Scan(&isAutomated, &gotOutcome, &gotConfidence, &gotHealthScore, &gotHealthGrade))
	assert.True(t, isAutomated)
	assert.Equal(t, "unknown", gotOutcome)
	assert.Equal(t, "low", gotConfidence)
	assert.Nil(t, gotHealthScore)
	assert.Nil(t, gotHealthGrade)

	var recencyBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT recency_state FROM raw_content_revisions WHERE session_id = $1`, restored.SessionID,
	).Scan(&recencyBytes))
	recency, err := decodeRawRecency(recencyBytes)
	require.NoError(t, err)
	assert.Equal(t, "unknown", recency.Outcome)
	assert.Equal(t, "low", recency.OutcomeConfidence)
	assert.Nil(t, recency.HealthScore)
	assert.Nil(t, recency.HealthGrade)
}

func TestRawProjectionReconcileSpawnLinksScopedToAffectedGroups(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	unrelated := projectionOutcome(prompt)
	unrelated.Outcome.Results[0].Result.Session.ID = "unrelated-worker"
	unrelated.Outcome.Results[0].Result.Session.SourceSessionID = "unrelated-worker"
	unrelated.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	unrelated.Outcome.Results[0].Result.Session.Entrypoint = "sdk-cli"
	unrelated.Outcome.Results[0].Result.Session.RelationshipType = parser.RelSubagent
	unrelated.Outcome.Results[0].Result.Messages[1].ToolCalls = nil

	a, _ := f.accept(t, "device-a", "unrelated-worker", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, a), a, unrelated))
	unrelatedResolved, err := f.sink.Resolve(ctx, "unrelated-worker")
	require.NoError(t, err)

	// Force a stale relationship on the unrelated worker to prove importing a
	// different group does not scan or rewrite unrelated raw sessions.
	_, err = f.runtime.ExecContext(ctx,
		`UPDATE sessions SET relationship_type = 'subagent' WHERE id = $1`, unrelatedResolved.SessionID,
	)
	require.NoError(t, err)

	other := projectionOutcome("Explain this package.")
	other.Outcome.Results[0].Result.Session.ID = "other-group-session"
	other.Outcome.Results[0].Result.Session.SourceSessionID = "other-group-session"
	b, _ := f.accept(t, "device-b", "other-group-session", "", parser.AgentCodex)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, b), b, other))

	var rel string
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT relationship_type FROM sessions WHERE id = $1`, unrelatedResolved.SessionID,
	).Scan(&rel))
	assert.Equal(t, "subagent", rel, "unrelated raw session must not be touched by another group's import")
}
