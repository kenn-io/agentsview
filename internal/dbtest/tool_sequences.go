package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// SeedToolSequencesExample writes the recovered Grep-to-Read sequence used by
// the HTTP and backend parity tests.
func SeedToolSequencesExample(
	t *testing.T, d *db.DB, sessionID string, options ...func(*db.Session),
) {
	t.Helper()
	seedOptions := []func(*db.Session){func(s *db.Session) {
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.StartedAt = Ptr("2026-04-26T10:00:00Z")
		s.EndedAt = Ptr("2026-04-26T10:00:08Z")
		s.TerminationStatus = Ptr("clean")
	}}
	seedOptions = append(seedOptions, options...)
	SeedSession(t, d, sessionID, "tool-sequences-test", seedOptions...)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, ToolSequencesExampleMessages(sessionID)))
}

// ToolSequencesExampleMessages returns the transcript rows for the recovered
// Grep-to-Read example without inserting them.
func ToolSequencesExampleMessages(sessionID string) []db.Message {
	return []db.Message{
		{
			SessionID: sessionID, Ordinal: 0, Role: "user", Content: "Find the config",
			ContentLength: len("Find the config"), Timestamp: "2026-04-26T10:00:00Z",
		},
		toolSequenceAssistantMessage(sessionID, 1, "Grep", "grep-1", `{"pattern":"config"}`, "No matches found", "2026-04-26T10:00:01Z", "2026-04-26T10:00:03Z"),
		toolSequenceAssistantMessage(sessionID, 2, "Grep", "grep-2", `{"pattern":"config"}`, "No matches found", "", ""),
		toolSequenceAssistantMessage(sessionID, 3, "Read", "read-1", `{"file_path":"app/config.json"}`, "{\"enabled\":true}", "2026-04-26T10:00:05Z", "2026-04-26T10:00:07Z"),
	}
}

func toolSequenceAssistantMessage(
	sessionID string,
	ordinal int,
	toolName string,
	toolUseID string,
	input string,
	result string,
	startedAt string,
	completedAt string,
) db.Message {
	call := db.ToolCall{
		ToolName: toolName, Category: toolName, ToolUseID: toolUseID,
		InputJSON: input, ResultContent: result, ResultContentLength: len(result),
	}
	if startedAt != "" && completedAt != "" {
		call.ResultEvents = []db.ToolResultEvent{
			{ToolUseID: toolUseID, Source: "tool_execution", Status: "started", Timestamp: startedAt, EventIndex: 0},
			{ToolUseID: toolUseID, Source: "tool_execution", Status: "completed", Timestamp: completedAt, Content: result, ContentLength: len(result), EventIndex: 1},
		}
	}
	return db.Message{
		SessionID: sessionID, Ordinal: ordinal, Role: "assistant", Content: "tool call",
		ContentLength: len("tool call"), Timestamp: "2026-04-26T10:00:01Z",
		HasToolUse: true, ToolCalls: []db.ToolCall{call},
	}
}
