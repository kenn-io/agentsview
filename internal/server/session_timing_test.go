package server_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

func TestHandleSessionTiming_OK(t *testing.T) {
	te := setup(t)
	seedTimingFixture(t, te.db, "timing-handler-ok")

	w := te.get(t, "/api/v1/sessions/timing-handler-ok/timing")
	assertStatus(t, w, http.StatusOK)

	got := decode[db.SessionTiming](t, w)
	assert.Equal(t, "timing-handler-ok", got.SessionID)
	assert.Equal(t, 1, got.TurnCount)
	assert.Equal(t, 1, got.ToolCallCount)
	require.Len(t, got.Turns, 1)
	require.NotNil(t, got.Turns[0].DurationMs)
	assert.Equal(t, int64(29_000), *got.Turns[0].DurationMs)
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
			Timestamp:     "2026-04-26T10:00:30Z",
		},
	}
	require.NoError(t, d.ReplaceSessionMessages(sessionID, msgs))
}

func TestHandleSessionTimingWithoutCallsMatchesContract(t *testing.T) {
	te := setup(t)
	dbtest.SeedSession(t, te.db, "timing-no-calls", "timing-test")
	w := te.get(t, "/api/v1/sessions/timing-no-calls/timing")
	assertStatus(t, w, http.StatusOK)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Contains(t, body, "slowest_call")
	assert.Nil(t, body["slowest_call"])
	spec := server.OpenAPISpec(server.VersionInfo{})
	schema := spec.Paths["/api/v1/sessions/{id}/timing"].Get.Responses["200"].Content["application/json"].Schema
	result := &huma.ValidateResult{}
	huma.Validate(spec.Components.Schemas, schema, &huma.PathBuffer{}, huma.ModeReadFromServer, body, result)
	assert.Empty(t, result.Errors)
}
