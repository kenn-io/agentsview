package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExportConversationsToolSummaries(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(testDataDir(t), "sessions.db"))
	insertExportSessionsTestSession(t, database, db.Session{ID: "chat", Project: "sample", Machine: "local", Agent: "sample-agent"})
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
		SessionID: "chat", Ordinal: 0, Role: "assistant", SourceUUID: "reply", Content: "Running the checks.", HasToolUse: true,
		ToolCalls: []db.ToolCall{
			{ToolName: "Bash", Category: "Bash", ToolUseID: "run", ResultEvents: []db.ToolResultEvent{
				{ToolUseID: "run", Source: "tool_execution", Status: "started", Timestamp: "2026-03-01T10:00:00Z", EventIndex: 0},
				{ToolUseID: "run", Source: "tool_execution", Status: "completed", Timestamp: "2026-03-01T10:00:02.500Z", EventIndex: 1},
			}},
			{ToolName: "Skill", Category: "Skill", ToolUseID: "skill", SkillName: "review-change"},
		},
	}}))
	require.NoError(t, database.UpdateSessionSignals(t.Context(), "chat", db.SessionSignalUpdate{ToolObservations: []db.ToolObservation{
		{MessageOrdinal: 0, CallIndex: 0, Outcome: "errored", Repeat: "none"},
	}}))
	require.NoError(t, database.Close())

	type page struct {
		Checkpoint string `json:"checkpoint"`
		Changes    []struct {
			Type        string         `json:"type"`
			SessionID   string         `json:"session_id"`
			ToolSummary jsontext.Value `json:"tool_summary"`
		} `json:"changes"`
	}
	stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "conversations", "changes")
	require.NoError(t, err)
	var plain page
	require.NoError(t, json.Unmarshal([]byte(stdout), &plain))
	require.Len(t, plain.Changes, 1)
	assert.Equal(t, "message", plain.Changes[0].Type)

	// The saved checkpoint never covered summaries, so asking now lists them.
	stdout, _, err = executeExportSessionsCommand(newRootCommand(), "export", "conversations", "changes",
		"--tool-summaries", "--checkpoint", plain.Checkpoint)
	require.NoError(t, err)
	var summaries page
	require.NoError(t, json.Unmarshal([]byte(stdout), &summaries))
	require.Len(t, summaries.Changes, 1)
	assert.Equal(t, "tool_summary", summaries.Changes[0].Type)
	assert.Equal(t, "chat", summaries.Changes[0].SessionID)
	assert.JSONEq(t, `{
		"duration_ms": 2500,
		"categories": [
			{"category": "Bash", "calls": 1, "observed": 1, "errored": 1, "timed": 1, "duration_ms": 2500},
			{"category": "Skill", "calls": 1, "observed": 0, "errored": 0, "timed": 0, "duration_ms": 0}
		],
		"skills": [{"name": "review-change", "calls": 1}]
	}`, string(summaries.Changes[0].ToolSummary))
}
