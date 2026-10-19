package server

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
)

func TestReportEvidenceArchiveToGenerator(t *testing.T) {
	srv := testServer(t, 0)
	ts := "2025-01-15T12:00:00Z"
	placeholder := `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`
	task := "Implement only the handler; acceptance criteria: verify tests pass."
	for _, s := range []db.Session{
		{ID: "ambient", Agent: "codex", FirstMessage: &placeholder, HasContextData: true},
		{ID: "human", Agent: "claude", FirstMessage: &task, HasContextData: true, ContextPressureMax: new(0.5)},
	} {
		s.Project = "sample-app"
		s.Machine = "test"
		s.StartedAt = &ts
		s.EndedAt = &ts
		s.UserMessageCount = 3
		s.MessageCount = 6
		s.RelationshipType = "root"
		s.DataVersion = 1
		require.NoError(t, srv.db.UpsertSession(t.Context(), s))
		require.NoError(t, srv.db.(*db.DB).UpdateSessionSignals(t.Context(), s.ID, db.SessionSignalUpdate{
			HasContextData:     s.HasContextData,
			ContextPressureMax: s.ContextPressureMax,
		}))
		stored, err := srv.db.GetSession(t.Context(), s.ID)
		require.NoError(t, err)
		require.NotNil(t, stored)
		require.Equal(t, s.HasContextData, stored.HasContextData)
		require.Equal(t, s.ContextPressureMax, stored.ContextPressureMax)
	}
	payload, hash, _, err := srv.buildCannedPayload(t.Context(), insight.CannedWorkflowHygieneReview,
		generateInsightRequest{DateFrom: "2025-01-15", DateTo: "2025-01-15", Agent: "claude"},
		insight.CannedSessionFilters{Timezone: "UTC", AutomatedScope: "all"}, insight.GenerateOptions{})
	require.NoError(t, err)
	prompt, err := insight.BuildCannedPrompt(payload, hash)
	require.NoError(t, err)
	_, body, ok := strings.Cut(prompt, "Aggregate payload JSON:\n")
	require.True(t, ok)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &obj))
	coach := obj["coach"].(map[string]any)
	assert.Equal(t, float64(1), coach["scored_prompt_sessions"])
	assert.Equal(t, map[string]any{"session_count": float64(2), "sessions_with_context_tokens": float64(2), "sessions_with_context_pressure": float64(1)}, coach["context_coverage"])
	// Signal rows are preserved; only the report's interpretation gains explicit coverage.
	assert.Equal(t, 1, payload.Signals.ContextHealth.SessionsWithContextData)
	stored, err := srv.db.GetSession(t.Context(), "ambient")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, placeholder, *stored.FirstMessage)
}
