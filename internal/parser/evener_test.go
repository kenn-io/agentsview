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
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func evenerTestTurn(kind, text string) map[string]any {
	return map[string]any{"kind": kind, "timestamp": "2026-01-01T00:01:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"kind": "text", "text": text}}}}
}

func writeEvenerMeta(t *testing.T, path string, meta map[string]any) {
	t.Helper()
	b, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path[:len(path)-len(".transcript.jsonl")]+".meta.json", b, 0o600))
}

func TestEvenerSemanticSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	answer := evenerTestTurn("ASSISTANT", "Hello")
	answer["response_model"] = "response-model"
	answer["response_provider"] = "response-provider"
	answer["usage"] = map[string]any{"input_tokens": 100, "output_tokens": 20, "cache_read_tokens": 30, "cache_write_tokens": 40, "cache_write_1h_tokens": 50, "reasoning_tokens": 10}
	path := writeEvenerFixture(t, dir, "session", nil, evenerTestTurn("USER_INPUT", "Question"), answer)
	writeEvenerMeta(t, path, map[string]any{"id": "session", "name": "Title", "model": "current-model"})
	sess, msgs, err := parseEvenerSession(t.Context(), path, "test")
	require.NoError(err)
	require.NotNil(sess)
	require.Len(msgs, 2)
	assert.Equal("evener:session", sess.ID)
	assert.Equal(AgentType("evener"), sess.Agent)
	assert.Equal("Title", sess.SessionName)
	assert.True(sess.SessionNamePresent)
	assert.Equal(dir, sess.Cwd)
	assert.Equal(1, sess.UserMessageCount)
	assert.Equal("Question", sess.FirstMessage)
	assert.Equal(20, sess.TotalOutputTokens)
	assert.Equal(220, sess.PeakContextTokens)
	assert.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), sess.StartedAt)
	assert.Equal(RoleAssistant, msgs[1].Role)
	assert.Equal("response-model", msgs[1].Model)
	assert.Equal("response-provider", msgs[1].ProviderID)
	assert.JSONEq(`{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":90,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":50},"reasoning_tokens":10}`, string(msgs[1].TokenUsage))
	assert.Equal(1, msgs[1].Ordinal)
}

func TestEvenerKindsAndContent(t *testing.T) {
	for _, tc := range []struct {
		kind, source string
		role         RoleType
	}{
		{"USER_INPUT", "", RoleUser},
		{"STEERING", "user", RoleUser},
		{"STEERING", "", RoleSystem},
		{"STEERING", "daemon", RoleSystem},
		{"ASSISTANT", "", RoleAssistant},
		{"TOOL", "", RoleTool},
		{"TOOL_RESULTS", "", RoleTool},
		{"SYSTEM", "", RoleSystem},
		{"CHECKPOINT", "", RoleSystem},
		{"SUMMARY", "", RoleSystem},
		{"MODEL_SWITCH", "", RoleSystem},
		{"TURN_FAILURE", "", RoleSystem},
		{"HOOK_COMPLETED", "", RoleSystem},
		{"ENVIRONMENT", "", RoleSystem},
		{"ATTENTION_RESOLUTION", "", RoleSystem},
		{"FUTURE_KIND", "", RoleSystem},
	} {
		t.Run(tc.kind+tc.source, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			turn := evenerTestTurn(tc.kind, "visible")
			turn["steering_source"] = tc.source
			path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
			_, msgs, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(err)
			require.Len(msgs, 1)
			assert.Equal(tc.role, msgs[0].Role)
			assert.Equal(tc.role == RoleSystem, msgs[0].IsSystem)
			assert.Contains(msgs[0].Content, "visible")
		})
	}
	t.Run("thinking and media", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		turn := evenerTestTurn("ASSISTANT", "")
		turn["message"] = map[string]any{"content": []any{
			map[string]any{"kind": "text", "text": "answer"},
			map[string]any{"kind": "thinking", "thinking": map[string]any{"text": "reasoning"}},
			map[string]any{"kind": "redacted_thinking", "thinking": map[string]any{"redacted": true}},
			map[string]any{"kind": "image", "image": map[string]any{"data": "c2VjcmV0", "media_type": "image/png"}},
			map[string]any{"kind": "audio", "audio": map[string]any{"url": "https://example.invalid/audio"}},
			map[string]any{"kind": "document", "document": map[string]any{"file_name": "notes.pdf"}},
			map[string]any{"kind": "web_search", "web_search": map[string]any{"query": "reference"}},
			map[string]any{"kind": "future_content", "text": "future detail"},
		}}
		path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
		_, msgs, err := parseEvenerSession(t.Context(), path, "test")
		require.NoError(err)
		require.Len(msgs, 1)
		assert.True(msgs[0].HasThinking)
		assert.Contains(msgs[0].ThinkingText, "reasoning")
		assert.Contains(msgs[0].ThinkingText, "redacted")
		for _, want := range []string{"answer", "image", "audio", "notes.pdf", "reference", "future detail"} {
			assert.Contains(msgs[0].Content, want)
		}
		assert.Contains(msgs[0].Content, "[Thinking]\nreasoning\n[/Thinking]")
		assert.NotContains(msgs[0].Content, "c2VjcmV0")
	})
	t.Run("diagnostics", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		failure := evenerTestTurn("TURN_FAILURE", "")
		failure["error"] = map[string]any{"message": "failed", "hint": "retry", "title": "Error"}
		hook := evenerTestTurn("HOOK_COMPLETED", "")
		hook["hook"] = map[string]any{"event": "stop", "exit_code": 0, "plugin_name": "example"}
		unknown := evenerTestTurn("FUTURE_KIND", "")
		unknown["detail"] = "retained"
		path := writeEvenerFixture(t, t.TempDir(), "session", nil, failure, hook, unknown)
		_, msgs, err := parseEvenerSession(t.Context(), path, "test")
		require.NoError(err)
		require.Len(msgs, 3)
		assert.Contains(msgs[0].Content, "failed")
		assert.Contains(msgs[0].Content, "retry")
		assert.Contains(msgs[1].Content, "stop")
		assert.Contains(msgs[1].Content, "0")
		assert.Contains(msgs[2].Content, "FUTURE_KIND")
		assert.Contains(msgs[2].Content, "retained")
	})
}

func TestEvenerTools(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	call := evenerTestTurn("ASSISTANT", "")
	call["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_call", "tool_call": map[string]any{"id": "call-1", "name": "shell", "arguments": map[string]any{"command": "pwd"}}}}}
	result := evenerTestTurn("TOOL_RESULTS", "")
	result["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_result", "tool_result": map[string]any{"tool_call_id": "call-1", "name": "shell", "content": map[string]any{"exit_code": 1, "output": "failed"}, "is_error": true, "image_data": "c2VjcmV0"}}}}
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, call, result, result)
	_, msgs, err := parseEvenerSession(t.Context(), path, "test")
	require.NoError(err)
	require.Len(msgs, 3)
	require.Len(msgs[0].ToolCalls, 1)
	assert.True(msgs[0].HasToolUse)
	tc := msgs[0].ToolCalls[0]
	assert.Equal("call-1", tc.ToolUseID)
	assert.Equal("shell", tc.ToolName)
	assert.JSONEq(`{"command":"pwd"}`, tc.InputJSON)
	require.Len(tc.ResultEvents, 2)
	assert.Equal("error", tc.ResultEvents[0].Status)
	require.Len(msgs[1].ToolResults, 1)
	assert.Contains(DecodeContent(msgs[1].ToolResults[0].ContentRaw), "failed")
	assert.Contains(DecodeContent(msgs[1].ToolResults[0].ContentRaw), "image")
	assert.NotContains(DecodeContent(msgs[1].ToolResults[0].ContentRaw), "c2VjcmV0")
	assert.Empty(msgs[1].Content)
}

func TestEvenerModelTimelineAndUsagePresence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	sess, msgs, err := parseEvenerSession(t.Context(), path, "test")
	require.NoError(err)
	require.Len(msgs, 9)
	for i, want := range map[int]string{0: "model-a", 2: "model-b", 4: "model-c", 6: "", 7: "actual-model", 8: ""} {
		assert.Equal(want, msgs[i].Model)
	}
	assert.Equal("provider-b", msgs[4].ProviderID)
	assert.Empty(msgs[6].ProviderID)
	assert.NotEmpty(msgs[0].TokenUsage)
	assert.True(msgs[0].HasOutputTokens)
	assert.True(msgs[0].HasContextTokens)
	assert.Empty(msgs[8].TokenUsage)
	assert.False(msgs[8].HasOutputTokens)
	assert.True(sess.HasTotalOutputTokens)
}

func TestEvenerValidationAndFraming(t *testing.T) {
	for _, tc := range []struct {
		name, tail         string
		truncated, wantErr bool
	}{
		{"partial json", `{"kind":`, true, false},
		{"valid unterminated", `{"kind":"entry","turn":{"kind":"USER_INPUT"}}`, true, false},
		{"complete malformed", "{bad}\n", false, true},
		{"invalid record kind", "{\"kind\":\"response\"}\n", false, true},
		{"missing turn", "{\"kind\":\"entry\"}\n", false, true},
		{"invalid sequence", "{\"kind\":\"entry\",\"seq\":\"bad\",\"turn\":{\"kind\":\"USER_INPUT\"}}\n", false, true},
		{"clean eof", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			path := writeEvenerFixture(t, t.TempDir(), "session", nil, evenerTestTurn("USER_INPUT", "kept"))
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(err)
			_, err = f.WriteString(tc.tail)
			require.NoError(err)
			require.NoError(f.Close())
			sess, msgs, err := parseEvenerSession(t.Context(), path, "test")
			if tc.wantErr {
				require.Error(err)
				assert.Nil(sess)
				return
			}
			require.NoError(err)
			require.Len(msgs, 1)
			assert.Equal(tc.truncated, sess.IsTruncated)
		})
	}
	for _, header := range []map[string]any{{"format_version": 1}, {"session_id": "other"}, {"kind": "other"}} {
		t.Run("header", func(t *testing.T) {
			path := writeEvenerFixture(t, t.TempDir(), "session", header)
			sess, _, err := parseEvenerSession(t.Context(), path, "test")
			require.Error(t, err)
			assert.Nil(t, sess)
		})
	}
	for _, meta := range []string{`{"id":"other"}`, `{broken}`, `null`} {
		t.Run("metadata", func(t *testing.T) {
			dir := t.TempDir()
			path := writeEvenerFixture(t, dir, "session", nil)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "session.meta.json"), []byte(meta), 0o600))
			sess, _, err := parseEvenerSession(t.Context(), path, "test")
			require.Error(t, err)
			assert.Nil(t, sess)
		})
	}
	t.Run("rename and clear", func(t *testing.T) {
		path := writeEvenerFixture(t, t.TempDir(), "session", nil)
		for _, name := range []string{"First", "Second", ""} {
			writeEvenerMeta(t, path, map[string]any{"id": "session", "name": name})
			sess, _, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(t, err)
			assert.Equal(t, name, sess.SessionName)
			assert.True(t, sess.SessionNamePresent)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := parseEvenerSession(ctx, "unused", "test")
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestEvenerVerifiedForkPrefix(t *testing.T) {
	for _, variant := range []string{"verified", "missing parent", "different timestamp", "different kind", "nested", "subagent", "symlink parent", "invalid parent metadata", "mismatched parent metadata", "symlink parent metadata"} {
		t.Run(variant, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dir := t.TempDir()
			a := evenerTestTurn("USER_INPUT", "shared")
			b := evenerTestTurn("ASSISTANT", "response")
			b["usage"] = map[string]any{"input_tokens": 2, "output_tokens": 3}
			parentID := "parent"
			if variant == "symlink parent" {
				other := t.TempDir()
				target := writeEvenerFixture(t, other, parentID, nil, a, b)
				require.NoError(os.Symlink(target, filepath.Join(dir, parentID+".transcript.jsonl")))
			}
			if variant != "missing parent" && variant != "symlink parent" {
				writeEvenerFixture(t, dir, parentID, nil, a, b)
			}
			parentMeta := filepath.Join(dir, parentID+".meta.json")
			switch variant {
			case "invalid parent metadata":
				require.NoError(os.WriteFile(parentMeta, []byte(`{bad}`), 0o600))
			case "mismatched parent metadata":
				require.NoError(os.WriteFile(parentMeta, []byte(`{"id":"another-session"}`), 0o600))
			case "symlink parent metadata":
				target := filepath.Join(t.TempDir(), "parent.meta.json")
				require.NoError(os.WriteFile(target, []byte(`{"id":"parent"}`), 0o600))
				require.NoError(os.Symlink(target, parentMeta))
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
			sess, msgs, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(err)
			assert.Equal("evener:"+parentID, sess.ParentSessionID)
			if variant == "verified" || variant == "nested" {
				require.Len(msgs, 1)
				assert.Equal("child-only", msgs[0].Content)
				assert.Equal(0, msgs[0].Ordinal)
				assert.Equal(0, sess.TotalOutputTokens)
			} else {
				require.Len(msgs, 3)
				assert.Equal(3, sess.TotalOutputTokens)
			}
			if variant == "subagent" {
				assert.Equal(RelSubagent, sess.RelationshipType)
			} else {
				assert.Equal(RelFork, sess.RelationshipType)
			}
		})
	}
}

func TestEvenerDelegateStructuredState(t *testing.T) {
	for _, name := range []string{"delegate", "delegate_send", "job_status"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			call := evenerTestTurn("ASSISTANT", "")
			call["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_call", "tool_call": map[string]any{"id": "delegate-call", "name": name, "arguments": map[string]any{"task": "inspect"}}}}}
			result := evenerTestTurn("TOOL_RESULTS", "")
			result["message"] = map[string]any{"content": []any{map[string]any{"kind": "tool_result", "tool_result": map[string]any{"tool_call_id": "delegate-call", "content": "Worker started", "tool_state": map[string]any{"delegate_id": "delegate-1", "type": "delegate", "transcript_ref": "local:child", "status": "running", "structured_result": map[string]any{"detail": "retained"}}}}}}
			dir := t.TempDir()
			path := writeEvenerFixture(t, dir, "session", nil, call)
			_, pending, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(err)
			require.Len(pending, 1)
			require.Len(pending[0].ToolCalls, 1)
			assert.Equal("Other", pending[0].ToolCalls[0].Category)
			assert.Empty(pending[0].ToolCalls[0].SubagentSessionID)
			writeEvenerFixture(t, dir, "session", nil, call, result)
			_, messages, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(err)
			require.Len(messages, 2)
			require.Len(messages[0].ToolCalls, 1)
			assert.Equal("Task", messages[0].ToolCalls[0].Category)
			assert.Equal("evener:child", messages[0].ToolCalls[0].SubagentSessionID)
			require.Len(messages[0].ToolCalls[0].ResultEvents, 1)
			assert.Equal("running", messages[0].ToolCalls[0].ResultEvents[0].Status)
			assert.Contains(messages[0].ToolCalls[0].ResultEvents[0].Content, "retained")
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
	assert := assert.New(t)
	require := require.New(t)

	path := writeEvenerFixture(t, t.TempDir(), "session", map[string]any{
		"system_prompt": "Use the repository conventions.",
		"task":          "Inspect the build failure.",
		"agent_tasks":   []any{map[string]any{"id": 1, "type": "test", "description": "Run the focused checks", "prompt": "Run the focused checks", "status": "pending"}},
	}, evenerTestTurn("USER_INPUT", "Please fix the build."))
	sess, msgs, err := parseEvenerSession(t.Context(), path, "test")
	require.NoError(err)
	require.Len(msgs, 4)
	for i, msg := range msgs[:3] {
		assert.Equal(RoleSystem, msg.Role)
		assert.True(msg.IsSystem)
		assert.Equal(i, msg.Ordinal)
		assert.Empty(msg.TokenUsage)
	}
	assert.Contains(msgs[0].Content, "repository conventions")
	assert.Contains(msgs[1].Content, "Inspect the build failure")
	assert.Contains(msgs[2].Content, "Run the focused checks")
	assert.Equal("Please fix the build.", sess.FirstMessage)
	assert.Equal(1, sess.UserMessageCount)
	assert.Equal(3, msgs[3].Ordinal)
}

func TestEvenerCompactionMarksBoundaryWithoutRemovingHistory(t *testing.T) {
	for _, kind := range []string{"CHECKPOINT", "SUMMARY"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			path := writeEvenerFixture(t, t.TempDir(), "session", nil, evenerTestTurn("USER_INPUT", "Before"), evenerTestTurn(kind, "Retained summary"), evenerTestTurn("USER_INPUT", "After"))
			_, msgs, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(err)
			require.Len(msgs, 3)
			assert.Equal("Before", msgs[0].Content)
			assert.True(msgs[1].IsCompactBoundary)
			assert.True(msgs[1].IsSystem)
			assert.Equal("Retained summary", msgs[1].Content)
			assert.Equal("After", msgs[2].Content)
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
			assert := assert.New(t)
			require := require.New(t)

			path := writeEvenerFixture(t, t.TempDir(), "child", map[string]any{"parent_session_id": "parent", "parent_tool_call_id": "spawn-call", "depth": 2}, evenerTestTurn("USER_INPUT", "Own message"))
			if tc.meta != nil {
				writeEvenerMeta(t, path, tc.meta)
			}
			sess, msgs, err := parseEvenerSession(t.Context(), path, "test")
			require.NoError(err)
			assert.Equal("evener:parent", sess.ParentSessionID)
			assert.Equal(tc.want, sess.RelationshipType)
			require.Len(msgs, 1)
		})
	}
}

func TestEvenerRedactedThinkingOmitsOpaquePayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	turn := evenerTestTurn("ASSISTANT", "")
	turn["message"] = map[string]any{"content": []any{map[string]any{"kind": "redacted_thinking", "thinking": map[string]any{"text": "opaque-encrypted-payload", "redacted": true}}, map[string]any{"kind": "text", "text": "Visible answer"}}}
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
	_, msgs, err := parseEvenerSession(t.Context(), path, "test")
	require.NoError(err)
	require.Len(msgs, 1)
	assert.True(msgs[0].HasThinking)
	assert.Contains(msgs[0].ThinkingText, "redacted")
	assert.NotContains(msgs[0].ThinkingText, "opaque-encrypted-payload")
	assert.Contains(msgs[0].Content, "Visible answer")
	assert.NotContains(msgs[0].Content, "opaque-encrypted-payload")
}

func TestEvenerContentUsesTranscriptRenderingMarkers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	turn := evenerTestTurn("ASSISTANT", "")
	turn["message"] = map[string]any{"content": []any{
		map[string]any{"kind": "text", "text": "Before reasoning"},
		map[string]any{"kind": "thinking", "thinking": map[string]any{"text": "Consider the input"}},
		map[string]any{"kind": "tool_call", "tool_call": map[string]any{"id": "call-1", "name": "exec_command", "arguments": map[string]any{"cmd": "pwd"}}},
		map[string]any{"kind": "text", "text": "After the tool"},
	}}
	path := writeEvenerFixture(t, t.TempDir(), "session", nil, turn)
	_, msgs, err := parseEvenerSession(t.Context(), path, "test")
	require.NoError(err)
	require.Len(msgs, 1)
	content := msgs[0].Content
	assert.Contains(content, "[Thinking]\nConsider the input\n[/Thinking]")
	assert.Contains(content, "[Tool: exec_command]\n\nAfter the tool")
	assert.Equal("Consider the input", msgs[0].ThinkingText)
	require.Len(msgs[0].ToolCalls, 1)
	assert.JSONEq(`{"cmd":"pwd"}`, msgs[0].ToolCalls[0].InputJSON)
	assert.NotContains(content, `{"cmd":"pwd"}`, "tool arguments belong to the structured tool block")
}
