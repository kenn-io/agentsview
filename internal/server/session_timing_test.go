package server_test

import (
	"net/http"
	"testing"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/dbtest"
)

func TestHandleSessionTiming_OK(t *testing.T) {
	te := setup(t)
	seedTimingFixture(t, te.db, "timing-handler-ok")

	w := te.get(t, "/api/v1/sessions/timing-handler-ok/timing")
	assertStatus(t, w, http.StatusOK)

	got := decode[db.SessionTiming](t, w)
	if got.SessionID != "timing-handler-ok" {
		t.Errorf("SessionID = %q, want timing-handler-ok",
			got.SessionID)
	}
	if got.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", got.TurnCount)
	}
	if got.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", got.ToolCallCount)
	}
	if len(got.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(got.Turns))
	}
	if got.Turns[0].DurationMs == nil ||
		*got.Turns[0].DurationMs != 29_000 {
		t.Errorf("turn duration = %v, want 29000",
			got.Turns[0].DurationMs)
	}
}

func TestHandleSessionTiming_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/missing/timing")
	assertStatus(t, w, http.StatusNotFound)
}

// seedTimingFixture inserts a session with one assistant turn containing
// a single Bash tool call followed by a user message, so the handler
// returns a populated SessionTiming payload (TurnCount=1, ToolCallCount=1,
// turn duration = user_followup - assistant_with_tools = 29s).
func seedTimingFixture(t *testing.T, d *db.DB, sessionID string) {
	t.Helper()
	const (
		startedAt = "2026-04-26T10:00:00Z"
		endedAt   = "2026-04-26T10:00:30Z"
	)
	dbtest.SeedSession(t, d, sessionID, "timing-test",
		func(s *db.Session) {
			s.MessageCount = 3
			s.UserMessageCount = 2
			s.StartedAt = dbtest.Ptr(startedAt)
			s.EndedAt = dbtest.Ptr(endedAt)
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
			Timestamp:     "2026-04-26T10:00:30Z",
		},
	}
	if err := d.ReplaceSessionMessages(sessionID, msgs); err != nil {
		t.Fatalf("seeding timing fixture: %v", err)
	}
}
