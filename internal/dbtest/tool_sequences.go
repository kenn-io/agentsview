package dbtest

import (
	"fmt"
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

// SeedToolSequencesParity adds retained-evidence, incomplete and long streamed
// cases to the basic recovery example used by the backend parity tests.
func SeedToolSequencesParity(t *testing.T, d *db.DB) []string {
	t.Helper()
	const exampleID = "tool-sequences-parity"
	const evidenceID = "tool-sequences-parity-evidence"
	const incompleteID = "tool-sequences-parity-incomplete"
	SeedToolSequencesExample(t, d, exampleID)

	evidenceMessages := []db.Message{
		{SessionID: evidenceID, Ordinal: 0, Role: "user", Content: "Check the config", ContentLength: 16, Timestamp: "2026-04-26T10:00:00Z"},
		toolSequenceParityMessage(evidenceID, 1, toolSequenceParityCall("Grep", "empty", "", 0, "completed")),
		toolSequenceParityMessage(evidenceID, 2, toolSequenceParityCall("Read", "summary", "single-event summary", 20, "completed")),
		toolSequenceParityMessage(evidenceID, 3, toolSequenceParityCall("Bash", "error", "command failed", 14, "errored")),
		toolSequenceParityMessage(evidenceID, 4, toolSequenceParityCall("Read", "image", "[image]", len("[image]"), "completed")),
		toolSequenceParityMessage(evidenceID, 5, db.ToolCall{
			ToolName: "Read", Category: "Read", ToolUseID: "withheld",
			ResultContentLength: 4096,
		}),
		toolSequenceParityMessage(evidenceID, 6, toolSequenceParityCall("Read", "recovery", "config loaded", 13, "completed")),
	}
	seedToolSequenceParitySession(t, d, evidenceID, "clean", evidenceMessages)

	incompleteMessages := []db.Message{
		{SessionID: incompleteID, Ordinal: 0, Role: "user", Content: "Run the command", ContentLength: len("Run the command"), Timestamp: "2026-04-26T10:00:00Z"},
		toolSequenceParityMessage(incompleteID, 1, toolSequenceParityCall("Bash", "", "command failed", 14, "errored")),
	}
	seedToolSequenceParitySession(t, d, incompleteID, "tool_call_pending", incompleteMessages)

	const streamedID = "tool-sequences-parity-streamed"
	streamed := []db.Message{{SessionID: streamedID, Ordinal: 0, Role: "user", Content: "search", ContentLength: 6}}
	for i := 1; i <= 130; i++ {
		call := toolSequenceParityCall("Grep", "reused", "No matches found", 16, "completed")
		call.InputJSON = `{"pattern":"config"}`
		if i == 130 {
			call = toolSequenceParityCall("Read", "recovery", "recovered", 9, "completed")
		}
		call.ResultEvents = []db.ToolResultEvent{
			{ToolUseID: call.ToolUseID, Source: "tool_execution", Status: "started", Timestamp: "2026-04-26T10:00:00Z", EventIndex: 0},
			{ToolUseID: call.ToolUseID, Source: "tool_execution", Status: "completed", Timestamp: "2026-04-26T10:00:02Z", Content: call.ResultContent, ContentLength: len(call.ResultContent), EventIndex: 1},
		}
		message := toolSequenceParityMessage(streamedID, i*2, call)
		if i == 1 {
			for j := 1; j < 25; j++ {
				sibling := call
				sibling.ToolUseID = fmt.Sprint("parallel-", j)
				if j == 20 {
					sibling.ToolUseID = "reused"
				}
				message.ToolCalls = append(message.ToolCalls, sibling)
			}
		}
		streamed = append(streamed, message)
	}
	seedToolSequenceParitySession(t, d, streamedID, "clean", streamed)

	return []string{exampleID, evidenceID, incompleteID, streamedID}
}

func seedToolSequenceParitySession(
	t *testing.T, d *db.DB, sessionID, termination string, messages []db.Message,
) {
	t.Helper()
	startedAt := "2026-04-26T10:00:00Z"
	endedAt := "2026-04-26T10:00:10Z"
	SeedSession(t, d, sessionID, "tool-sequences-parity", func(s *db.Session) {
		s.MessageCount = len(messages)
		s.UserMessageCount = 1
		s.StartedAt = &startedAt
		s.TerminationStatus = Ptr(termination)
		if termination == "clean" {
			s.EndedAt = &endedAt
		}
	})
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, messages))
}

func toolSequenceParityMessage(sessionID string, ordinal int, call db.ToolCall) db.Message {
	return db.Message{
		SessionID: sessionID, Ordinal: ordinal, Role: "assistant", Content: "tool call",
		ContentLength: len("tool call"), Timestamp: "2026-04-26T10:00:01Z",
		HasToolUse: true, ToolCalls: []db.ToolCall{call},
	}
}

func toolSequenceParityCall(tool, id, content string, length int, status string) db.ToolCall {
	call := db.ToolCall{
		ToolName: tool, Category: tool, ToolUseID: id,
		ResultContent: content, ResultContentLength: length,
	}
	if status != "" {
		call.ResultEvents = []db.ToolResultEvent{{
			ToolUseID: id, Source: "tool_execution", Status: status,
			Content: content, ContentLength: length, EventIndex: 0,
		}}
	}
	return call
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
