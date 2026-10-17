//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
