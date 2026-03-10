package parser

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseIflowSession(t *testing.T) {
	tests := []struct {
		name            string
		filename        string
		expectID        string
		expectMessageCount int
		expectFirstMessage string
	}{
		{
			name:            "basic iFlow session",
			filename:        "testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
			expectID:        "iflow:5de701fc-7454-4858-a249-95cac4fd3b51",
			expectMessageCount: 11,
			expectFirstMessage: "启动app时确保环境变量 DOCKER_API_VERSION=\"1.46\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := ParseIflowSession(
				tt.filename,
				"test-project",
				"local",
			)

			if err != nil {
				t.Fatalf("ParseIflowSession error: %v", err)
			}

			if len(results) == 0 {
				t.Fatal("expected at least one result")
			}

			session := results[0].Session
			if session.ID != tt.expectID {
				t.Errorf("expected ID %s, got %s", tt.expectID, session.ID)
			}

			if session.Agent != AgentIflow {
				t.Errorf("expected agent %s, got %s", AgentIflow, session.Agent)
			}

			if session.Project != "test-project" {
				t.Errorf("expected project test-project, got %s", session.Project)
			}

			if session.MessageCount != tt.expectMessageCount {
				t.Errorf("expected %d messages, got %d", tt.expectMessageCount, session.MessageCount)
			}

			if len(results[0].Messages) != tt.expectMessageCount {
				t.Errorf("expected %d parsed messages, got %d", tt.expectMessageCount, len(results[0].Messages))
			}

			if session.FirstMessage != tt.expectFirstMessage {
				t.Errorf("expected first message %q, got %q", tt.expectFirstMessage, session.FirstMessage)
			}

			// Check that timestamps are parsed
			if session.StartedAt.IsZero() {
				t.Error("expected non-zero StartedAt")
			}
			if session.EndedAt.IsZero() {
				t.Error("expected non-zero EndedAt")
			}

			// Check that file info is populated
			if session.File.Path == "" {
				t.Error("expected non-empty file path")
			}
			if session.File.Size == 0 {
				t.Error("expected non-zero file size")
			}
		})
	}
}

func TestExtractIflowProjectHints(t *testing.T) {
	cwd, gitBranch := ExtractIflowProjectHints("testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl")

	// Expected values from the test file
	if cwd != "C:\\exp\\docker-image-retagger" {
		t.Errorf("expected cwd C:\\exp\\docker-image-retagger, got %s", cwd)
	}

	// gitBranch is null in this test file
	if gitBranch != "" {
		t.Errorf("expected empty gitBranch, got %s", gitBranch)
	}
}

func TestIflowSystemMessageFiltering(t *testing.T) {
	results, err := ParseIflowSession(
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	if err != nil {
		t.Fatalf("ParseIflowSession error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	messages := results[0].Messages

	// Verify that user messages have content
	for _, msg := range messages {
		if msg.Role == RoleUser {
			if msg.Content == "" && len(msg.ToolResults) == 0 {
				t.Errorf("user message at ordinal %d should have content or tool results", msg.Ordinal)
			}
		}
	}
}

func TestIflowToolCallParsing(t *testing.T) {
	results, err := ParseIflowSession(
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	if err != nil {
		t.Fatalf("ParseIflowSession error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	messages := results[0].Messages

	// After deduplication of streaming updates, the fixture
	// should retain all meaningful messages (user prompts,
	// final assistant turns, and tool results).
	hasToolUse := false
	hasToolResult := false
	for _, msg := range messages {
		if msg.Role != RoleUser && msg.Role != RoleAssistant {
			t.Errorf("unexpected role: %s", msg.Role)
		}
		if msg.Ordinal < 0 {
			t.Errorf("invalid ordinal: %d", msg.Ordinal)
		}
		if len(msg.ToolCalls) > 0 {
			hasToolUse = true
		}
		if len(msg.ToolResults) > 0 {
			hasToolResult = true
		}
	}
	if !hasToolUse {
		t.Error("expected at least one message with tool calls")
	}
	if !hasToolResult {
		t.Error("expected at least one message with tool results")
	}
}

func TestIflowBurstMerge(t *testing.T) {
	results, err := ParseIflowSession(
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	if err != nil {
		t.Fatalf("ParseIflowSession error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	messages := results[0].Messages

	// The first assistant message (ordinal 1) is a merged
	// streaming burst from lines 1-4 of the fixture. It must
	// retain the explanatory text from the first snapshot and
	// all three unique read_file tool calls.
	if len(messages) < 2 {
		t.Fatal("expected at least 2 messages")
	}

	first := messages[1]
	if first.Role != RoleAssistant {
		t.Fatalf("expected assistant at ordinal 1, got %s", first.Role)
	}
	if !strings.Contains(first.Content, "DOCKER_API_VERSION") {
		t.Error("first assistant burst lost explanatory text")
	}
	if len(first.ToolCalls) != 3 {
		t.Errorf(
			"expected 3 tool calls in first burst, got %d",
			len(first.ToolCalls),
		)
	}

	// Verify every tool_result in the session has a matching
	// tool_call somewhere, confirming no orphaned results.
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			callIDs[tc.ToolUseID] = true
		}
		for _, tr := range m.ToolResults {
			resultIDs[tr.ToolUseID] = true
		}
	}
	for id := range resultIDs {
		if !callIDs[id] {
			t.Errorf("orphaned tool_result %s has no tool_call", id)
		}
	}
}

func TestIflowBurstBoundary(t *testing.T) {
	// Two assistant snapshots with the same parentUuid and
	// sub-second timestamps, but separated by a user entry.
	// They must NOT be merged into one burst.
	base := time.Date(2026, 1, 21, 5, 56, 52, 0, time.UTC)
	mkLine := func(typ, uuid, parent, text string) string {
		return `{"type":"` + typ +
			`","uuid":"` + uuid +
			`","parentUuid":"` + parent +
			`","message":{"content":"` + text + `"}}`
	}

	entries := []dagEntryIflow{
		{
			entryType:  "assistant",
			uuid:       "a1",
			parentUuid: "root",
			lineIndex:  0,
			timestamp:  base,
			line: mkLine(
				"assistant", "a1", "root", "first snapshot",
			),
		},
		{
			entryType:  "user",
			uuid:       "u1",
			parentUuid: "a1",
			lineIndex:  1,
			timestamp:  base.Add(100 * time.Millisecond),
			line: mkLine(
				"user", "u1", "a1", "tool result",
			),
		},
		{
			entryType:  "assistant",
			uuid:       "a2",
			parentUuid: "root",
			lineIndex:  2,
			timestamp:  base.Add(200 * time.Millisecond),
			line: mkLine(
				"assistant", "a2", "root", "second snapshot",
			),
		},
	}

	result := deduplicateIflowEntries(entries)

	// All three entries must survive: the user entry between
	// the two assistant entries prevents burst merging.
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	if result[0].uuid != "a1" {
		t.Errorf("expected a1, got %s", result[0].uuid)
	}
	if result[1].uuid != "u1" {
		t.Errorf("expected u1, got %s", result[1].uuid)
	}
	if result[2].uuid != "a2" {
		t.Errorf("expected a2, got %s", result[2].uuid)
	}

	// Also test: different-parent assistant between snapshots.
	entries2 := []dagEntryIflow{
		{
			entryType:  "assistant",
			uuid:       "a1",
			parentUuid: "root",
			lineIndex:  0,
			timestamp:  base,
			line: mkLine(
				"assistant", "a1", "root", "first",
			),
		},
		{
			entryType:  "assistant",
			uuid:       "a3",
			parentUuid: "other",
			lineIndex:  1,
			timestamp:  base.Add(50 * time.Millisecond),
			line: mkLine(
				"assistant", "a3", "other", "unrelated",
			),
		},
		{
			entryType:  "assistant",
			uuid:       "a2",
			parentUuid: "root",
			lineIndex:  2,
			timestamp:  base.Add(100 * time.Millisecond),
			line: mkLine(
				"assistant", "a2", "root", "second",
			),
		},
	}

	result2 := deduplicateIflowEntries(entries2)
	if len(result2) != 3 {
		t.Fatalf(
			"expected 3 entries with interleaved parent, got %d",
			len(result2),
		)
	}

	// Third case: a non-user/assistant event (e.g. system) was
	// filtered out before deduplication runs, so entries are
	// adjacent in the slice but have a gap in lineIndex.
	entries3 := []dagEntryIflow{
		{
			entryType:  "assistant",
			uuid:       "a1",
			parentUuid: "root",
			lineIndex:  1,
			timestamp:  base,
			line: mkLine(
				"assistant", "a1", "root", "first",
			),
		},
		// lineIndex 2 was a system event, now filtered out
		{
			entryType:  "assistant",
			uuid:       "a2",
			parentUuid: "root",
			lineIndex:  3,
			timestamp:  base.Add(100 * time.Millisecond),
			line: mkLine(
				"assistant", "a2", "root", "second",
			),
		},
	}

	result3 := deduplicateIflowEntries(entries3)
	if len(result3) != 2 {
		t.Fatalf(
			"expected 2 entries with filtered-event gap, got %d",
			len(result3),
		)
	}
}

func TestIflowTimestampParsing(t *testing.T) {
	results, err := ParseIflowSession(
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	if err != nil {
		t.Fatalf("ParseIflowSession error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	session := results[0].Session

	// Verify timestamps are in reasonable range
	if !session.StartedAt.Before(time.Now()) {
		t.Error("expected StartedAt to be in the past")
	}

	if !session.EndedAt.Before(time.Now()) {
		t.Error("expected EndedAt to be in the past")
	}

	if session.StartedAt.After(session.EndedAt) {
		t.Error("expected StartedAt to be before EndedAt")
	}

	// Verify message timestamps
	for _, msg := range results[0].Messages {
		if !msg.Timestamp.IsZero() {
			if msg.Timestamp.Before(session.StartedAt) {
				t.Errorf("message timestamp before session start: %v < %v", msg.Timestamp, session.StartedAt)
			}
			if msg.Timestamp.After(session.EndedAt) {
				t.Errorf("message timestamp after session end: %v > %v", msg.Timestamp, session.EndedAt)
			}
		}
	}
}

func TestIflowSessionIDExtraction(t *testing.T) {
	tests := []struct {
		filename string
		expectID string
	}{
		{
			filename: "session-96e6d875-92eb-40b9-b193-a9ba99f0f709.jsonl",
			expectID: "96e6d875-92eb-40b9-b193-a9ba99f0f709",
		},
		{
			filename: "session-abc123-def456.jsonl",
			expectID: "abc123-def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			sessionID := filepath.Base(tt.filename)
			sessionID = strings.TrimSuffix(sessionID, ".jsonl")
			if trimmed, ok := strings.CutPrefix(sessionID, "session-"); ok {
				sessionID = trimmed
			}

			if sessionID != tt.expectID {
				t.Errorf("expected ID %s, got %s", tt.expectID, sessionID)
			}
		})
	}
}