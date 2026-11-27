//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func codexCloudProjection(messages int, tokens bool, plan *parser.ParsedPlanUsage) rawderive.ParsedManifest {
	session := parser.ParsedSession{
		ID: "codex-cloud:thread-a", SourceSessionID: "thread-a", Agent: parser.AgentCodexCloud,
		Project: "synthetic", Machine: "capture-device", StartedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		PlanUsage: plan,
	}
	var usage []parser.ParsedUsageEvent
	if tokens {
		usage = []parser.ParsedUsageEvent{{
			SessionID: session.ID, Source: "session", Model: "synthetic-model",
			InputTokens: 1000, OutputTokens: 100, OccurredAt: "2026-10-01T12:05:00Z", DedupKey: "thread-a:0:1100",
		}}
		session.UsageEvents = usage
		session.TotalOutputTokens, session.HasTotalOutputTokens = 100, true
		session.PeakContextTokens, session.HasPeakContextTokens = 1000, true
		observed := time.Date(2026, 10, 1, 12, 5, 0, 0, time.UTC)
		session.CaptureCoverage = &parser.ParsedCaptureCoverage{LastCaptureAt: &observed}
	}
	var transcript []parser.ParsedMessage
	for i := range messages {
		role := parser.RoleUser
		if i%2 == 1 {
			role = parser.RoleAssistant
		}
		transcript = append(transcript, parser.ParsedMessage{Ordinal: i, Role: role, Content: "message"})
	}
	return rawderive.ParsedManifest{Outcome: parser.ParseOutcome{
		ResultSetComplete: true, ForceReplace: true,
		Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
			Session: session, Messages: transcript, UsageEvents: usage,
		}}},
	}}
}

// A rebuilt mirror uploaded before its history is fetched again must not
// replace the hub's transcript, and the refetched history must not drop
// the hub's live token counts.
func TestRawProjectionCodexCloudRebuiltMirrorKeepsHistory(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	store := &Store{pg: f.runtime}
	m1, accepted := f.accept(t, "device-a", "capture-a", "", parser.AgentCodexCloud)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m1), m1, codexCloudProjection(2, true, nil)))

	// The rebuilt file holds the listing and a plan usage outcome only.
	m2, accepted := f.accept(t, "device-a", "capture-b", accepted.Receipt, parser.AgentCodexCloud)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m2), m2, codexCloudProjection(0, false,
		&parser.ParsedPlanUsage{OutcomeKind: "left_out"})))
	resolved, err := f.sink.Resolve(t.Context(), "codex-cloud:thread-a")
	require.NoError(t, err)
	messages, err := store.GetMessages(t.Context(), resolved.SessionID, 0, 10, true)
	require.NoError(t, err)
	assert.Len(t, messages, 2, "the empty mirror keeps the transcript")

	// The refetched history has a new message and no token records.
	m3, _ := f.accept(t, "device-a", "capture-c", accepted.Receipt, parser.AgentCodexCloud)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m3), m3, codexCloudProjection(3, false,
		&parser.ParsedPlanUsage{})))
	resolved, err = f.sink.Resolve(t.Context(), "codex-cloud:thread-a")
	require.NoError(t, err)
	messages, err = store.GetMessages(t.Context(), resolved.SessionID, 0, 10, true)
	require.NoError(t, err)
	assert.Len(t, messages, 3, "the new message imports")
	usage, err := store.GetSessionUsageRows(t.Context(), []string{resolved.SessionID})
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Len(t, usage.Rows, 1, "the live usage event stays")
	assert.Equal(t, 1000, usage.Rows[0].InputTokens)
	session, err := store.GetSessionFull(t.Context(), resolved.SessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.HasTotalOutputTokens)
	assert.Equal(t, 100, session.TotalOutputTokens)
	assert.True(t, session.HasContextData, "the signals see the carried peak context")

	rows, err := f.runtime.QueryContext(t.Context(), `SELECT manifest_id,prior_contributed FROM raw_source_contributions ORDER BY projection_generation`)
	require.NoError(t, err)
	defer rows.Close()
	contributed := map[string]bool{}
	for rows.Next() {
		var manifest string
		var prior bool
		require.NoError(t, rows.Scan(&manifest, &prior))
		contributed[manifest] = prior
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]bool{m1.ManifestID: false, m2.ManifestID: true, m3.ManifestID: true}, contributed,
		"the kept transcript and the carried token counts come from the previous projection")
}

func TestRawProjectionCodexCloudPreserveUsesCurrentAutomationAfterAudit(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	ctx := t.Context()

	prompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	initial := codexCloudProjection(2, false, nil)
	initial.Outcome.Results[0].Result.Messages[0].Content = prompt
	initial.Outcome.Results[0].Result.Messages[1].Content = "Looks good."

	m1, accepted := f.accept(t, "device-a", "codex-cloud-review", "", parser.AgentCodexCloud)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, m1), m1, initial))

	// Replace the projected row with a consistent pre-upgrade capture whose
	// content-derived ID and revision match IsAutomated=false, completed/medium
	// outcome, and 100/A health score.
	var groupID, currentID string
	var currentPayload []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT r.group_id, r.session_id, r.payload
		 FROM raw_session_branches b
		 JOIN raw_content_revisions r ON r.session_id = b.captured_session_id
		 WHERE b.member_id = 'codex-cloud:thread-a'`,
	).Scan(&groupID, &currentID, &currentPayload))

	preUpgrade, err := decodeRawPayload(currentPayload)
	require.NoError(t, err)
	preUpgrade.Session.IsAutomated = false
	preUpgrade.Signals = ingest.RefreshSignalRecencyAt(
		preUpgrade.Session, preUpgrade.Messages, preUpgrade.Signals,
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	)
	require.Equal(t, "completed", preUpgrade.Signals.Outcome)
	require.Equal(t, "medium", preUpgrade.Signals.OutcomeConfidence)

	preUpgradeBytes, err := encodeRawPayload(preUpgrade)
	require.NoError(t, err)
	legacyRev, err := rawContentRevision(preUpgrade)
	require.NoError(t, err)
	legacyID := "raw-row-" + rawDigest("row-v1", groupID, legacyRev)
	require.NotEqual(t, currentID, legacyID)

	priorMetaBytes, err := encodeRawPayload(ingest.PreparedSession{Session: preUpgrade.Session})
	require.NoError(t, err)

	tx, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO raw_content_revisions(session_id, group_id, content_revision, payload, recency_state)
		 VALUES ($1, $2, $3, $4, '{"Outcome":"completed","OutcomeConfidence":"medium","HealthScore":100,"HealthGrade":"A"}'::jsonb)`,
		legacyID, groupID, legacyRev, preUpgradeBytes,
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`UPDATE raw_session_branches
		 SET session_id = $1, captured_session_id = $1, content_revision = $2, prior_payload = $3
		 WHERE member_id = 'codex-cloud:thread-a'`,
		legacyID, legacyRev, priorMetaBytes,
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`UPDATE session_sources
		 SET session_id = $1, physical_session_id = NULL, content_revision = $2
		 WHERE physical_session_id = $3`,
		legacyID, legacyRev, currentID,
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE raw_message_dialogue SET session_id = $1 WHERE session_id = $2`, legacyID, currentID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, currentID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM raw_embedding_outbox WHERE session_id = $1`, currentID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM raw_content_revisions WHERE session_id = $1`, currentID)
	require.NoError(t, err)
	require.NoError(t, f.sink.writePayload(ctx, tx, legacyID, groupID, legacyRev, preUpgrade))
	_, err = tx.ExecContext(ctx,
		`UPDATE sessions
		 SET is_automated = FALSE,
		     outcome = 'completed',
		     outcome_confidence = 'medium',
		     health_score = 100,
		     health_grade = 'A'
		 WHERE id = $1`, legacyID,
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`UPDATE raw_content_revisions
		 SET recency_state = '{"Outcome":"completed","OutcomeConfidence":"medium","HealthScore":100,"HealthGrade":"A"}'::jsonb
		 WHERE session_id = $1`, legacyID,
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE session_sources SET physical_session_id = $1 WHERE session_id = $1`, legacyID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	before, err := f.sink.Resolve(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	require.Equal(t, legacyID, before.SessionID)

	_, err = f.admin.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key = $1`, db.ClassifierHashKey)
	require.NoError(t, err)

	require.NoError(t, EnsureHostedTenant(ctx, f.admin, f.schema, f.tenant))

	// Uploading a shorter rebuilt mirror triggers HistoryPreserve and must keep
	// the audited unknown/low outcome and nil health without rewriting payload.
	m2, _ := f.accept(t, "device-a", "codex-cloud-mirror", accepted.Receipt, parser.AgentCodexCloud)
	require.NoError(t, f.sink.Project(ctx, f.lease(t, m2), m2, codexCloudProjection(0, false,
		&parser.ParsedPlanUsage{OutcomeKind: "left_out"})))

	after, err := f.sink.Resolve(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID, "preserving history must keep the same session ID")

	var afterBytes []byte
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT payload FROM raw_content_revisions WHERE session_id = $1`, after.SessionID,
	).Scan(&afterBytes))
	assert.Equal(t, preUpgradeBytes, afterBytes, "preserving history must leave stored payload unchanged")

	var isAutomated bool
	var gotOutcome, gotConfidence string
	var gotScore *int
	var gotGrade *string
	require.NoError(t, f.runtime.QueryRowContext(ctx,
		`SELECT is_automated, outcome, outcome_confidence, health_score, health_grade
		 FROM sessions WHERE id = $1`, after.SessionID,
	).Scan(&isAutomated, &gotOutcome, &gotConfidence, &gotScore, &gotGrade))
	assert.True(t, isAutomated)
	assert.Equal(t, "unknown", gotOutcome)
	assert.Equal(t, "low", gotConfidence)
	assert.Nil(t, gotScore)
	assert.Nil(t, gotGrade)
}
