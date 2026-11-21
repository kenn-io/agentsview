package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestCallOutcomes(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	for _, tc := range []struct {
		name    string
		args    map[string]any
		isError bool
	}{
		{ToolGetSessionOverview, map[string]any{"session_id": "private_missing_id"}, true},
		{ToolSearchSessions, map[string]any{"query": "private_unmatched_query"}, false},
		{ToolSearchDocs, map[string]any{"query": "session", "limit": 1, "max_chars": 8000}, false},
		{ToolSearchContent, map[string]any{"pattern": "unmatched", "mode": "substring"}, false},
		{ToolQueryRecall, map[string]any{"query": "unmatched"}, false},
		{ToolSearchSessions, map[string]any{"query": "private_query", "include_one_shot": true}, true},
	} {
		result, err := ct.CallTool(t.Context(), callParams(tc.name, tc.args))
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, tc.isError, result.IsError)
	}
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 5)
	assert.Equal(t, ToolSearchSessions, rows[0].Tool)
	assert.Equal(t, 2, rows[0].Calls)
	assert.InDelta(t, 50.0, rows[0].ErrorPercent, 0.001)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 50.0, *rows[0].EmptyPercent, 0.001)
	assert.Equal(t, []CallCount{{Name: "include_one_shot", Count: 1}}, rows[0].Rejected)
	assert.Equal(t, ToolGetSessionOverview, rows[1].Tool)
	assert.InDelta(t, 100.0, rows[1].ErrorPercent, 0.001)
	for _, row := range rows[2:] {
		require.NotNil(t, row.EmptyPercent)
		if row.Tool == ToolSearchDocs {
			assert.InDelta(t, 0.0, *row.EmptyPercent, 0.001)
		} else {
			assert.InDelta(t, 100.0, *row.EmptyPercent, 0.001, row.Tool)
		}
	}
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	var outcomes []string
	for line := range bytes.Lines(raw) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec callRecord
		require.NoError(t, json.Unmarshal(line, &rec))
		outcomes = append(outcomes, rec.Outcome)
	}
	assert.Equal(t, []string{"error", "empty", "ok", "empty", "empty", "error"}, outcomes)
}

func TestSummarizeCallsDatesAndMedians(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	day2 := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, day1+".jsonl"), []byte(`{"tool":"search_docs","outcome":"ok","bytes":10,"ms":2}
{"tool":"search_docs","outcome":"empty","empty":true,"bytes":30,"ms":6}
broken
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day2+".jsonl"), []byte(`{"tool":"search_docs","outcome":"ok","bytes":90,"ms":20}
`), 0o600))
	rows, err := SummarizeCalls(dir, day1, day1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Calls)
	assert.Equal(t, 20, rows[0].MedianBytes)
	assert.Equal(t, 4, rows[0].MedianMs)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 50.0, *rows[0].EmptyPercent, 0.001)
	rows, err = SummarizeCalls(dir, day1, day2)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 30, rows[0].MedianBytes)
}

func TestCallLogRejectedNames(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	for _, args := range []map[string]any{
		{"query": "unmatched-search"},
		{"query": "secret-value", "odd name": "secret-value", `quote"name`: true},
		{"query": "term", "cursor": `unexpected additional properties ["cursor"]`},
		{"query": 123},
		{"query": "secret-value", "limit": "bad"},
		{"query": "secret-value", "limit": "bad", "include_one_shot": true},
	} {
		_, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
	}
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 6, rows[0].Calls)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 20.0, *rows[0].EmptyPercent, 0.001)
	assert.Equal(t, []CallCount{{Name: "include_one_shot", Count: 1}, {Name: "odd name", Count: 1}, {Name: `quote"name`, Count: 1}}, rows[0].Rejected)
}

func TestCallLogWriteFailureWarnsOnce(t *testing.T) {
	callLogWarning = sync.Once{}
	dir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(dir, []byte("file"), 0o600))
	d := dbtest.OpenTestDB(t)
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	oldStderr := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = oldStderr; _ = reader.Close(); _ = writer.Close() })
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: filepath.Join(dir, "calls")}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	for range 2 {
		result, err := ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
		require.NoError(t, err)
		require.False(t, result.IsError)
	}
	require.NoError(t, writer.Close())
	warnings, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(warnings), "MCP call log"), "warnings: %s", warnings)
}

func TestRecordToolCallErrorOutcome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "calls")
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: ToolSearchDocs}}
	result := &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"results": []any{}, "truncated": true}}
	require.NoError(t, recordToolCall(dir, req, result, context.Canceled, time.Second, nil))
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ToolSearchDocs, rows[0].Tool)
	assert.InDelta(t, 100.0, rows[0].ErrorPercent, 0.001)
}

func TestCallLogArgumentNames(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	longName := strings.Repeat("z", 70000)
	args := map[string]any{"query": "private-value", "odd name!": "private-value", longName: true}
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
	require.NoError(t, err)
	require.True(t, result.IsError)
	_, err = ct.CallTool(t.Context(), callParams(strings.Repeat("z", 500), nil))
	require.Error(t, err)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var records []callRecord
	for _, line := range lines {
		if len(line) > 0 {
			var rec callRecord
			require.NoError(t, json.Unmarshal(line, &rec))
			records = append(records, rec)
		}
	}
	require.Len(t, records, 2)
	assert.ElementsMatch(t, []string{"odd name!", "query", longName}, records[0].Args)
	assert.Equal(t, strings.Repeat("z", 500), records[1].Tool)
	assert.NotContains(t, string(raw), "private-value")
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestCallLogListSessionsAndTruncatedMessages(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "a", "project", dbtest.WithMessageCounts(4, 2))
	dbtest.SeedSession(t, d, "b", "project", dbtest.WithMessageCounts(4, 2))
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg("a", 0, strings.Repeat("x", 50))}))
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	page, err := ct.CallTool(t.Context(), callParams(ToolListSessions, map[string]any{"limit": 1}))
	require.NoError(t, err)
	require.False(t, page.IsError)
	var out listSessionsOut
	b, err := json.Marshal(page.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &out))
	require.NotEmpty(t, out.NextCursor)
	for _, args := range []map[string]any{
		{"project": "missing"},
		{"project": "missing", "cursor": out.NextCursor},
	} {
		result, err := ct.CallTool(t.Context(), callParams(ToolListSessions, args))
		require.NoError(t, err)
		require.False(t, result.IsError)
	}
	for _, limit := range []int{10, 100} {
		result, err := ct.CallTool(t.Context(), callParams(ToolGetMessages, map[string]any{"session_id": "a", "max_chars_per_message": limit}))
		require.NoError(t, err)
		require.False(t, result.IsError)
	}
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, ToolListSessions, rows[0].Tool)
	assert.Equal(t, 3, rows[0].Calls)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 50.0, *rows[0].EmptyPercent, 0.001)
	assert.Equal(t, ToolGetMessages, rows[1].Tool)
	assert.InDelta(t, 50.0, rows[1].TruncatedPercent, 0.001)
}

func TestCallLogEmptyAndTruncated(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	svc := &fakeContentSearchService{SessionService: service.NewDirectBackend(d, nil), result: &service.ContentSearchResult{CandidateTruncated: true}}
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: svc, CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchContent, map[string]any{"pattern": "secret", "activity_cutoff": "bad-date"}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	result, err = ct.CallTool(t.Context(), callParams(ToolSearchContent, map[string]any{"concepts": []string{"secret", "other concept"}, "include_active": true}))
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Rejected)
	assert.InDelta(t, 50.0, rows[0].TruncatedPercent, 0.001)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 50.0, *rows[0].EmptyPercent, 0.001)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret")
	assert.NotContains(t, string(raw), "bad-date")
	assert.NotContains(t, string(raw), "invalid activity_cutoff")
	var outcomes []string
	for line := range bytes.Lines(raw) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec callRecord
		require.NoError(t, json.Unmarshal(line, &rec))
		outcomes = append(outcomes, rec.Outcome)
	}
	assert.Equal(t, []string{"error", "truncated"}, outcomes)
}

func TestCallLogCursors(t *testing.T) {
	for _, tc := range []struct {
		cursor any
		want   bool
	}{{0, false}, {3, true}, {-1, false}, {"", false}, {"next", true}} {
		dir := t.TempDir()
		args, err := json.Marshal(map[string]any{"cursor": tc.cursor})
		require.NoError(t, err)
		name := ToolListSessions
		if _, ok := tc.cursor.(int); ok {
			name = ToolSearchSessions
		}
		req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name, Arguments: args}}
		require.NoError(t, recordToolCall(dir, req, nil, nil, 0, nil))
		files, err := os.ReadDir(dir)
		require.NoError(t, err)
		raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
		require.NoError(t, err)
		var rec callRecord
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &rec))
		assert.Equal(t, tc.want, rec.Continuation)
	}
}

func TestCallLogKeepsOldAndPartialRecords(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().AddDate(0, 0, -91).Format("2006-01-02")
	rec := `{"tool":"search_docs","outcome":"empty","empty":true}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, old+".jsonl"), []byte(rec), 0o600))
	today := time.Now().UTC().Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, today+".jsonl"), []byte(`{"tool":`), 0o600))
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	rows, err := SummarizeCalls(dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ToolSearchDocs, rows[0].Tool)
	assert.Equal(t, 2, rows[0].Calls)
}
