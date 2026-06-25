package parser

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseIflowSessionForTest exercises the iFlow provider's transcript parsing
// in isolation, passing the project directly so tests can assert parse
// behavior without project-resolution enrichment.
func parseIflowSessionForTest(
	t *testing.T, path, project, machine string,
) ([]ParseResult, error) {
	t.Helper()
	return parseIflowSession(path, project, machine)
}

func TestParseIflowSession(t *testing.T) {
	tests := []struct {
		name               string
		filename           string
		expectID           string
		expectMessageCount int
		expectFirstMessage string
	}{
		{
			name:               "basic iFlow session",
			filename:           "testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
			expectID:           "iflow:5de701fc-7454-4858-a249-95cac4fd3b51",
			expectMessageCount: 11,
			expectFirstMessage: "启动app时确保环境变量 DOCKER_API_VERSION=\"1.46\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := parseIflowSessionForTest(
				t,
				tt.filename,
				"test-project",
				"local",
			)

			require.NoError(t, err, "ParseIflowSession error")
			require.NotEmpty(t, results, "expected at least one result")

			session := results[0].Session
			assert.Equal(t, tt.expectID, session.ID, "ID")
			assert.Equal(t, AgentIflow, session.Agent, "agent")
			assert.Equal(t, "test-project", session.Project, "project")
			assert.Equal(t, tt.expectMessageCount, session.MessageCount, "message count")
			assert.Len(t, results[0].Messages, tt.expectMessageCount, "parsed messages")
			assert.Equal(t, tt.expectFirstMessage, session.FirstMessage, "first message")

			// Check that timestamps are parsed
			assert.False(t, session.StartedAt.IsZero(), "expected non-zero StartedAt")
			assert.False(t, session.EndedAt.IsZero(), "expected non-zero EndedAt")

			// Check that file info is populated
			assert.NotEmpty(t, session.File.Path, "expected non-empty file path")
			assert.NotZero(t, session.File.Size, "expected non-zero file size")
		})
	}
}

func TestExtractIflowProjectHints(t *testing.T) {
	cwd, gitBranch := ExtractIflowProjectHints("testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl")

	// Expected values from the test file
	assert.Equal(t, "C:\\exp\\docker-image-retagger", cwd, "cwd")

	// gitBranch is null in this test file
	assert.Empty(t, gitBranch, "gitBranch")
}

func TestIflowSystemMessageFiltering(t *testing.T) {
	results, err := parseIflowSessionForTest(
		t,
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	require.NoError(t, err, "ParseIflowSession error")
	require.NotEmpty(t, results, "expected at least one result")

	messages := results[0].Messages

	// Verify that user messages have content
	for _, msg := range messages {
		if msg.Role == RoleUser {
			hasContent := msg.Content != "" || len(msg.ToolResults) > 0
			assert.True(t, hasContent,
				"user message at ordinal %d should have content or tool results", msg.Ordinal)
		}
	}
}

func TestIflowToolCallParsing(t *testing.T) {
	results, err := parseIflowSessionForTest(
		t,
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	require.NoError(t, err, "ParseIflowSession error")
	require.NotEmpty(t, results, "expected at least one result")

	messages := results[0].Messages

	// After deduplication of streaming updates, the fixture
	// should retain all meaningful messages (user prompts,
	// final assistant turns, and tool results).
	hasToolUse := false
	hasToolResult := false
	for _, msg := range messages {
		assert.Contains(t, []RoleType{RoleUser, RoleAssistant}, msg.Role, "unexpected role")
		assert.GreaterOrEqual(t, msg.Ordinal, 0, "invalid ordinal")
		if len(msg.ToolCalls) > 0 {
			hasToolUse = true
		}
		if len(msg.ToolResults) > 0 {
			hasToolResult = true
		}
	}
	assert.True(t, hasToolUse, "expected at least one message with tool calls")
	assert.True(t, hasToolResult, "expected at least one message with tool results")
}

func TestIflowBurstMerge(t *testing.T) {
	results, err := parseIflowSessionForTest(
		t,
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	require.NoError(t, err, "ParseIflowSession error")
	require.NotEmpty(t, results, "expected at least one result")

	messages := results[0].Messages

	// The first assistant message (ordinal 1) is a merged
	// streaming burst from lines 1-4 of the fixture. It must
	// retain the explanatory text from the first snapshot and
	// all three unique read_file tool calls.
	require.GreaterOrEqual(t, len(messages), 2, "expected at least 2 messages")

	first := messages[1]
	require.Equal(t, RoleAssistant, first.Role, "expected assistant at ordinal 1")
	assert.Contains(t, first.Content, "DOCKER_API_VERSION", "first assistant burst lost explanatory text")
	assert.Len(t, first.ToolCalls, 3, "expected 3 tool calls in first burst")

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
		assert.Truef(t, callIDs[id], "orphaned tool_result %s has no tool_call", id)
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
	require.Len(t, result, 3, "expected 3 entries")
	assert.Equal(t, "a1", result[0].uuid)
	assert.Equal(t, "u1", result[1].uuid)
	assert.Equal(t, "a2", result[2].uuid)

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
	require.Len(t, result2, 3, "expected 3 entries with interleaved parent")

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
	require.Len(t, result3, 2, "expected 2 entries with filtered-event gap")
}

func TestIflowTimestampParsing(t *testing.T) {
	results, err := parseIflowSessionForTest(
		t,
		"testdata/iflow/session-5de701fc-7454-4858-a249-95cac4fd3b51.jsonl",
		"test-project",
		"local",
	)

	require.NoError(t, err, "ParseIflowSession error")
	require.NotEmpty(t, results, "expected at least one result")

	session := results[0].Session

	// Verify timestamps are in reasonable range
	assert.True(t, session.StartedAt.Before(time.Now()), "expected StartedAt to be in the past")
	assert.True(t, session.EndedAt.Before(time.Now()), "expected EndedAt to be in the past")
	assert.False(t, session.StartedAt.After(session.EndedAt), "expected StartedAt to be before EndedAt")

	// Verify message timestamps
	for _, msg := range results[0].Messages {
		if !msg.Timestamp.IsZero() {
			assert.Falsef(t, msg.Timestamp.Before(session.StartedAt),
				"message timestamp before session start: %v < %v", msg.Timestamp, session.StartedAt)
			assert.Falsef(t, msg.Timestamp.After(session.EndedAt),
				"message timestamp after session end: %v > %v", msg.Timestamp, session.EndedAt)
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

			assert.Equal(t, tt.expectID, sessionID, "ID")
		})
	}
}
