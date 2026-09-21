//go:build chtest

package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

func TestStoreActivityReportAndRecentEdits(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()
	fixedNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-10", Timezone: "UTC",
	}, fixedNow)
	require.NoError(t, err)

	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Timezone:         "UTC",
		IncludeSubagents: true,
	}, q)
	require.NoError(t, err)
	assert.False(t, report.Partial)
	assert.Equal(t, 2, report.Totals.Sessions,
		"alpha root and its subagent child on 2026-01-10")

	edits, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, edits.Files, 1)
	assert.Equal(t, "alpha", edits.Files[0].Project)
	assert.Equal(t, "src/main.go", edits.Files[0].FilePath)
	assert.Equal(t, 1, edits.Files[0].EditCount)
	assert.Equal(t, fixtureAlphaID, edits.Files[0].LastSessionID)
}

func TestActivityReportIncludesSessionWithOnlyToolEventInWindow(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := context.Background()
	const sessionID = "ch-tool-window"
	started := "2026-01-09T10:00:00.000Z"
	called := "2026-01-09T10:01:00.000Z"
	completed := "2026-01-10T12:00:00.000Z"
	sess := fixtureSession(sessionID, "tools", "run the sample", started, 2)
	sess.EndedAt = &started
	call := fixtureMessage(sessionID, 1, "assistant", "", called, db.ToolCall{
		ToolName:  "sample_tool",
		Category:  "Other",
		ToolUseID: "sample-call",
		ResultEvents: []db.ToolResultEvent{
			{
				ToolUseID: "sample-call", Source: "tool_execution",
				Status: "started", Timestamp: called, EventIndex: 0,
			},
			{
				ToolUseID: "sample-call", Source: "tool_execution",
				Status: "completed", Timestamp: completed, EventIndex: 1,
			},
		},
	})
	call.HasToolUse = true
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: sess,
		Messages: []db.Message{
			fixtureMessage(sessionID, 0, "user", "run the sample", started),
			call,
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	fixedNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-10", Timezone: "UTC",
	}, fixedNow)
	require.NoError(t, err)
	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{Timezone: "UTC"}, q)
	require.NoError(t, err)
	ids := make([]string, 0, len(report.BySession))
	for _, row := range report.BySession {
		ids = append(ids, row.SessionID)
	}
	assert.Contains(t, ids, sessionID,
		"a session whose only in-window activity is a tool_execution event must appear")
}

// TestActivityReportUsageHandlesOversizedClaudeSnapshotKeys pushes enough
// distinct Claude (message_id, request_id) pairs that inlining them into the
// peer query would exceed ClickHouse's default 256 KiB query size. The peer
// session outside the candidate set carries fuller snapshots for some of
// those pairs, so the report must both survive the key volume and still pick
// the peer token rows while attributing them to the earliest session.
func TestActivityReportUsageHandlesOversizedClaudeSnapshotKeys(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := t.Context()

	const (
		candidateSessions   = 25
		messagesPerSession  = 60
		peerSnapshotCount   = 40
		idPadding           = 96
		clickHouseMaxQuery  = 262144
		candidateOutput     = 2
		peerOutput          = 5
		candidateTokenUsage = `{"input_tokens":1,"output_tokens":2}`
		peerTokenUsage      = `{"input_tokens":1,"output_tokens":5}`
	)
	pad := strings.Repeat("x", idPadding)
	messageID := func(session, ordinal int) string {
		return fmt.Sprintf("msg_%s_%03d_%03d", pad, session, ordinal)
	}
	requestID := func(session, ordinal int) string {
		return fmt.Sprintf("req_%s_%03d_%03d", pad, session, ordinal)
	}

	var writes []db.SessionBatchWrite
	inlinedPairBytes := 0
	for i := range candidateSessions {
		id := fmt.Sprintf("ch-snapshot-cand-%03d", i)
		started := time.Date(2026, 1, 10, 1, i, 0, 0, time.UTC)
		sess := fixtureSession(id, "snapshot", "candidate", started.Format(time.RFC3339Nano), messagesPerSession+1)
		msgs := []db.Message{fixtureMessage(id, 0, "user", "candidate", started.Format(time.RFC3339Nano))}
		for j := range messagesPerSession {
			ts := started.Add(time.Duration(j+1) * time.Second).Format(time.RFC3339Nano)
			m := fixtureMessage(id, j+1, "assistant", "reply", ts)
			m.ClaudeMessageID = messageID(i, j)
			m.ClaudeRequestID = requestID(i, j)
			m.TokenUsage = []byte(candidateTokenUsage)
			msgs = append(msgs, m)
			inlinedPairBytes += len("('" + m.ClaudeMessageID + "', '" + m.ClaudeRequestID + "'), ")
		}
		writes = append(writes, db.SessionBatchWrite{
			Session: sess, Messages: msgs, DataVersion: 1, ReplaceMessages: true,
		})
	}
	require.Greater(t, inlinedPairBytes, clickHouseMaxQuery,
		"fixture must exceed the ClickHouse query size limit when pairs are inlined")

	// The peer session ended before the report day, so it is not a candidate.
	// Its transcript copies the first candidate's snapshots one second later
	// with more output tokens, which is the fuller snapshot the survivor
	// selection must keep and attribute to the earlier candidate session.
	const peerID = "ch-snapshot-peer"
	peerStarted := time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC)
	peer := fixtureSession(peerID, "snapshot", "peer", peerStarted.Format(time.RFC3339Nano), peerSnapshotCount+1)
	peerMsgs := []db.Message{fixtureMessage(peerID, 0, "user", "peer", peerStarted.Format(time.RFC3339Nano))}
	candidateZeroStart := time.Date(2026, 1, 10, 1, 0, 0, 0, time.UTC)
	for j := range peerSnapshotCount {
		ts := candidateZeroStart.Add(time.Duration(j+1)*time.Second + time.Second).Format(time.RFC3339Nano)
		m := fixtureMessage(peerID, j+1, "assistant", "reply", ts)
		m.ClaudeMessageID = messageID(0, j)
		m.ClaudeRequestID = requestID(0, j)
		m.TokenUsage = []byte(peerTokenUsage)
		peerMsgs = append(peerMsgs, m)
	}
	writes = append(writes, db.SessionBatchWrite{
		Session: peer, Messages: peerMsgs, DataVersion: 1, ReplaceMessages: true,
	})
	_, err := local.WriteSessionBatchAtomic(ctx, writes)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	fixedNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-10", Timezone: "UTC",
	}, fixedNow)
	require.NoError(t, err)
	filter := db.AnalyticsFilter{Timezone: "UTC", Project: "snapshot"}

	want, err := local.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	got, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)

	// Every fixture message, including the opening user message, reports
	// candidateOutput tokens; the peer snapshots replace that many pairs.
	wantOutput := candidateSessions*(messagesPerSession+1)*candidateOutput +
		peerSnapshotCount*(peerOutput-candidateOutput)
	assert.Equal(t, wantOutput, want.Totals.OutputTokens,
		"SQLite reference must count the fuller peer snapshots")
	assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens)
	assert.Equal(t, want.Totals.Cost, got.Totals.Cost)
	assert.Equal(t, want.Totals.Sessions, got.Totals.Sessions)
	require.Len(t, got.BySession, candidateSessions)

	wantBySession := make(map[string]activity.SessionRow, len(want.BySession))
	for _, row := range want.BySession {
		wantBySession[row.SessionID] = row
	}
	for _, row := range got.BySession {
		ref, ok := wantBySession[row.SessionID]
		require.True(t, ok, "unexpected session %s in ClickHouse report", row.SessionID)
		assert.Equal(t, ref.OutputTokens, row.OutputTokens, row.SessionID)
		assert.Equal(t, ref.Cost, row.Cost, row.SessionID)
	}
	assert.NotContains(t, wantBySession, peerID)
}
