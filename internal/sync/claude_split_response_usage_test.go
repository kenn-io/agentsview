package sync_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// Claude Agent SDK transcripts omit requestId. A tool result can separate
// blocks of one response, which then repeat its usage on rows that the parser
// cannot coalesce; that usage must still count once.
func TestClaudeSplitResponseUsageCountsOnce(t *testing.T) {
	type record struct {
		kind, messageID, requestID string
		output                     int
	}
	split := func(requestID string) []record {
		return []record{
			{kind: "prompt"},
			{kind: "thinking", messageID: "msg_split", requestID: requestID, output: 460},
			{kind: "tool_a", messageID: "msg_split", requestID: requestID, output: 460},
			{kind: "result"},
			{kind: "tool_b", messageID: "msg_split", requestID: requestID, output: 460},
			{kind: "text", messageID: "msg_final", requestID: strings.ToUpper(requestID), output: 5},
		}
	}
	for _, tc := range []struct {
		name    string
		records []record
	}{
		{name: "separated blocks without requestId", records: split("")},
		{name: "separated blocks with requestId", records: split("req_split")},
		{name: "adjacent blocks without requestId", records: []record{
			{kind: "prompt"},
			{kind: "tool_a", messageID: "msg_split", output: 460},
			{kind: "tool_b", messageID: "msg_split", output: 460},
			{kind: "text", messageID: "msg_final", output: 5},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			const sessionID = "b2000000-0000-4000-8000-000000000101"
			var lines []string
			parent := ""
			for i, r := range tc.records {
				uuid := "c3000000-0000-4000-8000-00010100000" + string(rune('0'+i))
				line := map[string]any{
					"sessionId": sessionID, "cwd": "/synthetic/project",
					"isSidechain": false, "uuid": uuid,
					"timestamp": "2026-01-10T11:00:0" + string(rune('0'+i)) + "Z",
				}
				if parent != "" {
					line["parentUuid"] = parent
				}
				parent = uuid
				switch r.kind {
				case "prompt":
					line["type"] = "user"
					line["message"] = map[string]any{"role": "user", "content": "Synthetic prompt"}
				case "result":
					line["type"] = "user"
					line["message"] = map[string]any{"role": "user", "content": []any{map[string]any{
						"type": "tool_result", "tool_use_id": "toolu_a", "content": "Synthetic",
					}}}
				default:
					var block map[string]any
					switch r.kind {
					case "thinking":
						block = map[string]any{"type": "thinking", "thinking": "Synthetic", "signature": "sig"}
					case "tool_a", "tool_b":
						id := "toolu_" + strings.TrimPrefix(r.kind, "tool_")
						block = map[string]any{
							"type": "tool_use", "id": id, "name": "Synthetic",
							"input": map[string]any{"text": id},
						}
					default:
						block = map[string]any{"type": "text", "text": "Synthetic"}
					}
					cacheCreation, cacheRead, input := 100, 1000, 2
					if r.messageID == "msg_final" {
						cacheCreation, cacheRead, input = 0, 10, 1
					}
					line["type"] = "assistant"
					if r.requestID != "" {
						line["requestId"] = r.requestID
					}
					line["message"] = map[string]any{
						"role": "assistant", "id": r.messageID, "model": "claude-sonnet-4-6",
						"content": []any{block},
						"usage": map[string]any{
							"input_tokens": input, "output_tokens": r.output,
							"cache_creation_input_tokens": cacheCreation,
							"cache_read_input_tokens":     cacheRead,
						},
					}
				}
				encoded, err := json.Marshal(line)
				require.NoError(t, err)
				lines = append(lines, string(encoded))
			}

			root := t.TempDir()
			projectDir := filepath.Join(root, "synthetic-project")
			require.NoError(t, os.MkdirAll(projectDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"),
				[]byte(strings.Join(lines, "\n")+"\n"), 0o600))
			database, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, database.Close()) }()
			engine := sync.NewEngine(ctx, database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local",
			})
			defer engine.Close()
			require.Zero(t, engine.SyncAll(ctx, nil).Failed)

			usage, err := database.GetSessionUsage(ctx, sessionID, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, 465, usage.TotalOutputTokens)
			var input, cacheCreation, cacheRead int
			for _, row := range usage.Breakdown {
				input += row.InputTokens
				cacheCreation += row.CacheCreationInputTokens
				cacheRead += row.CacheReadInputTokens
			}
			assert.Equal(t, 3, input)
			assert.Equal(t, 100, cacheCreation)
			assert.Equal(t, 1010, cacheRead)

			daily, err := database.GetDailyUsage(ctx, db.UsageFilter{
				From: "2026-01-10", To: "2026-01-10", Timezone: "UTC",
			})
			require.NoError(t, err)
			assert.Equal(t, 3, daily.Totals.InputTokens)
			assert.Equal(t, 465, daily.Totals.OutputTokens)
			assert.Equal(t, 100, daily.Totals.CacheCreationTokens)
			assert.Equal(t, 1010, daily.Totals.CacheReadTokens)
		})
	}
}
