package parser

import (
	"context"
	"encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeEvenerFixture(t *testing.T, dir, id string, header map[string]any, turns ...map[string]any) string {
	t.Helper()
	h := map[string]any{"kind": "header", "format_version": 2, "session_id": id, "created_at": "2026-01-01T00:00:00Z", "model": "model-a", "profile_id": "provider-a", "working_dir": dir}
	maps.Copy(h, header)
	records := []any{h}
	for i, turn := range turns {
		records = append(records, map[string]any{"kind": "entry", "seq": i, "turn": turn})
	}
	var data []byte
	for _, record := range records {
		b, err := json.Marshal(record)
		require.NoError(t, err)
		data = append(data, b...)
		data = append(data, '\n')
	}
	path := filepath.Join(dir, id+".transcript.jsonl")
	require.NoError(t, os.WriteFile(path, data, 0600))
	return path
}

func evenerTestTurn(kind, text string) map[string]any {
	return map[string]any{"kind": kind, "timestamp": "2026-01-01T00:01:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"kind": "text", "text": text}}}}
}

func writeEvenerMeta(t *testing.T, path string, meta map[string]any) {
	t.Helper()
	b, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path[:len(path)-len(".transcript.jsonl")]+".meta.json", b, 0600))
}

func TestEvenerSemanticSession(t *testing.T) {
	dir := t.TempDir()
	answer := evenerTestTurn("ASSISTANT", "Hello")
	answer["response_model"] = "response-model"
	answer["response_provider"] = "response-provider"
	answer["usage"] = map[string]any{"input_tokens": 100, "output_tokens": 20, "cache_read_tokens": 30, "cache_write_tokens": 40, "cache_write_1h_tokens": 50, "reasoning_tokens": 10}
	path := writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "Question"), answer)
	writeEvenerMeta(t, path, map[string]any{"id": "session", "name": "Title", "model": "current-model"})
	sess, msgs, err := parseEvenerSession(context.Background(), path, "test")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Equal(t, "evener:session", sess.ID)
	assert.Equal(t, AgentType("evener"), sess.Agent)
	assert.Equal(t, "Title", sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	assert.Equal(t, dir, sess.Cwd)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "Question", sess.FirstMessage)
	assert.Equal(t, 20, sess.TotalOutputTokens)
	assert.Equal(t, 220, sess.PeakContextTokens)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), sess.StartedAt)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "response-model", msgs[1].Model)
	assert.Equal(t, "response-provider", msgs[1].ProviderID)
	assert.JSONEq(t, `{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":90,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":50},"reasoning_tokens":10}`, string(msgs[1].TokenUsage))
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestEvenerKindsAndContent(t *testing.T) {
	for _, tc := range []struct {
		kind, source string
		role         RoleType
	}{
		{"USER_INPUT", "", RoleUser}, {"STEERING", "user", RoleUser}, {"STEERING", "", RoleSystem}, {"STEERING", "daemon", RoleSystem},
		{"ASSISTANT", "", RoleAssistant}, {"TOOL", "", RoleTool}, {"TOOL_RESULTS", "", RoleTool}, {"SYSTEM", "", RoleSystem},
		{"CHECKPOINT", "", RoleSystem}, {"SUMMARY", "", RoleSystem}, {"MODEL_SWITCH", "", RoleSystem}, {"TURN_FAILURE", "", RoleSystem},
		{"HOOK_COMPLETED", "", RoleSystem}, {"ENVIRONMENT", "", RoleSystem}, {"ATTENTION_RESOLUTION", "", RoleSystem}, {"FUTURE_KIND", "", RoleSystem},
	} {
		t.Run(tc.kind+tc.source, func(t *testing.T) {
			turn := evenerTestTurn(tc.kind, "visible")
			turn["steering_source"] = tc.source
			path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
			_, msgs, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			assert.Equal(t, tc.role, msgs[0].Role)
			assert.Equal(t, tc.role == RoleSystem, msgs[0].IsSystem)
			assert.Contains(t, msgs[0].Content, "visible")
		})
	}
	t.Run("thinking and media", func(t *testing.T) {
		turn := evenerTestTurn("ASSISTANT", "")
		turn["message"] = map[string]any{"content": []any{
			map[string]any{"kind": "text", "text": "answer"}, map[string]any{"kind": "thinking", "thinking": map[string]any{"text": "reasoning"}},
			map[string]any{"kind": "redacted_thinking", "thinking": map[string]any{"redacted": true}},
			map[string]any{"kind": "image", "image": map[string]any{"data": "c2VjcmV0", "media_type": "image/png"}},
			map[string]any{"kind": "audio", "audio": map[string]any{"url": "https://example.invalid/audio"}},
			map[string]any{"kind": "document", "document": map[string]any{"file_name": "notes.pdf"}},
			map[string]any{"kind": "web_search", "web_search": map[string]any{"query": "reference"}},
			map[string]any{"kind": "future_content", "text": "future detail"},
		}}
		path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
		_, msgs, err := parseEvenerSession(context.Background(), path, "test")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.True(t, msgs[0].HasThinking)
		assert.Contains(t, msgs[0].ThinkingText, "reasoning")
		assert.Contains(t, msgs[0].ThinkingText, "redacted")
		for _, want := range []string{"answer", "image", "audio", "notes.pdf", "reference", "future detail"} {
			assert.Contains(t, msgs[0].Content, want)
		}
		assert.Contains(t, msgs[0].Content, "[Thinking]\nreasoning\n[/Thinking]")
		assert.NotContains(t, msgs[0].Content, "c2VjcmV0")
	})
	t.Run("diagnostics", func(t *testing.T) {
		failure := evenerTestTurn("TURN_FAILURE", "")
		failure["error"] = map[string]any{"message": "failed", "hint": "retry", "title": "Error"}
		hook := evenerTestTurn("HOOK_COMPLETED", "")
		hook["hook"] = map[string]any{"event": "stop", "exit_code": 0, "plugin_name": "example"}
		unknown := evenerTestTurn("FUTURE_KIND", "")
		unknown["detail"] = "retained"
		path := writeEvenerFixture(t, t.TempDir(), "session", nil, failure, hook, unknown)
		_, msgs, err := parseEvenerSession(context.Background(), path, "test")
		require.NoError(t, err)
		require.Len(t, msgs, 3)
		assert.Contains(t, msgs[0].Content, "failed")
		assert.Contains(t, msgs[0].Content, "retry")
		assert.Contains(t, msgs[1].Content, "stop")
		assert.Contains(t, msgs[1].Content, "0")
		assert.Contains(t, msgs[2].Content, "FUTURE_KIND")
		assert.Contains(t, msgs[2].Content, "retained")
	})
}

func TestEvenerTools(t *testing.T) {
	call := evenerTestTurn("ASSISTANT", "")
	call["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_call", "tool_call": map[string]any{"id": "call-1", "name": "shell", "arguments": map[string]any{"command": "pwd"}}}}}
	result := evenerTestTurn("TOOL_RESULTS", "")
	result["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_result", "tool_result": map[string]any{"tool_call_id": "call-1", "name": "shell", "content": map[string]any{"exit_code": 1, "output": "failed"}, "is_error": true, "image_data": "c2VjcmV0"}}}}
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, call, result, result)
	_, msgs, err := parseEvenerSession(context.Background(), path, "test")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.True(t, msgs[0].HasToolUse)
	tc := msgs[0].ToolCalls[0]
	assert.Equal(t, "call-1", tc.ToolUseID)
	assert.Equal(t, "shell", tc.ToolName)
	assert.JSONEq(t, `{"command":"pwd"}`, tc.InputJSON)
	require.Len(t, tc.ResultEvents, 2)
	assert.Equal(t, "error", tc.ResultEvents[0].Status)
	require.Len(t, msgs[1].ToolResults, 1)
	assert.Contains(t, DecodeContent(msgs[1].ToolResults[0].ContentRaw), "failed")
	assert.Contains(t, msgs[1].Content, "image")
	assert.NotContains(t, msgs[1].Content, "c2VjcmV0")
}

func TestEvenerModelTimelineAndUsagePresence(t *testing.T) {
	response := func() map[string]any {
		v := evenerTestTurn("ASSISTANT", "answer")
		v["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return v
	}
	switchTo := func(provider, model string) map[string]any {
		v := evenerTestTurn("MODEL_SWITCH", "do not parse this prose")
		v["model_switch"] = map[string]any{"new_provider": provider, "new_model": model}
		return v
	}
	turns := []map[string]any{response(), switchTo("provider-a", "model-b"), response(), switchTo("provider-b", "model-c"), response(), evenerTestTurn("MODEL_SWITCH", "Switched to fake/model"), response()}
	override := response()
	override["response_model"] = "actual-model"
	override["response_provider"] = "actual-provider"
	turns = append(turns, override, evenerTestTurn("ASSISTANT", "no usage"))
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, turns...)
	writeEvenerMeta(t, path, map[string]any{"id": "session", "model": "latest-only", "profile_id": "latest-provider"})
	sess, msgs, err := parseEvenerSession(context.Background(), path, "test")
	require.NoError(t, err)
	require.Len(t, msgs, 9)
	for i, want := range map[int]string{0: "model-a", 2: "model-b", 4: "model-c", 6: "", 7: "actual-model", 8: ""} {
		assert.Equal(t, want, msgs[i].Model)
	}
	assert.Equal(t, "provider-b", msgs[4].ProviderID)
	assert.Equal(t, "", msgs[6].ProviderID)
	assert.NotEmpty(t, msgs[0].TokenUsage)
	assert.True(t, msgs[0].HasOutputTokens)
	assert.True(t, msgs[0].HasContextTokens)
	assert.Empty(t, msgs[8].TokenUsage)
	assert.False(t, msgs[8].HasOutputTokens)
	assert.True(t, sess.HasTotalOutputTokens)
}

func TestEvenerValidationAndFraming(t *testing.T) {
	for _, tc := range []struct {
		name, tail         string
		truncated, wantErr bool
	}{
		{"partial json", `{"kind":`, true, false}, {"valid unterminated", `{"kind":"entry","turn":{"kind":"USER_INPUT"}}`, true, false},
		{"complete malformed", "{bad}\n", false, true}, {"invalid record kind", "{\"kind\":\"response\"}\n", false, true},
		{"missing turn", "{\"kind\":\"entry\"}\n", false, true},
		{"invalid sequence", "{\"kind\":\"entry\",\"seq\":\"bad\",\"turn\":{\"kind\":\"USER_INPUT\"}}\n", false, true}, {"clean eof", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEvenerFixture(t, t.TempDir(), "session", nil, evenerTestTurn("USER_INPUT", "kept"))
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			require.NoError(t, err)
			_, err = f.WriteString(tc.tail)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			sess, msgs, err := parseEvenerSession(context.Background(), path, "test")
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, sess)
				return
			}
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			assert.Equal(t, tc.truncated, sess.IsTruncated)
		})
	}
	for _, header := range []map[string]any{{"format_version": 1}, {"session_id": "other"}, {"kind": "other"}} {
		t.Run("header", func(t *testing.T) {
			path := writeEvenerFixture(t, t.TempDir(), "session", header)
			sess, _, err := parseEvenerSession(context.Background(), path, "test")
			require.Error(t, err)
			assert.Nil(t, sess)
		})
	}
	for _, meta := range []string{`{"id":"other"}`, `{broken}`, `null`} {
		t.Run("metadata", func(t *testing.T) {
			dir := t.TempDir()
			path := writeEvenerFixture(t, dir, "session", nil)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "session.meta.json"), []byte(meta), 0600))
			sess, _, err := parseEvenerSession(context.Background(), path, "test")
			require.Error(t, err)
			assert.Nil(t, sess)
		})
	}
	t.Run("rename and clear", func(t *testing.T) {
		path := writeEvenerFixture(t, t.TempDir(), "session", nil)
		for _, name := range []string{"First", "Second", ""} {
			writeEvenerMeta(t, path, map[string]any{"id": "session", "name": name})
			sess, _, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			assert.Equal(t, name, sess.SessionName)
			assert.True(t, sess.SessionNamePresent)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := parseEvenerSession(ctx, "unused", "test")
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestEvenerVerifiedForkPrefix(t *testing.T) {
	for _, variant := range []string{"verified", "missing parent", "different timestamp", "different kind", "nested", "subagent", "symlink parent", "invalid parent metadata", "mismatched parent metadata", "symlink parent metadata"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			a := evenerTestTurn("USER_INPUT", "shared")
			b := evenerTestTurn("ASSISTANT", "response")
			b["usage"] = map[string]any{"input_tokens": 2, "output_tokens": 3}
			parentID := "parent"
			if variant == "symlink parent" {
				other := t.TempDir()
				target := writeEvenerFixture(t, other, parentID, nil, a, b)
				require.NoError(t, os.Symlink(target, filepath.Join(dir, parentID+".transcript.jsonl")))
			}
			if variant != "missing parent" && variant != "symlink parent" {
				writeEvenerFixture(t, dir, parentID, nil, a, b)
			}
			parentMeta := filepath.Join(dir, parentID+".meta.json")
			switch variant {
			case "invalid parent metadata":
				require.NoError(t, os.WriteFile(parentMeta, []byte(`{bad}`), 0o600))
			case "mismatched parent metadata":
				require.NoError(t, os.WriteFile(parentMeta, []byte(`{"id":"another-session"}`), 0o600))
			case "symlink parent metadata":
				target := filepath.Join(t.TempDir(), "parent.meta.json")
				require.NoError(t, os.WriteFile(target, []byte(`{"id":"parent"}`), 0o600))
				require.NoError(t, os.Symlink(target, parentMeta))
			}
			if variant == "nested" {
				writeEvenerFixture(t, dir, "middle", map[string]any{"parent_session_id": "parent"}, a, b)
				writeEvenerMeta(t, filepath.Join(dir, "middle.transcript.jsonl"), map[string]any{"id": "middle", "parent_session_id": "parent", "divergence_turn": 2})
				parentID = "middle"
			}
			if variant == "different timestamp" {
				a = evenerTestTurn("USER_INPUT", "shared")
				a["timestamp"] = "2026-01-01T00:02:00Z"
			}
			if variant == "different kind" {
				a = evenerTestTurn("STEERING", "shared")
				a["steering_source"] = "user"
			}
			path := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": parentID}, a, b, evenerTestTurn("USER_INPUT", "child-only"))
			divergence := 3
			if variant == "subagent" {
				divergence = 0
			}
			writeEvenerMeta(t, path, map[string]any{"id": "child", "parent_session_id": parentID, "divergence_turn": divergence, "is_subagent": variant == "subagent"})
			sess, msgs, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			assert.Equal(t, "evener:"+parentID, sess.ParentSessionID)
			if variant == "verified" || variant == "nested" {
				require.Len(t, msgs, 1)
				assert.Equal(t, "child-only", msgs[0].Content)
				assert.Equal(t, 0, msgs[0].Ordinal)
				assert.Equal(t, 0, sess.TotalOutputTokens)
			} else {
				require.Len(t, msgs, 3)
				assert.Equal(t, 3, sess.TotalOutputTokens)
			}
			if variant == "subagent" {
				assert.Equal(t, RelSubagent, sess.RelationshipType)
			} else {
				assert.Equal(t, RelFork, sess.RelationshipType)
			}
		})
	}
}

func TestEvenerDelegateStructuredState(t *testing.T) {
	for _, name := range []string{"delegate", "delegate_send", "job_status"} {
		t.Run(name, func(t *testing.T) {
			call := evenerTestTurn("ASSISTANT", "")
			call["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_call", "tool_call": map[string]any{"id": "delegate-call", "name": name, "arguments": map[string]any{"task": "inspect"}}}}}
			result := evenerTestTurn("TOOL_RESULTS", "")
			result["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_result", "tool_result": map[string]any{"tool_call_id": "delegate-call", "content": "Worker started", "tool_state": map[string]any{"delegate_id": "delegate-1", "type": "delegate", "transcript_ref": "local:child", "status": "running", "structured_result": map[string]any{"detail": "retained"}}}}}}
			dir := t.TempDir()
			path := writeEvenerFixture(t, dir, "session", nil, call)
			_, pending, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.Len(t, pending[0].ToolCalls, 1)
			assert.Equal(t, "Other", pending[0].ToolCalls[0].Category)
			assert.Empty(t, pending[0].ToolCalls[0].SubagentSessionID)
			writeEvenerFixture(t, dir, "session", nil, call, result)
			_, messages, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			require.Len(t, messages, 2)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, "Task", messages[0].ToolCalls[0].Category)
			assert.Equal(t, "evener:child", messages[0].ToolCalls[0].SubagentSessionID)
			require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, "running", messages[0].ToolCalls[0].ResultEvents[0].Status)
			assert.Contains(t, messages[1].Content, "retained")
		})
	}
}

func TestEvenerParentTranscriptDependency(t *testing.T) {
	dir := t.TempDir()
	path := writeEvenerFixture(t, dir, "child", map[string]any{"parent_session_id": "parent"})
	for _, tc := range []struct {
		parent     string
		divergence int
		want       string
	}{
		{"parent", 3, filepath.Join(dir, "parent.transcript.jsonl")}, {"parent", 1, ""}, {"parent", 0, ""}, {"../outside", 3, ""}, {"", 3, ""}, {"child", 3, ""}, {"..", 3, ""}, {"bad:id", 3, ""},
	} {
		writeEvenerMeta(t, path, map[string]any{"id": "child", "parent_session_id": tc.parent, "divergence_turn": tc.divergence})
		parent, err := evenerParentTranscriptPath(path)
		require.NoError(t, err)
		assert.Equal(t, tc.want, parent)
	}
	writeEvenerMeta(t, path, map[string]any{"id": "mismatch", "parent_session_id": "parent", "divergence_turn": 3})
	_, err := evenerParentTranscriptPath(path)
	require.Error(t, err)
}

func TestEvenerHeaderContextPreservesInitialInstructions(t *testing.T) {
	path := writeEvenerFixture(t, t.TempDir(), "session", map[string]any{
		"system_prompt": "Use the repository conventions.",
		"task":          "Inspect the build failure.",
		"agent_tasks":   []any{map[string]any{"id": 1, "type": "test", "description": "Run the focused checks", "prompt": "Run the focused checks", "status": "pending"}},
	}, evenerTestTurn("USER_INPUT", "Please fix the build."))
	sess, msgs, err := parseEvenerSession(context.Background(), path, "test")
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	for i, msg := range msgs[:3] {
		assert.Equal(t, RoleSystem, msg.Role)
		assert.True(t, msg.IsSystem)
		assert.Equal(t, i, msg.Ordinal)
		assert.Empty(t, msg.TokenUsage)
	}
	assert.Contains(t, msgs[0].Content, "repository conventions")
	assert.Contains(t, msgs[1].Content, "Inspect the build failure")
	assert.Contains(t, msgs[2].Content, "Run the focused checks")
	assert.Equal(t, "Please fix the build.", sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, 3, msgs[3].Ordinal)
}

func TestEvenerCompactionMarksBoundaryWithoutRemovingHistory(t *testing.T) {
	for _, kind := range []string{"CHECKPOINT", "SUMMARY"} {
		t.Run(kind, func(t *testing.T) {
			path := writeEvenerFixture(t, t.TempDir(), "session", nil, evenerTestTurn("USER_INPUT", "Before"), evenerTestTurn(kind, "Retained summary"), evenerTestTurn("USER_INPUT", "After"))
			_, msgs, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			require.Len(t, msgs, 3)
			assert.Equal(t, "Before", msgs[0].Content)
			assert.True(t, msgs[1].IsCompactBoundary)
			assert.True(t, msgs[1].IsSystem)
			assert.Equal(t, "Retained summary", msgs[1].Content)
			assert.Equal(t, "After", msgs[2].Content)
		})
	}
}

func TestEvenerRelationshipRequiresMetadataEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
		want RelationshipType
	}{
		{name: "missing sidecar"},
		{name: "no explicit classification", meta: map[string]any{"id": "child"}},
		{name: "fork", meta: map[string]any{"id": "child", "parent_session_id": "parent", "divergence_turn": 1}, want: RelFork},
		{name: "subagent", meta: map[string]any{"id": "child", "is_subagent": true}, want: RelSubagent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEvenerFixture(t, t.TempDir(), "child", map[string]any{"parent_session_id": "parent", "parent_tool_call_id": "spawn-call", "depth": 2}, evenerTestTurn("USER_INPUT", "Own message"))
			if tc.meta != nil {
				writeEvenerMeta(t, path, tc.meta)
			}
			sess, msgs, err := parseEvenerSession(context.Background(), path, "test")
			require.NoError(t, err)
			assert.Equal(t, "evener:parent", sess.ParentSessionID)
			assert.Equal(t, tc.want, sess.RelationshipType)
			require.Len(t, msgs, 1)
		})
	}
}

func TestEvenerRedactedThinkingOmitsOpaquePayload(t *testing.T) {
	turn := evenerTestTurn("ASSISTANT", "")
	turn["message"] = map[string]any{"content": []any{map[string]any{"kind": "redacted_thinking", "thinking": map[string]any{"text": "opaque-encrypted-payload", "redacted": true}}, map[string]any{"kind": "text", "text": "Visible answer"}}}
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
	_, msgs, err := parseEvenerSession(context.Background(), path, "test")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasThinking)
	assert.Contains(t, msgs[0].ThinkingText, "redacted")
	assert.NotContains(t, msgs[0].ThinkingText, "opaque-encrypted-payload")
	assert.Contains(t, msgs[0].Content, "Visible answer")
	assert.NotContains(t, msgs[0].Content, "opaque-encrypted-payload")
}

func TestEvenerContentUsesTranscriptRenderingMarkers(t *testing.T) {
	turn := evenerTestTurn("ASSISTANT", "")
	turn["message"] = map[string]any{"content": []any{
		map[string]any{"kind": "text", "text": "Before reasoning"},
		map[string]any{"kind": "thinking", "thinking": map[string]any{"text": "Consider the input"}},
		map[string]any{"kind": "tool_call", "tool_call": map[string]any{"id": "call-1", "name": "exec_command", "arguments": map[string]any{"cmd": "pwd"}}},
		map[string]any{"kind": "text", "text": "After the tool"},
	}}
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
	_, msgs, err := parseEvenerSession(context.Background(), path, "test")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	content := msgs[0].Content
	assert.Contains(t, content, "[Thinking]\nConsider the input\n[/Thinking]")
	assert.Contains(t, content, "[Tool: exec_command]\n\nAfter the tool")
	assert.Equal(t, "Consider the input", msgs[0].ThinkingText)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.JSONEq(t, `{"cmd":"pwd"}`, msgs[0].ToolCalls[0].InputJSON)
	assert.NotContains(t, content, `{"cmd":"pwd"}`, "tool arguments belong to the structured tool block")
}
