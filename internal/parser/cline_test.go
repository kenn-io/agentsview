package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestCleanClinePrompt(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain prompt",
			input: "Please fix the button styling",
			want:  "Please fix the button styling",
		},
		{
			name:  "user_input mode act",
			input: `<user_input mode="act">Please fix the button styling</user_input>`,
			want:  "Please fix the button styling",
		},
		{
			name:  "user_input mode plan",
			input: `<user_input mode="plan">Investigate the architecture</user_input>`,
			want:  "Investigate the architecture",
		},
		{
			name:  "user_input no mode",
			input: `<user_input>Run unit tests</user_input>`,
			want:  "Run unit tests",
		},
		{
			name:  "multiline user_input with whitespace",
			input: "  <user_input mode=\"act\">\nLine 1\nLine 2\n</user_input>  ",
			want:  "Line 1\nLine 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanClinePrompt(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseClineSession_Full(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000000_abcde"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"version": 1,
		"session_id": "1789000000000_abcde",
		"source": "cli",
		"started_at": "2026-09-10T10:00:00.000Z",
		"ended_at": "2026-09-10T10:05:00.000Z",
		"exit_code": 0,
		"status": "completed",
		"model": "deepseek/deepseek-v4-flash",
		"cwd": "/workspace/astro-spectrometer",
		"prompt": "<user_input mode=\"act\">Calibrate orbital telescope spectrometer frequency</user_input>",
		"metadata": {
			"title": "Calibrate orbital telescope spectrometer frequency",
			"totalCost": 0.015,
			"git": {
				"branch": "instruments/optics"
			},
			"usage": {
				"inputTokens": 20000,
				"outputTokens": 500,
				"cacheReadTokens": 15000,
				"cacheWriteTokens": 0,
				"totalCost": 0.015
			}
		}
	}`
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "msg_001",
				"role": "user",
				"content": [
					{
						"type": "text",
						"text": "<user_input mode=\"act\">Calibrate orbital telescope spectrometer frequency</user_input>"
					}
				],
				"ts": 1789034400000
			},
			{
				"id": "msg_002",
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "I should read the optics definition and run tests."
					},
					{
						"type": "text",
						"text": "I will inspect the optics instrumentation files."
					},
					{
						"type": "tool_use",
						"id": "call_001",
						"name": "read_files",
						"input": {"files": [{"path": "instruments/optics.py"}]}
					},
					{
						"type": "tool_use",
						"id": "call_002",
						"name": "Skill",
						"input": {"skill": "stellar-cartography"}
					}
				],
				"ts": 1789034402000,
				"modelInfo": {
					"id": "deepseek/deepseek-v4-flash"
				},
				"metrics": {
					"inputTokens": 5000,
					"outputTokens": 200,
					"cacheReadTokens": 4000,
					"cacheWriteTokens": 0
				}
			},
			{
				"id": "msg_003",
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_001",
						"name": "read_files",
						"content": [
							{
								"query": "instruments/optics.py",
								"result": "class Spectrometer:\n    def calibrate_frequency(self): return 432.0",
								"success": true
							}
						]
					},
					{
						"type": "tool_result",
						"tool_use_id": "call_002",
						"name": "Skill",
						"content": "Skill loaded successfully."
					}
				],
				"ts": 1789034403000
			},
			{
				"id": "msg_004",
				"role": "assistant",
				"content": [
					{
						"type": "text",
						"text": "Spectrometer optics calibrated to 432.0nm and telemetry frequency is locked."
					}
				],
				"ts": 1789034405000,
				"metrics": {
					"inputTokens": 6000,
					"outputTokens": 100,
					"cacheReadTokens": 5000,
					"cacheWriteTokens": 0
				}
			}
		]
	}`
	messagesPath := filepath.Join(taskDir, sessionID+".messages.json")
	require.NoError(t, os.WriteFile(messagesPath, []byte(messagesJSON), 0o644))

	sess, msgs, err := parseClineSession(metaPath, "astro-spectrometer", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, "cline:"+sessionID, sess.ID)
	assert.Equal(t, AgentCline, sess.Agent)
	assert.Equal(t, "Calibrate orbital telescope spectrometer frequency", sess.FirstMessage)
	assert.Equal(t, "Calibrate orbital telescope spectrometer frequency", sess.SessionName)
	assert.Equal(t, "astro_spectrometer", sess.Project)
	assert.Equal(t, "/workspace/astro-spectrometer", sess.Cwd)
	assert.Equal(t, "instruments/optics", sess.GitBranch)
	assert.Equal(t, TerminationClean, sess.TerminationStatus)
	assert.Equal(t, 4, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Peak context tokens = max(5000+4000, 6000+5000) = 11000
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 11000, sess.PeakContextTokens)

	// Total output tokens from usage (capped/extended by metadata usage)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 500, sess.TotalOutputTokens)

	// Reported totalCost is preserved on an authoritative usage event with zero tokens
	// to avoid double-counting per-message token usage.
	require.Len(t, sess.UsageEvents, 1)
	require.NotNil(t, sess.UsageEvents[0].Cost)
	assert.Equal(t, money.MustParseDollars("0.015"), *sess.UsageEvents[0].Cost)
	assert.Zero(t, sess.UsageEvents[0].InputTokens)
	assert.Zero(t, sess.UsageEvents[0].OutputTokens)
	assert.Zero(t, sess.UsageEvents[0].CacheReadInputTokens)
	assert.Zero(t, sess.UsageEvents[0].CacheCreationInputTokens)

	// Messages checks
	require.Len(t, msgs, 4)

	// msg 0: user prompt
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.False(t, msgs[0].IsSystem)
	assert.Equal(t, "Calibrate orbital telescope spectrometer frequency", msgs[0].Content)

	// msg 1: assistant with thinking and 2 tool calls
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "I should read the optics definition and run tests.", msgs[1].ThinkingText)
	assert.Equal(t, "[Thinking]\nI should read the optics definition and run tests.\n[/Thinking]\n\nI will inspect the optics instrumentation files.", msgs[1].Content)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 2)

	tc0 := msgs[1].ToolCalls[0]
	assert.Equal(t, "call_001", tc0.ToolUseID)
	assert.Equal(t, "read_files", tc0.ToolName)
	assert.Equal(t, "Read", tc0.Category)
	require.Len(t, tc0.ResultEvents, 1)
	assert.Equal(t, "completed", tc0.ResultEvents[0].Status)
	assert.Contains(t, tc0.ResultEvents[0].Content, "class Spectrometer")

	tc1 := msgs[1].ToolCalls[1]
	assert.Equal(t, "call_002", tc1.ToolUseID)
	assert.Equal(t, "Skill", tc1.ToolName)
	assert.Equal(t, "Tool", tc1.Category)
	assert.Equal(t, "stellar-cartography", tc1.SkillName)
	require.Len(t, tc1.ResultEvents, 1)
	assert.Equal(t, "completed", tc1.ResultEvents[0].Status)
	assert.Equal(t, "Skill loaded successfully.", tc1.ResultEvents[0].Content)

	require.NotEmpty(t, msgs[1].TokenUsage)
	assert.JSONEq(t, `{"input_tokens":5000,"output_tokens":200,"cache_read_input_tokens":4000,"cache_creation_input_tokens":0}`, string(msgs[1].TokenUsage))
	assert.True(t, msgs[1].HasOutputTokens)
	assert.Equal(t, 200, msgs[1].OutputTokens)
	assert.True(t, msgs[1].HasContextTokens)
	assert.Equal(t, 9000, msgs[1].ContextTokens)

	// msg 2: tool results only -> system flag and subtype
	assert.Equal(t, RoleUser, msgs[2].Role)
	assert.True(t, msgs[2].IsSystem)
	assert.Equal(t, SourceSubtypeToolResult, msgs[2].SourceSubtype)
	require.Len(t, msgs[2].ToolResults, 2)
	assert.Equal(t, "call_001", msgs[2].ToolResults[0].ToolUseID)
	assert.Contains(t, DecodeContent(msgs[2].ToolResults[0].ContentRaw), "class Spectrometer")
	assert.Equal(t, len(tc0.ResultEvents[0].Content), msgs[2].ToolResults[0].ContentLength)

	// msg 3: final assistant text
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t, "Spectrometer optics calibrated to 432.0nm and telemetry frequency is locked.", msgs[3].Content)
	require.NotEmpty(t, msgs[3].TokenUsage)
	assert.JSONEq(t, `{"input_tokens":6000,"output_tokens":100,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0}`, string(msgs[3].TokenUsage))
	assert.True(t, msgs[3].HasOutputTokens)
	assert.Equal(t, 100, msgs[3].OutputTokens)
	assert.True(t, msgs[3].HasContextTokens)
	assert.Equal(t, 11000, msgs[3].ContextTokens)
}

func TestParseClineSession_FallbackUsageEvent(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000099_fallback"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"version": 1,
		"session_id": "1789000000099_fallback",
		"started_at": "2026-09-10T10:00:00.000Z",
		"ended_at": "2026-09-10T10:05:00.000Z",
		"status": "completed",
		"model": "deepseek/deepseek-v4-flash",
		"metadata": {
			"totalCost": 0.015,
			"usage": {
				"inputTokens": 20000,
				"outputTokens": 500,
				"cacheReadTokens": 15000,
				"cacheWriteTokens": 0,
				"totalCost": 0.015
			}
		}
	}`
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	// No messages file exists; parser should emit fallback aggregate usage event.
	sess, msgs, err := parseClineSession(metaPath, "test-proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Empty(t, msgs)

	require.Len(t, sess.UsageEvents, 1)
	ue := sess.UsageEvents[0]
	assert.Equal(t, "cline:"+sessionID, ue.SessionID)
	assert.Equal(t, "deepseek/deepseek-v4-flash", ue.Model)
	assert.Equal(t, 20000, ue.InputTokens)
	assert.Equal(t, 500, ue.OutputTokens)
	assert.Equal(t, 15000, ue.CacheReadInputTokens)
	require.NotNil(t, ue.Cost)
	assert.Equal(t, money.MustParseDollars("0.015"), *ue.Cost)
}

func TestParseClineSession_CommandError(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000001_err"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000001_err",
		"status": "failed",
		"prompt": "build hydrophone telemetry"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "build hydrophone telemetry"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_cmd",
						"name": "execute_command",
						"input": {"commands": ["make -C firmware build"]}
					}
				],
				"ts": 2000
			},
			{
				"id": "m3",
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_cmd",
						"name": "execute_command",
						"content": [
							{
								"query": "make -C firmware build",
								"result": "exit code 1: missing hydrophone header",
								"error": "Command exited with code 1",
								"success": false
							}
						]
					}
				],
				"ts": 3000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, msgs, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Since status was "failed", termination status is Truncated
	assert.Equal(t, TerminationTruncated, sess.TerminationStatus)
	require.Len(t, msgs, 3)

	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "Bash", tc.Category)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "errored", tc.ResultEvents[0].Status)
	assert.Contains(t, tc.ResultEvents[0].Content, "missing hydrophone header")
}

func TestParseClineSession_OrphanedToolCall(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000002_orphan"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000002_orphan",
		"status": "completed",
		"prompt": "tune hydrophone filter"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	// Tool call with no tool_result
	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "tune hydrophone filter"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_edit",
						"name": "edit_file",
						"input": {"path": "firmware/filter.c"}
					}
				],
				"ts": 2000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, _, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Tool call pending detected despite status="completed" in metadata
	assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
}

func TestParseClineSession_AttemptCompletionEnding(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000007_complete"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000007_complete",
		"status": "completed",
		"prompt": "build filter"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "build filter"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "text",
						"text": "Filter has been built successfully."
					},
					{
						"type": "tool_use",
						"id": "call_done",
						"name": "attempt_completion",
						"input": {"result": "Built successfully"}
					}
				],
				"ts": 2000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, _, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, TerminationClean, sess.TerminationStatus)
}

func TestParseClineSession_AttemptCompletionWithUnresolvedPrecedingToolCall(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000008_unresolved"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000008_unresolved",
		"status": "completed",
		"prompt": "run command"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "run command"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_cmd",
						"name": "execute_command",
						"input": {"command": "go test ./..."}
					},
					{
						"type": "tool_use",
						"id": "call_done",
						"name": "attempt_completion",
						"input": {"result": "Done"}
					}
				],
				"ts": 2000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, _, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Preceding unresolved tool call flags termination as pending despite attempt_completion
	assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
}

func TestParseClineSession_AttemptCompletionWithResolvedPrecedingToolCall(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000009_resolved"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000009_resolved",
		"status": "completed",
		"prompt": "run command"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "run command"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_cmd",
						"name": "execute_command",
						"input": {"command": "go test ./..."}
					}
				],
				"ts": 2000
			},
			{
				"id": "m3",
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_cmd",
						"content": "ok"
					}
				],
				"ts": 3000
			},
			{
				"id": "m4",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_done",
						"name": "attempt_completion",
						"input": {"result": "Done"}
					}
				],
				"ts": 4000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, _, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// All preceding tool calls resolved, attempt_completion marks session clean
	assert.Equal(t, TerminationClean, sess.TerminationStatus)
}

func TestParseClineSession_ThinkingContentInlining(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000010_thinking_inline"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000010_thinking_inline",
		"status": "completed",
		"prompt": "explain this"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "explain this"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "Let me think about how to explain this."
					},
					{
						"type": "text",
						"text": "Here is the explanation."
					}
				],
				"ts": 2000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, messages, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, messages, 2)

	asst := messages[1]
	assert.True(t, asst.HasThinking)
	assert.Equal(t, "Let me think about how to explain this.", asst.ThinkingText)
	assert.Equal(t, "[Thinking]\nLet me think about how to explain this.\n[/Thinking]\n\nHere is the explanation.", asst.Content)
}

func TestParseClineSession_ThinkingOnlyEnding(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000003_thinking"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000003_thinking",
		"status": "completed",
		"prompt": "think about it"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".json"), []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "m1",
				"role": "user",
				"content": [{"type": "text", "text": "think about it"}],
				"ts": 1000
			},
			{
				"id": "m2",
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "Still thinking..."
					}
				],
				"ts": 2000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, sessionID+".messages.json"), []byte(messagesJSON), 0o644))

	sess, messages, err := parseClineSession(filepath.Join(taskDir, sessionID+".json"), "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Interrupted mid-thought detected
	assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
	assert.Equal(t, "[Thinking]\nStill thinking...\n[/Thinking]", messages[1].Content)
	assert.Equal(t, "Still thinking...", messages[1].ThinkingText)
	assert.True(t, messages[1].HasThinking)
}

func TestParseClineSession_EmptyTranscript(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000004_empty"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000004_empty",
		"started_at": "2026-09-10T12:00:00.000Z",
		"prompt": "hello"
	}`
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	sess, msgs, err := parseClineSession(metaPath, "test-proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Empty(t, msgs)
	assert.Equal(t, "hello", sess.SessionName)
	assert.Equal(t, "hello", sess.FirstMessage)
}

func TestParseClineSession_ZeroCostAuthoritative(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000005_zerocost"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	zeroVal := jsontext.Value("0")
	meta := clineSessionMetadata{
		SessionID: sessionID,
		Model:     "ollama/llama3",
		Metadata: clineMetadataObj{
			TotalCost: &zeroVal,
			Usage: &clineUsageObj{
				InputTokens:  1000,
				OutputTokens: 200,
				TotalCost:    &zeroVal,
			},
		},
	}
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, data, 0o644))

	sess, _, err := parseClineSession(metaPath, "", "")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, sess.UsageEvents, 1)
	require.NotNil(t, sess.UsageEvents[0].Cost)
	assert.Equal(t, money.MustParseDollars("0"), *sess.UsageEvents[0].Cost)
}

func TestParseClineSession_MessageMetricsZeroCostAuthoritative(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000006_zerocost_msgs"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	zeroVal := jsontext.Value("0")
	meta := clineSessionMetadata{
		SessionID: sessionID,
		Model:     "ollama/llama3",
		Metadata: clineMetadataObj{
			TotalCost: &zeroVal,
			Usage: &clineUsageObj{
				InputTokens:  1000,
				OutputTokens: 200,
				TotalCost:    &zeroVal,
			},
		},
	}
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, data, 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "msg_001",
				"role": "user",
				"content": [{"type": "text", "text": "hello"}],
				"ts": 1789034400000
			},
			{
				"id": "msg_002",
				"role": "assistant",
				"content": [{"type": "text", "text": "world"}],
				"ts": 1789034402000,
				"metrics": {
					"inputTokens": 1000,
					"outputTokens": 200
				}
			}
		]
	}`
	messagesPath := filepath.Join(taskDir, sessionID+".messages.json")
	require.NoError(t, os.WriteFile(messagesPath, []byte(messagesJSON), 0o644))

	sess, msgs, err := parseClineSession(metaPath, "test-proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Equal(t, 200, sess.TotalOutputTokens)

	// Authoritative zero cost must be preserved on usage event with 0 tokens
	require.Len(t, sess.UsageEvents, 1)
	require.NotNil(t, sess.UsageEvents[0].Cost)
	assert.Equal(t, money.MustParseDollars("0"), *sess.UsageEvents[0].Cost)
	assert.Zero(t, sess.UsageEvents[0].InputTokens)
	assert.Zero(t, sess.UsageEvents[0].OutputTokens)
	assert.Zero(t, sess.UsageEvents[0].CacheReadInputTokens)
	assert.Zero(t, sess.UsageEvents[0].CacheCreationInputTokens)
}

func TestParseClineSession_ContentBlockToolResults(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000088_content_blocks"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"session_id": "1789000000088_content_blocks",
		"started_at": "2026-09-10T10:00:00Z",
		"status": "completed"
	}`
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"role": "user",
				"ts": 1789000000000,
				"content": [
					{
						"type": "text",
						"text": "Run inspection"
					}
				]
			},
			{
				"role": "assistant",
				"ts": 1789000001000,
				"content": [
					{
						"type": "tool_use",
						"id": "call_blk_1",
						"name": "read_files",
						"input": {"paths": ["main.go"]}
					}
				]
			},
			{
				"role": "user",
				"ts": 1789000002000,
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_blk_1",
						"content": [
							{"type": "text", "text": "package main\n\nfunc main() {}"}
						]
					}
				]
			}
		]
	}`
	messagesPath := filepath.Join(taskDir, sessionID+".messages.json")
	require.NoError(t, os.WriteFile(messagesPath, []byte(messagesJSON), 0o644))

	sess, msgs, err := parseClineSession(metaPath, "test-proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 3)

	// Verify tool call was paired with decoded result
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "call_blk_1", tc.ToolUseID)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "package main\n\nfunc main() {}", tc.ResultEvents[0].Content)

	// Verify tool result preserved raw JSON in ContentRaw and can be decoded
	require.Len(t, msgs[2].ToolResults, 1)
	tr := msgs[2].ToolResults[0]
	assert.Equal(t, "call_blk_1", tr.ToolUseID)
	assert.Equal(t, len("package main\n\nfunc main() {}"), tr.ContentLength)
	assert.JSONEq(t, `[{"type": "text", "text": "package main\n\nfunc main() {}"}]`, tr.ContentRaw)
	assert.Equal(t, "package main\n\nfunc main() {}", DecodeContent(tr.ContentRaw))
}

func TestParseClineSession_ProviderPropagation(t *testing.T) {
	dir := t.TempDir()
	sessionID := "1789000000001_provider"
	taskDir := filepath.Join(dir, sessionID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	metaJSON := `{
		"version": 1,
		"session_id": "1789000000001_provider",
		"provider": "openrouter",
		"model": "deepseek-chat",
		"started_at": "2026-09-10T10:00:00.000Z",
		"ended_at": "2026-09-10T10:05:00.000Z",
		"metadata": {
			"totalCost": 0.05,
			"usage": {
				"inputTokens": 1000,
				"outputTokens": 200,
				"totalCost": 0.05
			}
		}
	}`
	metaPath := filepath.Join(taskDir, sessionID+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	messagesJSON := `{
		"messages": [
			{
				"id": "msg_001",
				"role": "user",
				"content": [{"type": "text", "text": "Hello"}],
				"ts": 1789034400000
			},
			{
				"id": "msg_002",
				"role": "assistant",
				"content": [{"type": "text", "text": "Hi there"}],
				"ts": 1789034401000
			},
			{
				"id": "msg_003",
				"role": "assistant",
				"modelInfo": {
					"id": "claude-3-5-sonnet",
					"provider": "anthropic"
				},
				"content": [{"type": "text", "text": "Switched model"}],
				"ts": 1789034402000
			}
		]
	}`
	messagesPath := filepath.Join(taskDir, sessionID+".messages.json")
	require.NoError(t, os.WriteFile(messagesPath, []byte(messagesJSON), 0o644))

	sess, msgs, err := parseClineSession(metaPath, "test-proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 3)

	// User message should have empty ProviderID
	assert.Empty(t, msgs[0].ProviderID)

	// First assistant message should inherit session-level provider ("openrouter")
	assert.Equal(t, "openrouter", msgs[1].ProviderID)
	assert.Equal(t, "deepseek-chat", msgs[1].Model)

	// Second assistant message has per-message modelInfo overriding provider to "anthropic"
	assert.Equal(t, "anthropic", msgs[2].ProviderID)
	assert.Equal(t, "claude-3-5-sonnet", msgs[2].Model)

	// Aggregate usage event should carry session-level provider ("openrouter")
	require.NotEmpty(t, sess.UsageEvents)
	assert.Equal(t, "openrouter", sess.UsageEvents[0].ProviderID)
	assert.Equal(t, "deepseek-chat", sess.UsageEvents[0].Model)
}

func TestParseClineSession_CanonicalSessionIDValidation(t *testing.T) {
	dir := t.TempDir()

	// Case 1: Matching metadata session_id succeeds
	sess1Dir := filepath.Join(dir, "sess-canonical-1")
	require.NoError(t, os.MkdirAll(sess1Dir, 0o755))
	meta1 := filepath.Join(sess1Dir, "sess-canonical-1.json")
	require.NoError(t, os.WriteFile(meta1, []byte(`{"session_id":"sess-canonical-1"}`), 0o644))
	sess1, _, err := parseClineSession(meta1, "proj", "local")
	require.NoError(t, err)
	assert.Equal(t, "cline:sess-canonical-1", sess1.ID)

	// Case 2: Omitted metadata session_id falls back to canonical ID
	sess2Dir := filepath.Join(dir, "sess-canonical-2")
	require.NoError(t, os.MkdirAll(sess2Dir, 0o755))
	meta2 := filepath.Join(sess2Dir, "sess-canonical-2.json")
	require.NoError(t, os.WriteFile(meta2, []byte(`{}`), 0o644))
	sess2, _, err := parseClineSession(meta2, "proj", "local")
	require.NoError(t, err)
	assert.Equal(t, "cline:sess-canonical-2", sess2.ID)

	// Case 3: Mismatch between metadata session_id and directory name is rejected
	sess3Dir := filepath.Join(dir, "sess-canonical-3")
	require.NoError(t, os.MkdirAll(sess3Dir, 0o755))
	meta3 := filepath.Join(sess3Dir, "sess-canonical-3.json")
	require.NoError(t, os.WriteFile(meta3, []byte(`{"session_id":"stale-or-evil-id"}`), 0o644))
	_, _, err = parseClineSession(meta3, "proj", "local")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match canonical id")

	// Case 4: Mismatch between filename and directory name is rejected
	sess4Dir := filepath.Join(dir, "sess-canonical-4")
	require.NoError(t, os.MkdirAll(sess4Dir, 0o755))
	meta4 := filepath.Join(sess4Dir, "wrong-name.json")
	require.NoError(t, os.WriteFile(meta4, []byte(`{"session_id":"sess-canonical-4"}`), 0o644))
	_, _, err = parseClineSession(meta4, "proj", "local")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match directory")

	// Case 5: Malformed directory name (e.g. underscore prefix) is rejected
	sess5Dir := filepath.Join(dir, "_invalid_sess")
	require.NoError(t, os.MkdirAll(sess5Dir, 0o755))
	meta5 := filepath.Join(sess5Dir, "_invalid_sess.json")
	require.NoError(t, os.WriteFile(meta5, []byte(`{"session_id":"_invalid_sess"}`), 0o644))
	_, _, err = parseClineSession(meta5, "proj", "local")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cline session directory name")
}

func TestIsClineTeammateMessagesFile(t *testing.T) {
	sessionID := "1789000000000_mocksess"
	tests := []struct {
		filename string
		want     bool
	}{
		{"worker-scout__sub1abc.messages.json", true},
		{"diff-checker__sub2xyz.messages.json", true},
		{"worker__task1.messages.json", true},
		{"my_agent__123.messages.json", true},
		{"1789000000000_mocksess.messages.json", false},
		{"1789000000000_mocksess.json", false},
		{"other.json", false},
		{"worker-scout.messages.json", false},
		{"__sub1abc.messages.json", false},
		{"worker-scout__.messages.json", false},
		{".hidden__t1.messages.json", false},
		{"_agent__t1.messages.json", false},
		{"../evil__t1.messages.json", false},
		{"foo/bar__t1.messages.json", false},
		{"foo\\bar__t1.messages.json", false},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			assert.Equal(t, tt.want, IsClineTeammateMessagesFile(sessionID, tt.filename))
		})
	}
}

func TestParseClineSession_TeammateSubagents(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sess-parent")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaJSON := `{
		"session_id": "sess-parent",
		"started_at": "2026-09-10T10:00:00Z",
		"cwd": "/workspace/teamproject",
		"provider": "anthropic",
		"model": "claude-3-5-sonnet",
		"metadata": {
			"title": "Lead Coordinator",
			"git": { "branch": "feat/team-feature" }
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "sess-parent.json"), []byte(metaJSON), 0o644))

	parentMsgsJSON := `{
		"version": 1,
		"agent": "lead",
		"sessionId": "sess-parent",
		"messages": [
			{
				"id": "msg_user_1",
				"role": "user",
				"content": [
					{
						"type": "text",
						"text": "Spawn git-scout to inspect repository status"
					}
				],
				"ts": 1789207950000
			},
			{
				"id": "msg_asst_1",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_spawn_scout",
						"name": "team_spawn_teammate",
						"input": {
							"agentId": "git-scout",
							"rolePrompt": "You are a git recon specialist."
						}
					}
				],
				"ts": 1789207960000
			},
			{
				"id": "msg_user_2",
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_spawn_scout",
						"content": "{\"agentId\":\"git-scout\",\"status\":\"spawned\"}"
					}
				],
				"ts": 1789207961000
			},
			{
				"id": "msg_asst_2",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_run_scout",
						"name": "team_run_task",
						"input": {
							"agentId": "git-scout",
							"taskId": "task_0001",
							"runMode": "sync",
							"task": "Collect git status. taskId: task_0001"
						}
					}
				],
				"ts": 1789207970000
			},
			{
				"id": "msg_user_3",
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "call_run_scout",
						"content": "{\"agentId\":\"git-scout\",\"status\":\"completed\"}"
					}
				],
				"ts": 1789207980000
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "sess-parent.messages.json"), []byte(parentMsgsJSON), 0o644))

	teammateMsgsJSON := `{
		"version": 1,
		"updated_at": "2026-09-10T10:05:00Z",
		"agent": "teammate",
		"sessionId": "sess-parent__teamtask__git-scout__t1abc",
		"taskType": "team",
		"origin": {
			"source": "cli",
			"mode": "team",
			"sessionId": "sess-parent__teamtask__git-scout__t1abc",
			"parentThreadId": "sess-parent",
			"subagent": "git-scout",
			"version": "3.0.61"
		},
		"messages": [
			{
				"id": "msg_sub_user_1",
				"role": "user",
				"content": [
					{
						"type": "text",
						"text": "Collect git status. taskId: task_0001"
					}
				],
				"ts": 1789207971000
			},
			{
				"id": "msg_sub_asst_1",
				"role": "assistant",
				"content": [
					{
						"type": "thinking",
						"thinking": "Executing git commands"
					},
					{
						"type": "tool_use",
						"id": "call_sub_cmd_1",
						"name": "run_commands",
						"input": {
							"commands": ["git status --short", "git branch"]
						}
					}
				],
				"ts": 1789207975000,
				"modelInfo": {
					"id": "claude-3-5-haiku",
					"provider": "anthropic"
				},
				"metrics": {
					"inputTokens": 200,
					"outputTokens": 80,
					"cacheReadTokens": 50,
					"cacheWriteTokens": 0
				}
			}
		]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "git-scout__t1abc.messages.json"), []byte(teammateMsgsJSON), 0o644))

	metaPath := filepath.Join(sessDir, "sess-parent.json")

	// 1. Test parseClineSessionWithTeammates directly
	results, err := parseClineSessionWithTeammates(metaPath, "teamproject", "local")
	require.NoError(t, err)
	require.Len(t, results, 2)

	// Verify Parent
	parent := results[0]
	assert.Equal(t, "cline:sess-parent", parent.Session.ID)
	assert.Empty(t, parent.Session.ParentSessionID)
	assert.Equal(t, RelNone, parent.Session.RelationshipType)
	assert.Equal(t, "Lead Coordinator", parent.Session.SessionName)

	// Verify Tool Calls in Parent are annotated with child SubagentSessionID
	require.Len(t, parent.Messages, 5)
	spawnCall := parent.Messages[1].ToolCalls[0]
	assert.Empty(t, spawnCall.SubagentSessionID)
	assert.Equal(t, "Task", spawnCall.Category)

	runCall := parent.Messages[3].ToolCalls[0]
	assert.Equal(t, "cline:sess-parent__teamtask__git-scout__t1abc", runCall.SubagentSessionID)
	assert.Equal(t, "Task", runCall.Category)
	require.Len(t, runCall.ResultEvents, 1)
	assert.Equal(t, "cline:sess-parent__teamtask__git-scout__t1abc", runCall.ResultEvents[0].SubagentSessionID)
	assert.Equal(t, "git-scout", runCall.ResultEvents[0].AgentID)

	// Verify Child Subagent
	child := results[1]
	assert.Equal(t, "cline:sess-parent__teamtask__git-scout__t1abc", child.Session.ID)
	assert.Equal(t, "cline:sess-parent", child.Session.ParentSessionID)
	assert.Equal(t, RelSubagent, child.Session.RelationshipType)
	assert.Equal(t, "Teammate: git-scout", child.Session.SessionName)
	assert.Equal(t, "feat/team-feature", child.Session.GitBranch)
	assert.Equal(t, "teamproject", child.Session.Project)
	assert.Equal(t, "/workspace/teamproject", child.Session.Cwd)
	assert.Equal(t, AgentCline, child.Session.Agent)
	assert.Equal(t, 2, child.Session.MessageCount)
	assert.Equal(t, 1, child.Session.UserMessageCount)
	assert.Equal(t, "Collect git status. taskId: task_0001", child.Session.FirstMessage)
	assert.Equal(t, 80, child.Session.TotalOutputTokens)
	assert.Equal(t, 250, child.Session.PeakContextTokens)

	// Verify child messages
	require.Len(t, child.Messages, 2)
	assert.True(t, child.Messages[1].HasThinking)
	assert.Equal(t, "Executing git commands", child.Messages[1].ThinkingText)
	require.Len(t, child.Messages[1].ToolCalls, 1)
	assert.Equal(t, "run_commands", child.Messages[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Bash", child.Messages[1].ToolCalls[0].Category)

	// 2. Verify parseClineSession (backwards compatibility) returns parent with annotated tool calls
	sess, msgs, err := parseClineSession(metaPath, "teamproject", "local")
	require.NoError(t, err)
	assert.Equal(t, "cline:sess-parent", sess.ID)
	assert.Empty(t, msgs[1].ToolCalls[0].SubagentSessionID)
	assert.Equal(t, "cline:sess-parent__teamtask__git-scout__t1abc", msgs[3].ToolCalls[0].SubagentSessionID)
}
