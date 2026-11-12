package sync_test

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// usageOnlyModel is priced on output tokens alone, so every format's
// expected cost is the same despite their different input conventions.
const usageOnlyModel = "usage-contract-model"

const (
	visibleOutputTokens   = 100
	usageOnlyOutputTokens = 200
	usageOnlyInputTokens  = 1_000
)

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, raw, 0o644))
}

func writeJSONLFile(t *testing.T, path string, records ...any) {
	t.Helper()
	var content []byte
	for _, record := range records {
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		content = append(append(content, raw...), '\n')
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

// usageOnlyCase is a native session with a user prompt, one visible reply,
// and, last, one assistant record that carries usage but no text or tool
// call.
type usageOnlyCase struct {
	name      string
	agent     parser.AgentType
	sessionID string
	write     func(t *testing.T, dir string)
}

func usageOnlyCases() []usageOnlyCase {
	return []usageOnlyCase{
		{
			name:      "opencode v1",
			agent:     parser.AgentOpenCode,
			sessionID: "opencode:ses_usage_only",
			write: func(t *testing.T, dir string) {
				t.Helper()
				oc := createOpenCodeDB(t, dir)
				oc.addProject(t, "prj_usage", "/workspace/project-a")
				oc.addSession(t, "ses_usage_only", "prj_usage", 1_000, 5_000)
				oc.addMessage(t, "msg_1", "ses_usage_only", "user", 1_000)
				oc.addTextPart(t, "prt_1", "ses_usage_only", "msg_1", "check the build", 1_000)
				oc.addMessage(t, "msg_2", "ses_usage_only", "assistant", 2_000)
				oc.updateMessageData(t, "msg_2", map[string]any{
					"role": "assistant", "modelID": usageOnlyModel,
					"tokens": map[string]any{"input": 10, "output": visibleOutputTokens},
				})
				oc.addTextPart(t, "prt_2", "ses_usage_only", "msg_2", "the build passes", 2_000)
				// A step that produced no text, reasoning text, or tool part.
				oc.addMessage(t, "msg_3", "ses_usage_only", "assistant", 3_000)
				oc.updateMessageData(t, "msg_3", map[string]any{
					"role": "assistant", "modelID": usageOnlyModel,
					"tokens": map[string]any{
						"input": usageOnlyInputTokens, "output": usageOnlyOutputTokens,
					},
				})
			},
		},
		{
			name:      "openclaw",
			agent:     parser.AgentOpenClaw,
			sessionID: "openclaw:main:usage-only",
			write:     writeClawUsageOnlySession,
		},
		{
			name:      "qclaw",
			agent:     parser.AgentQClaw,
			sessionID: "qclaw:main:usage-only",
			write:     writeClawUsageOnlySession,
		},
		{
			name:      "forge",
			agent:     parser.AgentForge,
			sessionID: "forge:usage-only",
			write: func(t *testing.T, dir string) {
				t.Helper()
				text := func(role, content string, completion int) map[string]any {
					return map[string]any{
						"message": map[string]any{"text": map[string]any{
							"role": role, "content": content, "model": usageOnlyModel,
							"timestamp": "2026-05-02T09:58:16Z",
						}},
						"usage": map[string]any{
							"prompt_tokens":     map[string]any{"actual": usageOnlyInputTokens},
							"completion_tokens": map[string]any{"actual": completion},
							"cached_tokens":     map[string]any{"actual": 0},
						},
					}
				}
				context, err := json.Marshal(map[string]any{
					"conversation_id": "usage-only",
					"messages": []map[string]any{
						text("User", "check the build", 0),
						text("Assistant", "the build passes", visibleOutputTokens),
						// A stop with no content, tool calls, or reasoning.
						text("Assistant", "", usageOnlyOutputTokens),
					},
				})
				require.NoError(t, err)
				forge := createForgeDB(t, dir)
				forge.addConversation(t, "usage-only", "Usage only", string(context),
					"2026-05-02 09:58:15", "2026-05-02 10:00:16", "")
			},
		},
		{
			name:      "cline",
			agent:     parser.AgentCline,
			sessionID: "cline:usage-only",
			write: func(t *testing.T, dir string) {
				t.Helper()
				sessionDir := filepath.Join(dir, "data", "sessions", "usage-only")
				writeJSONFile(t, filepath.Join(sessionDir, "usage-only.json"), map[string]any{
					"session_id": "usage-only", "cwd": "/workspace/project-a",
					"model": usageOnlyModel,
				})
				writeJSONFile(t, filepath.Join(sessionDir, "usage-only.messages.json"), map[string]any{
					"messages": []map[string]any{
						{
							"id": "c1", "role": "user", "ts": 1_000,
							"content": []map[string]any{{"type": "text", "text": "check the build"}},
						},
						{
							"id": "c2", "role": "assistant", "ts": 2_000,
							"content": []map[string]any{{"type": "text", "text": "the build passes"}},
							"metrics": map[string]any{"inputTokens": 10, "outputTokens": visibleOutputTokens},
						},
						// Provider-executed tool activity leaves no content block.
						{
							"id": "c3", "role": "assistant", "ts": 3_000,
							"content": []map[string]any{},
							"metrics": map[string]any{
								"inputTokens": usageOnlyInputTokens, "outputTokens": usageOnlyOutputTokens,
							},
						},
					},
				})
			},
		},
		{
			name:      "codex",
			agent:     parser.AgentCodex,
			sessionID: "codex:019f0000-0000-7000-8000-0000000a5e01",
			write:     writeCodexUsageOnlySession,
		},
		{
			name:      "open interpreter",
			agent:     parser.AgentInterpreter,
			sessionID: "open-interpreter:019f0000-0000-7000-8000-0000000a5e01",
			write:     writeCodexUsageOnlySession,
		},
		{
			name:      "gemini",
			agent:     parser.AgentGemini,
			sessionID: "gemini:usage-only",
			write: func(t *testing.T, dir string) {
				t.Helper()
				reply := func(id, ts, content string, output int) map[string]any {
					return map[string]any{
						"id": id, "timestamp": ts, "type": "gemini", "content": content,
						"model":  usageOnlyModel,
						"tokens": map[string]any{"input": usageOnlyInputTokens, "output": output},
					}
				}
				path := filepath.Join(dir, "tmp", "project", "chats", "session-usage-only.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(testjsonl.GeminiSessionJSON(
					"usage-only", "project", "2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
					[]map[string]any{
						testjsonl.GeminiUserMsg("u1", "2026-08-09T10:00:00Z", "check the build"),
						reply("g1", "2026-08-09T10:00:10Z", "the build passes", visibleOutputTokens),
						// A reply recorded with no content.
						reply("g2", "2026-08-09T10:00:20Z", "", usageOnlyOutputTokens),
					},
				)), 0o644))
			},
		},
	}
}

// writeClawUsageOnlySession writes an OpenClaw-format session, which QClaw
// also writes.
func writeClawUsageOnlySession(t *testing.T, dir string) {
	t.Helper()
	writeJSONLFile(t, filepath.Join(dir, "main", "sessions", "usage-only.jsonl"),
		map[string]any{
			"type": "session", "version": 3, "id": "usage-only",
			"timestamp": "2026-09-22T10:00:00Z", "cwd": "/workspace/project-a",
		},
		map[string]any{
			"type": "message", "id": "m1", "timestamp": "2026-09-22T10:00:01Z",
			"message": map[string]any{"role": "user", "content": "check the build"},
		},
		map[string]any{
			"type": "message", "id": "m2", "timestamp": "2026-09-22T10:00:02Z",
			"message": map[string]any{
				"role": "assistant", "model": usageOnlyModel,
				"content": []map[string]any{{"type": "text", "text": "the build passes"}},
				"usage":   map[string]any{"input": 10, "output": visibleOutputTokens},
			},
		},
		// A provider reply with no content blocks, such as an error stop.
		map[string]any{
			"type": "message", "id": "m3", "timestamp": "2026-09-22T10:00:03Z",
			"message": map[string]any{
				"role": "assistant", "model": usageOnlyModel,
				"content": []map[string]any{},
				"usage": map[string]any{
					"input": usageOnlyInputTokens, "output": usageOnlyOutputTokens,
				},
			},
		},
	)
}

// writeCodexUsageOnlySession writes a Codex rollout, which Open Interpreter
// also writes.
func writeCodexUsageOnlySession(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "2026", "09", "15",
		"rollout-2026-09-15T10-00-00-019f0000-0000-7000-8000-0000000a5e01.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("019f0000-0000-7000-8000-0000000a5e01",
			"/workspace/project-a", "user", "2026-09-15T10:00:00Z"),
		testjsonl.CodexTurnContextJSON(usageOnlyModel, "2026-09-15T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "check the build", "2026-09-15T10:00:01Z"),
		testjsonl.CodexMsgJSON("assistant", "the build passes", "2026-09-15T10:00:05Z"),
		testjsonl.CodexTokenCountJSON("2026-09-15T10:00:05Z", 10, visibleOutputTokens, 0),
		// A call with no row of its own, such as encrypted-only reasoning:
		// Codex counts it, but writes no message for it.
		testjsonl.CodexTokenCountJSON("2026-09-15T10:00:07Z",
			usageOnlyInputTokens, usageOnlyOutputTokens, 0),
	)), 0o644))
}

// TestUsageOnlyRecordsReachStoredUsage checks the ParsedMessage usage
// contract end to end: each parser's usage-only record reaches the archive
// and the cost as a row the transcript hides.
func TestUsageOnlyRecordsReachStoredUsage(t *testing.T) {
	for _, tc := range usageOnlyCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.write(t, dir)
			database := dbtest.OpenTestDB(t)
			require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
				ModelPattern:  usageOnlyModel,
				OutputPerMTok: money.MustParseDollars("10"),
			}}))
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{tc.agent: {dir}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			require.Equal(t, 1, stats.Synced)

			messages := fetchMessages(t, database, tc.sessionID)
			var hidden []db.Message
			for _, m := range messages {
				if m.Role == "assistant" && m.Content == "" && !m.HasToolUse {
					hidden = append(hidden, m)
				}
			}
			require.Len(t, hidden, 1, "the usage-only record is stored once")
			assert.True(t, hidden[0].IsSystem, "the transcript hides the usage-only row")
			assert.Equal(t, usageOnlyOutputTokens, hidden[0].OutputTokens)
			assert.Equal(t, usageOnlyModel, hidden[0].Model)

			usage, err := database.GetSessionUsage(t.Context(), tc.sessionID, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			output := 0
			for _, row := range usage.Breakdown {
				output += row.OutputTokens
			}
			assert.Equal(t, visibleOutputTokens+usageOnlyOutputTokens, output)
			require.True(t, usage.HasCost)
			// 300 output tokens at $10 per million.
			assert.Equal(t, money.MustParseDollars("0.003"), usage.Cost)
		})
	}
}

// TestUsageOnlyUpgradeRestoresUsageInPlace models an archive written before
// data version 175, which dropped the usage-only record, and checks that the
// scoped upgrade reparses the source in place: the usage returns, and the
// session and a pinned message keep their identities.
func TestUsageOnlyUpgradeRestoresUsageInPlace(t *testing.T) {
	for _, tc := range usageOnlyCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			tc.write(t, dir)
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.OpenIsolated(ctx, archivePath)
			require.NoError(t, err)
			cfg := sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{tc.agent: {dir}},
				Machine:   "local",
			}
			engine := sync.NewEngine(ctx, database, cfg)
			require.Zero(t, engine.SyncAll(ctx, nil).Failed)
			before := fetchMessages(t, database, tc.sessionID)
			require.Len(t, before, 3)
			_, err = database.PinMessage(ctx, tc.sessionID, before[1].ID, new("saved reply"))
			require.NoError(t, err)
			engine.Close()
			require.NoError(t, database.Close())

			raw, err := sql.Open("sqlite3", archivePath)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, `
				DELETE FROM messages WHERE session_id = ? AND ordinal = 2;
				UPDATE sessions SET message_count = 2, data_version = 174 WHERE id = ?;
				PRAGMA user_version = 174`, tc.sessionID, tc.sessionID)
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			reopened, err := db.OpenIsolated(ctx, archivePath)
			require.NoError(t, err)
			defer func() { require.NoError(t, reopened.Close()) }()
			assert.False(t, reopened.NeedsResync(), "the archive upgrades in place")
			upgraded := sync.NewEngine(ctx, reopened, cfg)
			defer upgraded.Close()
			stats := upgraded.SyncAll(ctx, nil)
			require.Zero(t, stats.Failed)
			assert.Positive(t, stats.Synced)

			after := fetchMessages(t, reopened, tc.sessionID)
			require.Len(t, after, 3)
			assert.True(t, after[2].IsSystem)
			assert.Equal(t, usageOnlyOutputTokens, after[2].OutputTokens)
			assert.Equal(t, before[1].ID, after[1].ID)
			pins, err := reopened.ListPinnedMessages(ctx, tc.sessionID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, after[1].ID, pins[0].MessageID)
		})
	}
}
