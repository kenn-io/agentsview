package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestGenerateInsightRejectsWriterClosedBeforeStream pins the maintenance-mode
// UX: while a worker pass holds the write barrier, insight generation fails at
// request time with the transient 503 + Retry-After instead of running for
// minutes and then failing to save.
func TestGenerateInsightRejectsWriterClosedBeforeStream(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	local, ok := srv.db.(*db.DB)
	require.True(t, ok)
	require.NoError(t, local.CloseWriter())
	t.Cleanup(func() { require.NoError(t, local.ReopenWriter()) })

	w := serveJSON(t, srv.Handler(), http.MethodPost, "/api/v1/insights/generate",
		map[string]any{
			"type":      "daily_activity",
			"date_from": "2026-01-01",
			"date_to":   "2026-01-01",
		})

	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"body: %s", w.Body.String())
	assert.Equal(t, writerClosedRetryAfterSeconds, w.Header().Get("Retry-After"))
	assert.NotContains(t, w.Body.String(), "event:",
		"the rejection must not open an SSE stream")
}

// TestActivityRangeSummaryUsesRequestTimezone confirms the insight activity
// summary resolves its window in the request timezone, so a non-UTC viewer's
// summary covers the same local-day window as the activity dashboard the dates
// were derived from. A session whose only instant is 2026-06-16T02:00:00Z is
// the local day 2026-06-15 in America/New_York (UTC-4 in June) but the UTC day
// 2026-06-16, so the June-15 summary must include it under New York and
// exclude it under UTC. Before the fix the window was always UTC, so the New
// York request would have wrongly excluded the session.
func TestActivityRangeSummaryUsesRequestTimezone(t *testing.T) {
	srv := testServer(t, 0)
	ctx := context.Background()
	ts := "2026-06-16T02:00:00Z"
	require.NoError(t, srv.db.UpsertSession(db.Session{
		ID: "x", Project: "proj", Machine: "test", Agent: "claude",
		StartedAt: &ts, EndedAt: &ts, MessageCount: 1,
		RelationshipType: "root", DataVersion: 1,
	}))
	require.NoError(t, srv.db.ReplaceSessionMessages("x", []db.Message{{
		SessionID: "x", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp: ts, Model: "m1",
	}}))

	ny, err := srv.activityRangeSummary(ctx, generateInsightRequest{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
		Timezone: "America/New_York",
	})
	require.NoError(t, err)
	require.NotNil(t, ny)
	assert.Equal(t, 1, ny.Sessions,
		"New York June-15 window covers the 02:00Z instant (22:00 local)")

	utc, err := srv.activityRangeSummary(ctx, generateInsightRequest{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
		Timezone: "UTC",
	})
	require.NoError(t, err)
	require.NotNil(t, utc)
	assert.Equal(t, 0, utc.Sessions,
		"UTC June-15 window ends at June 16 00:00Z, before the instant")
}

// TestActivityRangeSummaryAppliesAutomatedScope confirms the insight activity
// summary honors the request's automated scope rather than always excluding
// automated sessions, so an automated_scope=all request attaches a summary that
// covers the same sessions BuildPrompt's list does. Before the fix the summary
// hard-coded ExcludeAutomated, so "all" and "automated" requests undercounted.
func TestActivityRangeSummaryAppliesAutomatedScope(t *testing.T) {
	srv := testServer(t, 0)
	ctx := context.Background()
	ts := "2026-06-15T12:00:00Z"

	// Interactive session: multi-turn, ordinary prompt.
	require.NoError(t, srv.db.UpsertSession(db.Session{
		ID: "human", Project: "proj", Machine: "test", Agent: "claude",
		StartedAt: &ts, EndedAt: &ts, MessageCount: 4, UserMessageCount: 2,
		RelationshipType: "root", DataVersion: 1,
	}))
	require.NoError(t, srv.db.ReplaceSessionMessages("human", []db.Message{{
		SessionID: "human", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp: ts, Model: "m1",
	}}))

	// Automated session: a single-turn review prompt sets is_automated.
	reviewPrompt := "You are a code reviewer. Review the code."
	require.NoError(t, srv.db.UpsertSession(db.Session{
		ID: "auto", Project: "proj", Machine: "test", Agent: "claude",
		StartedAt: &ts, EndedAt: &ts, MessageCount: 3, UserMessageCount: 1,
		FirstMessage: &reviewPrompt, RelationshipType: "root", DataVersion: 1,
	}))
	require.NoError(t, srv.db.ReplaceSessionMessages("auto", []db.Message{{
		SessionID: "auto", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp: ts, Model: "m1",
	}}))

	base := generateInsightRequest{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
		Timezone: "UTC",
	}

	human := base
	human.AutomatedScope = "human"
	humanSummary, err := srv.activityRangeSummary(ctx, human)
	require.NoError(t, err)
	require.NotNil(t, humanSummary)
	assert.Equal(t, 1, humanSummary.Sessions,
		"human scope counts only the interactive session")

	all := base
	all.AutomatedScope = "all"
	allSummary, err := srv.activityRangeSummary(ctx, all)
	require.NoError(t, err)
	require.NotNil(t, allSummary)
	assert.Equal(t, 2, allSummary.Sessions,
		"all scope counts interactive and automated sessions")
}
