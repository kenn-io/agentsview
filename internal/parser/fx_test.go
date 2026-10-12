package parser

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFxFixture(t *testing.T) {
	session, messages, err := parseFxSession(t.Context(), filepath.Join("testdata", "fx", "parent-session", "events.jsonl"), "local")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "fx:parent-session", session.ID)
	assert.Equal(t, "Rename Widget Helper and Print Sequence Output", session.SessionName)
	assert.Equal(t, "/tmp/fx-demo/widget", session.Cwd)
	assert.Equal(t, "widget", session.Project)
	assert.Equal(t, "local", session.Machine)
	assert.Equal(t, time.UnixMilli(1791839193807).UTC(), session.StartedAt)
	assert.Equal(t, time.UnixMilli(1791839241056).UTC(), session.EndedAt)
	assert.Equal(t, 2, session.UserMessageCount)
	assert.Equal(t, "Read widget.go, rename helper to buildWidget there and in README.md, then print the output of seq 1 3000 using the shell.", session.FirstMessage)
	require.Len(t, messages, 21)
	assert.Equal(t, len(messages), session.MessageCount)
	wantRoles := []RoleType{RoleUser, RoleAssistant, RoleUser, RoleUser, RoleUser, RoleAssistant, RoleUser, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser, RoleAssistant, RoleUser, RoleAssistant}
	// Consecutive tool calls share prose; calls after a result need an empty assistant row.
	var roles []RoleType
	var calls []ParsedToolCall
	var results []ParsedToolResult
	for i, message := range messages {
		assert.Equal(t, i, message.Ordinal)
		assert.Equal(t, len(message.Content), message.ContentLength)
		roles = append(roles, message.Role)
		calls = append(calls, message.ToolCalls...)
		results = append(results, message.ToolResults...)
		if message.Role == RoleAssistant {
			assert.Equal(t, "gpt-6.1-sol", message.Model)
			assert.Equal(t, "codex", message.ProviderID)
		}
	}
	assert.Equal(t, wantRoles, roles)
	require.Len(t, calls, 10)
	require.Len(t, results, 10)
	wantCategories := []string{"Read", "Read", "Bash", "Edit", "Edit", "Bash", "Read", "Edit", "Task", "Bash"}
	wantIDs := []string{"call-1", "call-2", "call-3", "call-4", "call-5", "call-6", "call-7", "call-8", "call-9", "call-10"}
	for i, call := range calls {
		assert.Equal(t, wantIDs[i], call.ToolUseID)
		assert.Equal(t, wantCategories[i], call.Category)
		assert.Equal(t, wantIDs[i], results[i].ToolUseID)
		status := "completed"
		if i == 2 || i == 9 {
			status = "errored"
		}
		assert.Equal(t, status, results[i].Status)
		assert.Equal(t, "tool_result", results[i].Source)
		assert.NotEmpty(t, results[i].ContentRaw)
	}
	assert.Equal(t, `{"path":"widget.go"}`, calls[0].InputJSON)
	assert.Empty(t, messages[2].Content)
	assert.Contains(t, results[0].ContentRaw, "func helper()")
	assert.Contains(t, messages[len(messages)-1].Content, "Added `func Name()")
	require.Len(t, session.UsageEvents, 1)
	assert.Equal(t, ParsedUsageEvent{
		SessionID: "fx:parent-session", Source: "session", Model: "gpt-6.1-sol", ProviderID: "codex",
		InputTokens: 29248, OutputTokens: 647, CacheReadInputTokens: 72320,
		OccurredAt: "2026-10-12T21:07:21.056Z", DedupKey: "session:fx:parent-session|codex/gpt-6.1-sol",
	}, session.UsageEvents[0])
	assert.Equal(t, 647, session.TotalOutputTokens)
	assert.Zero(t, session.PeakContextTokens)
	assert.False(t, session.HasPeakContextTokens)

	child, childMessages, err := parseFxSession(t.Context(), filepath.Join("testdata", "fx", "child-session", "events.jsonl"), "local")
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "fx:parent-session", child.ParentSessionID)
	assert.Equal(t, RelSubagent, child.RelationshipType)
	require.Len(t, childMessages, 4)
	assert.Equal(t, child.FirstMessage, child.SessionName)
	assert.Empty(t, child.UsageEvents)
}

func TestFxEventVariants(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "variants")
	filePath := filepath.Join(dir, "events.jsonl")
	writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4,"model":"fallback","provider":"gateway","created_at_ms":1000,"updated_at_ms":9000}`)
	writeSourceFile(t, filePath, `{"schema_version":3,"seq":1,"timestamp_ms":2000,"event":{"steering":{"text":"Use the skill"}}}
{"seq":2,"timestamp_ms":3000,"event":{"tool_call":{"call_id":"skill-1","tool_name":"skill","arguments_json":"{\"location\":\"/tmp/demo/skills/review\"}"}}}
{"seq":3,"timestamp_ms":4000,"event":{"tool_result":{"call_id":"skill-1","status":"success","preview":"loaded"}}}
{"seq":4,"timestamp_ms":5000,"event":{"context_checkpoint":{"summary":"Earlier work"}}}
{"seq":5,"timestamp_ms":6000,"event":{"interrupted":{"partial_text":"Half an answer"}}}
{"seq":6,"timestamp_ms":7000,"event":{"interrupted":{"partial_text":null}}}
{"seq":7,"timestamp_ms":8000,"event":{"turn_completed":{}}}
{"seq":8,"timestamp_ms":8000,"event":{"assistant":{"text":"Reply","provider_replay":{"source":{"model":"actual","provider":"codex"}}}}}
`)
	writeSourceFile(t, filepath.Join(dir, "usage-v2.json"), `{"snapshot":{"models":[{"model":"gateway/one","input_tokens":20,"output_tokens":4,"cache_read_tokens":8,"cache_write_tokens":3,"reasoning_tokens":2,"total_cost":0},{"model":"codex/two","input_tokens":1,"output_tokens":6,"cache_read_tokens":5}]}}`)
	session, messages, err := parseFxSession(t.Context(), filePath, "local")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Zero(t, session.MalformedLines)
	assert.Equal(t, 1, session.UserMessageCount)
	assert.Equal(t, "Use the skill", session.SessionName)
	assert.Equal(t, time.UnixMilli(9000).UTC(), session.EndedAt)
	require.Len(t, messages, 6)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, "Use the skill", messages[0].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "review", messages[1].ToolCalls[0].SkillName)
	assert.Equal(t, "Tool", messages[1].ToolCalls[0].Category)
	assert.Equal(t, "fallback", messages[1].Model)
	assert.True(t, messages[3].IsCompactBoundary)
	assert.True(t, messages[3].IsSystem)
	assert.Equal(t, "compact_boundary", messages[3].SourceSubtype)
	assert.Equal(t, "Earlier work", messages[3].Content)
	assert.Equal(t, "Half an answer", messages[4].Content)
	assert.Equal(t, "fallback", messages[4].Model)
	assert.Equal(t, "actual", messages[5].Model)
	assert.Equal(t, "codex", messages[5].ProviderID)
	require.Len(t, session.UsageEvents, 2)
	assert.Equal(t, 9, session.UsageEvents[0].InputTokens)
	assert.Equal(t, 3, session.UsageEvents[0].CacheCreationInputTokens)
	assert.Equal(t, 2, session.UsageEvents[0].ReasoningTokens)
	assert.Nil(t, session.UsageEvents[0].Cost)
	assert.Equal(t, 0, session.UsageEvents[1].InputTokens)
	assert.Equal(t, "session:fx:variants|codex/two", session.UsageEvents[1].DedupKey)
	assert.Equal(t, 10, session.TotalOutputTokens)
	assert.Equal(t, "1970-01-01T00:00:08Z", session.UsageEvents[0].OccurredAt)
}

func TestFxMetadataAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name      string
		metadata  string
		wantError bool
	}{
		{"missing", "", false},
		{"inline schema", `{"schema_version":2}`, false},
		{"old event log", `{"schema_version":3}`, false},
		{"future schema", `{"schema_version":5}`, false},
		{"malformed", `{broken`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.metadata != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "session.json"), []byte(test.metadata), 0o600))
			}
			session, _, err := parseFxSession(t.Context(), filepath.Join(dir, "events.jsonl"), "local")
			if test.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Nil(t, session)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := parseFxSession(ctx, "unused", "local")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFxToolCallsKeepAssistantModel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model-change")
	writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4,"model":"current-model","provider":"current-provider"}`)
	filePath := filepath.Join(dir, "events.jsonl")
	writeSourceFile(t, filePath, `{"event":{"assistant":{"text":"Earlier reply","provider_replay":{"source":{"model":"earlier-model","provider":"earlier-provider"}}}}}
{"event":{"tool_call":{"call_id":"one","tool_name":"read_file","arguments_json":"{}"}}}
{"event":{"tool_result":{"call_id":"one","status":"success","preview":"read"}}}
{"event":{"tool_call":{"call_id":"two","tool_name":"shell","arguments_json":"{}"}}}
{"event":{"tool_result":{"call_id":"two","status":"success","preview":"done"}}}
{"event":{"assistant":{"text":"Later reply","provider_replay":{"source":{"model":"later-model","provider":"later-provider"}}}}}
{"event":{"tool_call":{"call_id":"three","tool_name":"read_file","arguments_json":"{}"}}}
{"event":{"tool_result":{"call_id":"three","status":"success","preview":"read"}}}
{"event":{"tool_call":{"call_id":"four","tool_name":"shell","arguments_json":"{}"}}}
{"event":{"user":{"text":"New turn"}}}
{"event":{"tool_call":{"call_id":"five","tool_name":"shell","arguments_json":"{}"}}}
{"event":{"steering":{"text":"Another turn"}}}
{"event":{"tool_call":{"call_id":"six","tool_name":"shell","arguments_json":"{}"}}}
`)
	_, messages, err := parseFxSession(t.Context(), filePath, "local")
	require.NoError(t, err)
	require.Len(t, messages, 11)
	for _, row := range []int{0, 2} {
		assert.Equal(t, "earlier-model", messages[row].Model)
		assert.Equal(t, "earlier-provider", messages[row].ProviderID)
	}
	for _, row := range []int{4, 6} {
		assert.Equal(t, "later-model", messages[row].Model)
		assert.Equal(t, "later-provider", messages[row].ProviderID)
	}
	require.Len(t, messages[2].ToolCalls, 1)
	assert.Equal(t, "two", messages[2].ToolCalls[0].ToolUseID)
	require.Len(t, messages[6].ToolCalls, 1)
	assert.Equal(t, "four", messages[6].ToolCalls[0].ToolUseID)
	for _, row := range []int{8, 10} {
		assert.Equal(t, "current-model", messages[row].Model)
		assert.Equal(t, "current-provider", messages[row].ProviderID)
	}
}

func TestFxToolResultWithoutPreview(t *testing.T) {
	for _, test := range []struct{ name, result, raw string }{
		{"absent", `{"call_id":"read-1","status":"success"}`, ""},
		{"null", `{"call_id":"read-1","status":"success","preview":null}`, "null"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4}`)
			filePath := filepath.Join(dir, "events.jsonl")
			writeSourceFile(t, filePath, `{"event":{"tool_result":`+test.result+`}}`)
			_, messages, err := parseFxSession(t.Context(), filePath, "local")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, RoleUser, messages[0].Role)
			assert.Empty(t, messages[0].Content)
			require.Len(t, messages[0].ToolResults, 1)
			assert.Equal(t, ParsedToolResult{ToolUseID: "read-1", Source: "tool_result", Status: "completed", ContentRaw: test.raw}, messages[0].ToolResults[0])
		})
	}
}

func TestFxFutureArtifactVersions(t *testing.T) {
	for _, test := range []struct{ name, path, content, reason string }{
		{"events", "events.jsonl", `{"schema_version":3,"event":{"user":{"text":"Supported"}}}
{"schema_version":4,"event":{"assistant":{"text":"Future"}}}`, "events.jsonl frame schema_version 4"},
		{"usage", "usage-v2.json", `{"schema_version":2,"snapshot":{"schema_version":3,"models":[{"model":"test","input_tokens":5}]}}`, "usage-v2.json schema_version 2"},
		{"snapshot", "usage-v2.json", `{"schema_version":1,"snapshot":{"schema_version":4,"models":[{"model":"test","input_tokens":5}]}}`, "usage-v2.json snapshot schema_version 4"},
		{"owner", "subagent/owner.json", `{"schema_version":2,"parent_id":"parent-a"}`, "owner.json schema_version 2"},
		{"older session", "session.json", `{"schema_version":3}`, "session.json schema_version 3 is older than supported 4"},
		{"newer session", "session.json", `{"schema_version":5}`, "session.json schema_version 5 is newer than supported 4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4}`)
			filePath := filepath.Join(dir, "events.jsonl")
			writeSourceFile(t, filePath, `{"schema_version":3,"event":{"user":{"text":"Hello"}}}`)
			writeSourceFile(t, filepath.Join(dir, filepath.FromSlash(test.path)), test.content)
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			session, messages, err := parseFxSession(t.Context(), filePath, "local")
			require.NoError(t, err)
			assert.Nil(t, session)
			assert.Empty(t, messages)
			assert.Contains(t, logs.String(), test.reason)
		})
	}
}

func TestFxMalformedOwner(t *testing.T) {
	dir := t.TempDir()
	writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4}`)
	writeSourceFile(t, filepath.Join(dir, "events.jsonl"), `{"event":{"user":{"text":"Hello"}}}`)
	writeSourceFile(t, filepath.Join(dir, "subagent", "owner.json"), `{broken`)
	session, messages, err := parseFxSession(t.Context(), filepath.Join(dir, "events.jsonl"), "local")
	require.ErrorContains(t, err, "invalid fx metadata")
	assert.Nil(t, session)
	assert.Empty(t, messages)
}

func TestFxSkillNames(t *testing.T) {
	for _, test := range []struct{ name, arguments, want string }{
		{"explicit", `{"name":"review","location":"/tmp/skills/other"}`, "review"},
		{"name only", `{"name":"review"}`, "review"},
		{"location", `{"location":"/tmp/skills/review"}`, "review"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4}`)
			writeSourceFile(t, filepath.Join(dir, "events.jsonl"), fmt.Sprintf(`{"event":{"tool_call":{"call_id":"skill-1","tool_name":"skill","arguments_json":%q}}}`, test.arguments))
			_, messages, err := parseFxSession(t.Context(), filepath.Join(dir, "events.jsonl"), "local")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, test.want, messages[0].ToolCalls[0].SkillName)
		})
	}
}

func TestFxInvalidFrames(t *testing.T) {
	for _, frame := range []string{`{broken`, `null`, `{}`, `{"event":null}`, `{"event":[]}`} {
		t.Run(frame, func(t *testing.T) {
			dir := t.TempDir()
			writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4}`)
			writeSourceFile(t, filepath.Join(dir, "events.jsonl"), "{\"event\":{\"user\":{\"text\":\"Hello\"}}}\n"+frame)
			session, messages, err := parseFxSession(t.Context(), filepath.Join(dir, "events.jsonl"), "local")
			require.ErrorContains(t, err, "invalid fx events.jsonl")
			assert.Nil(t, session)
			assert.Empty(t, messages)
		})
	}
}

func TestFxUsageStructureAndSessionIdentity(t *testing.T) {
	for _, test := range []struct{ name, usage, wantError string }{
		{"absent", "", ""},
		{"empty models", `{"snapshot":{"models":[]}}`, ""},
		{"matching session", `{"session_id":"session-a","snapshot":{"models":[]}}`, ""},
		{"other session", `{"session_id":"session-b","snapshot":{"models":[]}}`, "session_id does not match"},
		{"malformed", `{broken`, "invalid fx metadata"},
		{"null envelope", `null`, "snapshot object and models array"},
		{"array envelope", `[]`, "snapshot object and models array"},
		{"missing snapshot", `{}`, "snapshot object and models array"},
		{"null snapshot", `{"snapshot":null}`, "snapshot object and models array"},
		{"array snapshot", `{"snapshot":[]}`, "snapshot object and models array"},
		{"missing models", `{"snapshot":{}}`, "snapshot object and models array"},
		{"null models", `{"snapshot":{"models":null}}`, "snapshot object and models array"},
		{"object models", `{"snapshot":{"models":{}}}`, "snapshot object and models array"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "session-a")
			writeSourceFile(t, filepath.Join(dir, "session.json"), `{"schema_version":4}`)
			writeSourceFile(t, filepath.Join(dir, "events.jsonl"), `{"event":{"user":{"text":"Hello"}}}`)
			if test.usage != "" {
				writeSourceFile(t, filepath.Join(dir, "usage-v2.json"), test.usage)
			}
			session, messages, err := parseFxSession(t.Context(), filepath.Join(dir, "events.jsonl"), "local")
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				assert.Nil(t, session)
				assert.Empty(t, messages)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Empty(t, session.UsageEvents)
		})
	}
}
