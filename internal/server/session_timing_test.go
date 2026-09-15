package server_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/sessionwatch"
)

func TestHandleSessionTiming_OK(t *testing.T) {
	for _, measured := range []bool{false, true} {
		name := "unknown"
		if measured {
			name = "measured"
		}
		t.Run(name, func(t *testing.T) {
			te := setup(t)
			seedTimingFixture(t, te.db, "timing-handler-ok", measured)
			w := te.get(t, "/api/v1/sessions/timing-handler-ok/timing")
			assertStatus(t, w, http.StatusOK)
			assertTimingPayload(t, w.Body.Bytes(), "timing-handler-ok", measured)
		})
	}
}

func TestHandleSessionTiming_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/missing/timing")
	assertStatus(t, w, http.StatusNotFound)
}

func TestHandleSessionTiming_SSEInitialAndUpdate(t *testing.T) {
	t.Cleanup(sessionwatch.SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
	te := setup(t)
	const sessionID = "timing-handler-sse"
	seedTimingFixture(t, te.db, sessionID, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := newFlushRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/watch", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); te.handler.ServeHTTP(w, req) }()
	t.Cleanup(func() { cancel(); <-done })
	te.waitForSSEEvent(t, w, "session.timing", 3*time.Second)
	initial := timingSSEPayloads(w)
	require.Len(t, initial, 1)
	assertTimingPayload(t, []byte(initial[0]), sessionID, false)

	messages, err := te.db.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	messages[1].ToolCalls[0].ResultEvents = timingExecutionEvents()
	require.NoError(t, te.db.ReplaceSessionMessages(sessionID, messages))
	require.Eventually(t, func() bool { return len(timingSSEPayloads(w)) >= 2 }, 3*time.Second, 10*time.Millisecond)
	updates := timingSSEPayloads(w)
	assertTimingPayload(t, []byte(updates[len(updates)-1]), sessionID, true)
	assert.True(t, hasSSEEvent(w, "session_updated"))
}

func timingSSEPayloads(w *flushRecorder) []string {
	var payloads []string
	for _, event := range parseSSE(w.BodyString()) {
		if event.Event == "session.timing" {
			payloads = append(payloads, event.Data)
		}
	}
	return payloads
}

func assertTimingPayload(t *testing.T, payload []byte, sessionID string, measured bool) {
	t.Helper()
	var raw map[string]any
	require.NoError(t, json.Unmarshal(payload, &raw))
	assert.Contains(t, raw, "activity")
	assert.Contains(t, raw, "activity_totals")
	var got db.SessionTiming
	require.NoError(t, json.Unmarshal(payload, &got))
	assert.Equal(t, sessionID, got.SessionID)
	assert.Equal(t, 1, got.TurnCount)
	assert.Equal(t, 1, got.ToolCallCount)
	assert.Equal(t, int64(6000), got.TotalDurationMs)
	tool, unattributed := int64(0), int64(6000)
	if measured {
		tool, unattributed = 2000, 4000
	}
	assert.Equal(t, tool, got.ToolDurationMs)
	assert.Equal(t, map[string]any{"thinking_ms": float64(0), "generation_ms": float64(0), "tool_ms": float64(tool), "unattributed_ms": float64(unattributed)}, raw["activity_totals"])
	assert.Equal(t, db.ActivityTotals{ToolMs: tool, UnattributedMs: unattributed}, got.ActivityTotals)
	require.Len(t, got.Activity, 2)
	assert.Positive(t, got.Activity[0].MessageID)
	assert.Equal(t, 0, got.Activity[0].Ordinal)
	assert.Equal(t, "2026-04-26T10:00:00Z", got.Activity[0].StartedAt)
	assert.Equal(t, int64(6000), got.Activity[0].DurationMs)
	assert.Equal(t, tool, got.Activity[0].ToolMs)
	assert.Equal(t, unattributed, got.Activity[0].UnattributedMs)
	assert.Zero(t, got.Activity[0].ThinkingMs)
	assert.Zero(t, got.Activity[0].GenerationMs)
	assert.Equal(t, "message_only", got.Activity[0].Precision)
	assert.False(t, got.Activity[0].Running)
	require.Len(t, got.Turns, 1)
	require.Len(t, got.Turns[0].Calls, 1)
	assert.Equal(t, "tu_1", got.Turns[0].Calls[0].ToolUseID)
	if measured {
		assert.Equal(t, new(int64(2000)), got.Turns[0].Calls[0].DurationMs)
	} else {
		assert.Nil(t, got.Turns[0].Calls[0].DurationMs)
	}
}

func timingExecutionEvents() []db.ToolResultEvent {
	return []db.ToolResultEvent{
		{ToolUseID: "tu_1", Source: "tool_execution", Status: "started", Timestamp: "2026-04-26T10:00:02Z"},
		{ToolUseID: "tu_1", Source: "tool_execution", Status: "completed", Timestamp: "2026-04-26T10:00:04Z"},
	}
}

func seedTimingFixture(t *testing.T, d *db.DB, sessionID string, measured bool) {
	t.Helper()
	const (
		startedAt = "2026-04-26T10:00:00Z"
		endedAt   = "2026-04-26T10:00:06Z"
	)
	dbtest.SeedSession(t, d, sessionID, "timing-test",
		func(s *db.Session) {
			s.MessageCount = 3
			s.UserMessageCount = 2
			s.StartedAt = new(string(startedAt))
			s.EndedAt = new(string(endedAt))
		})

	msgs := []db.Message{
		{
			SessionID:     sessionID,
			Ordinal:       0,
			Role:          "user",
			Content:       "go",
			ContentLength: 2,
			Timestamp:     "2026-04-26T10:00:00Z",
		},
		{
			SessionID:     sessionID,
			Ordinal:       1,
			Role:          "assistant",
			Content:       "running",
			ContentLength: 7,
			Timestamp:     "2026-04-26T10:00:01Z",
			HasToolUse:    true,
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Bash",
					Category:  "Bash",
					ToolUseID: "tu_1",
					InputJSON: "{}",
				},
			},
		},
		{
			SessionID:     sessionID,
			Ordinal:       2,
			Role:          "user",
			Content:       "ok",
			ContentLength: 2,
			Timestamp:     "2026-04-26T10:00:06Z",
		},
	}
	if measured {
		msgs[1].ToolCalls[0].ResultEvents = timingExecutionEvents()
	}
	require.NoError(t, d.ReplaceSessionMessages(sessionID, msgs))
}
