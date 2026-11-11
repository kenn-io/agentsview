package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func fidelityLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func fidelityReasoning(t *testing.T, ts, summary string) string {
	t.Helper()
	return fidelityLine(t, map[string]any{"type": "response_item", "timestamp": ts, "payload": map[string]any{
		"type":              "reasoning",
		"summary":           []any{map[string]any{"type": "summary_text", "text": summary}},
		"encrypted_content": "gAAAAopaque",
	}})
}

func fidelityCompacted(t *testing.T, ts, message string) string {
	t.Helper()
	return fidelityLine(t, map[string]any{"type": "compacted", "timestamp": ts, "payload": map[string]any{
		"message": message, "replacement_history": []any{},
	}})
}

// fidelityTokenCount writes one call's usage with the session's running total.
func fidelityTokenCount(t *testing.T, ts string, input, output, cached, call int) string {
	t.Helper()
	usage := func(n int) map[string]int {
		return map[string]int{
			"input_tokens": input * n, "cached_input_tokens": cached * n, "output_tokens": output * n,
			"reasoning_output_tokens": 0, "total_tokens": (input + output) * n,
		}
	}
	return fidelityLine(t, map[string]any{"type": "event_msg", "timestamp": ts, "payload": map[string]any{
		"type": "token_count",
		"info": map[string]any{"last_token_usage": usage(1), "total_token_usage": usage(call)},
	}})
}

func syncFidelityRollout(t *testing.T, uuid, body string) *db.DB {
	t.Helper()
	root := t.TempDir()
	day := filepath.Join(root, "2026", "11", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-2026-11-01T10-00-00-"+uuid+".jsonl"), []byte(body), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		Machine: "local", Ephemeral: true,
		AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	return database
}

// Two calls with equal per-call counts are billed twice, and a response that
// produced only reasoning keeps its usage, all the way to stored cost.
func TestCodexFidelityUsageReachesCost(t *testing.T) {
	const model = "gpt-5.4"
	head := func(uuid string) []string {
		return []string{
			testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2026-11-01T10:00:00Z"),
			testjsonl.CodexTurnContextJSON(model, "2026-11-01T10:00:01Z"),
			testjsonl.CodexMsgJSON("user", "run it", "2026-11-01T10:00:01Z"),
		}
	}
	const one = "019f0000-0000-7000-8000-000000000001"
	single := syncFidelityRollout(t, one, testjsonl.JoinJSONL(append(head(one),
		testjsonl.CodexMsgJSON("assistant", "done", "2026-11-01T10:00:02Z"),
		fidelityTokenCount(t, "2026-11-01T10:00:02Z", 10000, 400, 6000, 1),
	)...))
	base, err := single.GetSessionUsage(t.Context(), "codex:"+one, true)
	require.NoError(t, err)
	require.NotNil(t, base)
	require.True(t, base.HasCost, "fixture model must be priced")
	require.Positive(t, base.Cost.Microdollars)

	for _, tc := range []struct {
		name string
		tail []string
	}{
		{name: "equal per-call counts on distinct calls", tail: []string{
			testjsonl.CodexFunctionCallJSON("exec_command", "ls", "2026-11-01T10:00:02Z"),
			fidelityTokenCount(t, "2026-11-01T10:00:02Z", 10000, 400, 6000, 1),
			// A repeated observation of the same call still counts once.
			fidelityTokenCount(t, "2026-11-01T10:00:02Z", 10000, 400, 6000, 1),
			testjsonl.CodexMsgJSON("assistant", "listed", "2026-11-01T10:00:03Z"),
			fidelityTokenCount(t, "2026-11-01T10:00:03Z", 10000, 400, 6000, 2),
		}},
		{name: "reasoning-only second response", tail: []string{
			testjsonl.CodexMsgJSON("assistant", "first", "2026-11-01T10:00:02Z"),
			fidelityTokenCount(t, "2026-11-01T10:00:02Z", 10000, 400, 6000, 1),
			fidelityReasoning(t, "2026-11-01T10:00:03Z", "Nothing left to change."),
			fidelityTokenCount(t, "2026-11-01T10:00:03Z", 10000, 400, 6000, 2),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const two = "019f0000-0000-7000-8000-000000000002"
			database := syncFidelityRollout(t, two, testjsonl.JoinJSONL(append(head(two), tc.tail...)...))
			usage, err := database.GetSessionUsage(t.Context(), "codex:"+two, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, 2*base.TotalOutputTokens, usage.TotalOutputTokens)
			assert.True(t, usage.HasCost)
			assert.Equal(t, 2*base.Cost.Microdollars, usage.Cost.Microdollars)
		})
	}
}

// Appended reasoning and compaction rows, and the usage that follows them,
// match a single full parse whether the tail is read incrementally or not.
func TestCodexFidelityAppendMatchesFullParse(t *testing.T) {
	const uuid = "019f0000-0000-7000-8000-000000000003"
	const summary = "Another language model started to solve this problem.\n## Progress\nRead the parser."
	lines := []string{
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2026-11-01T10:00:00Z"),
		testjsonl.CodexTurnContextJSON("gpt-5.4", "2026-11-01T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "fix it", "2026-11-01T10:00:01Z"),
		fidelityReasoning(t, "2026-11-01T10:00:02Z", "Looking at the parser."),
		testjsonl.CodexMsgJSON("assistant", "checking", "2026-11-01T10:00:02Z"),
		fidelityTokenCount(t, "2026-11-01T10:00:02Z", 1000, 50, 400, 1),
		// A call with no row of its own: neither the earlier reasoning row
		// nor the later boundary may take it, so it becomes a hidden
		// usage-only row.
		fidelityTokenCount(t, "2026-11-01T10:00:03Z", 1100, 20, 900, 2),
		fidelityCompacted(t, "2026-11-01T10:00:04Z", summary),
		fidelityTokenCount(t, "2026-11-01T10:00:04Z", 1200, 30, 1000, 3),
		fidelityReasoning(t, "2026-11-01T10:00:05Z", "Done; nothing else."),
		fidelityTokenCount(t, "2026-11-01T10:00:05Z", 1300, 40, 1100, 4),
		// Remote compaction writes no summary; its boundary stays empty.
		fidelityCompacted(t, "2026-11-01T10:00:05Z", ""),
		testjsonl.CodexMsgJSON("user", "thanks", "2026-11-01T10:00:06Z"),
	}
	type row struct {
		Role, Content, Usage       string
		Thinking, Boundary, System bool
	}
	want := []row{
		{Role: "user", Content: "fix it"},
		{Role: "assistant", Content: "[Thinking]\nLooking at the parser.\n[/Thinking]", Thinking: true},
		{Role: "assistant", Content: "checking", Usage: `{"cache_read_input_tokens":400,"input_tokens":600,"output_tokens":50}`},
		{Role: "assistant", Usage: `{"cache_read_input_tokens":900,"input_tokens":200,"output_tokens":20}`, System: true},
		{Role: "assistant", Content: summary, Boundary: true, System: true},
		{Role: "assistant", Usage: `{"cache_read_input_tokens":1000,"input_tokens":200,"output_tokens":30}`, System: true},
		{Role: "assistant", Content: "[Thinking]\nDone; nothing else.\n[/Thinking]", Thinking: true, Usage: `{"cache_read_input_tokens":1100,"input_tokens":200,"output_tokens":40}`},
		{Role: "assistant", Boundary: true, System: true},
		{Role: "user", Content: "thanks"},
	}
	for split := 4; split < len(lines); split++ {
		for _, threshold := range []int64{1, 1 << 30} {
			t.Run("split="+strconv.Itoa(split)+"/threshold="+strconv.FormatInt(threshold, 10), func(t *testing.T) {
				root := t.TempDir()
				day := filepath.Join(root, "2026", "11", "01")
				require.NoError(t, os.MkdirAll(day, 0o755))
				path := filepath.Join(day, "rollout-2026-11-01T10-00-00-"+uuid+".jsonl")
				require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(lines[:split]...)), 0o600))
				database := openTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{
					Machine: "local", Ephemeral: true,
					AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
					StagedCodexParseMinBytes:          threshold,
					DisableFilesystemProjectDiscovery: true,
				})
				t.Cleanup(engine.Close)
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				appendFile(t, path, testjsonl.JoinJSONL(lines[split:]...))
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))

				messages, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
				require.NoError(t, err)
				got := make([]row, 0, len(messages))
				for _, m := range messages {
					got = append(got, row{
						Role: m.Role, Content: m.Content, Usage: string(m.TokenUsage),
						Thinking: m.HasThinking, Boundary: m.IsCompactBoundary,
						System: m.IsSystem,
					})
				}
				assert.Equal(t, want, got)
			})
		}
	}
}
