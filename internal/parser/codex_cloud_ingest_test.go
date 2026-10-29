package parser_test

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"image/color"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// prepareCodexCloud parses a mirror file built from items and runs it
// through the ingest steps that pair and drop tool-result carriers.
func prepareCodexCloud(t *testing.T, items ...string) ingest.PreparedSession {
	t.Helper()
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	records := []mirror.Record{{
		Kind: mirror.KindThread, FetchedAt: fetched,
		Data: jsontext.Value(`{"id":"thread-a","createdAt":1,"updatedAt":2,"cwd":"/workspace/project-a"}`),
	}}
	for i, item := range items {
		started := strconv.FormatInt(1759320000000+int64(i)*1000, 10)
		records = append(records, mirror.Record{
			Kind: mirror.KindItem, FetchedAt: fetched,
			Data: jsontext.Value(`{"turnId":"turn-1","startedAtMs":` + started + `,"item":` + item + `}`),
		})
	}
	path := filepath.Join(t.TempDir(), "account-key", "thread-a.jsonl.zstd")
	_, err := mirror.Append(path, 0, records)
	require.NoError(t, err)
	parsed, ok, err := parser.ParseCodexCloudFile(t.Context(), path, "machine-a")
	require.NoError(t, err)
	require.True(t, ok)
	candidate, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{})
	require.NoError(t, err)
	prepared, err := ingest.Finalize(t.Context(), candidate, ingest.ContentOptions{})
	require.NoError(t, err)
	return prepared
}

func TestCodexCloudStoredCountsExcludeResultCarriers(t *testing.T) {
	prepared := prepareCodexCloud(t,
		`{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Run the tests"}]}`,
		`{"type":"commandExecution","id":"c1","command":"go test ./...","aggregatedOutput":"ok","status":"completed"}`,
		`{"type":"mcpToolCall","id":"m1","server":"github","tool":"search","result":"none","status":"completed"}`,
		`{"type":"agentMessage","id":"a1","text":"Tests pass."}`,
	)
	require.Len(t, prepared.Messages, 4, "user, two calls, and the reply are stored")
	assert.Equal(t, len(prepared.Messages), prepared.Session.MessageCount)
	assert.Equal(t, 1, prepared.Session.UserMessageCount)
}

func TestCodexCloudMultiFileChangeEveryFileGetsItsResult(t *testing.T) {
	prepared := prepareCodexCloud(t,
		`{"type":"fileChange","id":"f1","changes":[{"path":"/workspace/project-a/a.go"},{"path":"/workspace/project-a/b.go"}],"status":"failed"}`,
	)
	require.Len(t, prepared.Messages, 1)
	calls := prepared.Messages[0].ToolCalls
	require.Len(t, calls, 2)
	ids := map[string]bool{}
	for _, call := range calls {
		ids[call.ToolUseID] = true
		require.Len(t, call.ResultEvents, 1, call.FilePath)
		assert.Equal(t, "errored", call.ResultEvents[0].Status, call.FilePath)
	}
	assert.Len(t, ids, 2, "each file has its own tool-use ID")
	assert.Equal(t, "/workspace/project-a/a.go", calls[0].FilePath)
	assert.Equal(t, "/workspace/project-a/b.go", calls[1].FilePath)
}

func TestSessionTotalsCountEachIncreaseOnce(t *testing.T) {
	counts := func(input, output int64) mirror.TokenCounts {
		return mirror.TokenCounts{Input: input, Output: output, Total: input + output}
	}
	tests := []struct {
		name       string
		tokens     [][2]mirror.TokenCounts // total, last
		wantOutput int
		wantEvents []int // output tokens per stored event
	}{
		{
			// Every event is request-scoped, so the engine re-derives the
			// total from them; it must not add them to the provider's.
			name:       "only request-scoped increases",
			tokens:     [][2]mirror.TokenCounts{{counts(1000, 100), counts(1000, 100)}, {counts(1500, 150), counts(500, 50)}},
			wantOutput: 150,
			wantEvents: []int{100, 50},
		},
		{
			name: "a thread-level increase past 2,000,000 tokens",
			tokens: [][2]mirror.TokenCounts{
				{counts(1000, 100), counts(1000, 100)},
				{counts(3_001_000, 2_100_100), counts(1000, 0)},
			},
			wantOutput: 2_100_100,
			wantEvents: []int{100, 2_100_000},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			records := []mirror.Record{{
				Kind: mirror.KindThread, FetchedAt: fetched,
				Data: jsontext.Value(`{"id":"thread-a","createdAt":1,"updatedAt":2,"model":"gpt-6"}`),
			}}
			for i, pair := range tt.tokens {
				data, err := json.Marshal(mirror.TokenUsageData{
					JoinID: "join-a", TurnID: "turn-1", Total: pair[0], Last: pair[1],
					ObservedAt: fetched.Add(time.Duration(i) * time.Minute),
				})
				require.NoError(t, err)
				records = append(records, mirror.Record{Kind: mirror.KindTokenUsage, FetchedAt: fetched, Data: jsontext.Value(data)})
			}
			path := filepath.Join(t.TempDir(), "account-key", "thread-a.jsonl.zstd")
			_, err := mirror.Append(path, 0, records)
			require.NoError(t, err)
			parsed, ok, err := parser.ParseCodexCloudFile(t.Context(), path, "machine-a")
			require.NoError(t, err)
			require.True(t, ok)
			candidate, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{})
			require.NoError(t, err)
			prepared, err := ingest.Finalize(t.Context(), candidate, ingest.ContentOptions{})
			require.NoError(t, err)

			assert.Equal(t, tt.wantOutput, prepared.Session.TotalOutputTokens)
			assert.True(t, prepared.Session.HasTotalOutputTokens)
			assert.Equal(t, 1000, prepared.Session.PeakContextTokens)
			outputs := make([]int, len(prepared.UsageEvents))
			for i, ev := range prepared.UsageEvents {
				outputs[i] = ev.OutputTokens
			}
			assert.Equal(t, tt.wantEvents, outputs, "a session event is floored, never clamped")
		})
	}
}

func TestCodexCloudGeneratedImageBecomesImageResult(t *testing.T) {
	png := testjsonl.SolidPNGBase64(4, 3, color.RGBA{R: 200, G: 40, B: 90, A: 255})
	text := base64.StdEncoding.EncodeToString([]byte("plain text, not an image"))
	item := func(result string) string {
		return `{"type":"imageGeneration","id":"g1","status":"completed","revisedPrompt":"A red square on white","result":"` + result + `"}`
	}
	tests := []struct {
		name       string
		item       string
		wantInput  string
		wantResult string
	}{
		{
			name:       "png moves to the result",
			item:       item(png),
			wantInput:  `{"id":"g1","revisedPrompt":"A red square on white","status":"completed","type":"imageGeneration"}`,
			wantResult: `[{"image_url":"data:image/png;base64,` + png + `","type":"input_image"}]`,
		},
		{
			name:      "a payload that is not an image stays in the input",
			item:      item(text),
			wantInput: item(text),
		},
		{
			name:      "a payload that is not base64 stays in the input",
			item:      item("not base64!"),
			wantInput: item("not base64!"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared := prepareCodexCloud(t, tt.item)
			require.Len(t, prepared.Messages, 1)
			require.Len(t, prepared.Messages[0].ToolCalls, 1)
			call := prepared.Messages[0].ToolCalls[0]
			assert.Equal(t, "image_generation", call.ToolName)
			assert.Equal(t, tt.wantInput, call.InputJSON)
			assert.Equal(t, tt.wantResult, call.ResultContent)
		})
	}
}
