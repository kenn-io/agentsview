package parser_test

import (
	"encoding/json/jsontext"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
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
