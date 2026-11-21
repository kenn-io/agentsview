package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
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
		{ToolSearchDocs, map[string]any{"query": "session"}, false},
		{ToolSearchSessions, map[string]any{"query": "private_query", "include_one_shot": true}, true},
	} {
		result, err := ct.CallTool(t.Context(), callParams(tc.name, tc.args))
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, tc.isError, result.IsError)
	}
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, ToolSearchSessions, rows[0].Tool)
	assert.Equal(t, 2, rows[0].Calls)
	assert.Equal(t, 50.0, rows[0].ErrorPercent)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.Equal(t, 50.0, *rows[0].EmptyPercent)
	assert.Equal(t, []CallCount{{Name: "include_one_shot", Count: 1}}, rows[0].Rejected)
	require.NotNil(t, rows[2].EmptyPercent)
	assert.Equal(t, 0.0, *rows[2].EmptyPercent)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "private_missing_id")
	assert.NotContains(t, string(raw), "private_unmatched_query")
	assert.NotContains(t, string(raw), "private_query")
}

func TestSummarizeCallsDatesAndMedians(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	day2 := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, day1+".jsonl"), []byte(`{"tool":"search_docs","outcome":"ok","bytes":10,"ms":2}
{"tool":"search_docs","outcome":"empty","empty":true,"bytes":30,"ms":6}
broken
`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day2+".jsonl"), []byte(`{"tool":"search_docs","outcome":"ok","bytes":90,"ms":20}
`), 0600))
	rows, err := SummarizeCalls(t.Context(), dir, day1, day1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Calls)
	assert.Equal(t, 20, rows[0].MedianBytes)
	assert.Equal(t, 4, rows[0].MedianMs)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.Equal(t, 50.0, *rows[0].EmptyPercent)
	rows, err = SummarizeCalls(t.Context(), dir, day1, day2)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 30, rows[0].MedianBytes)
}

func TestCallLogPaginationAndRejectedNames(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	for _, args := range []map[string]any{
		{"query": "unmatched-search"},
		{"query": "unmatched-search", "cursor": 3},
		{"query": "unmatched-search", "cursor": -1},
		{"query": "secret-value", "odd name": "secret-value", `quote"name`: true},
		{"query": 123},
		{"query": "secret-value", "limit": "bad"},
	} {
		_, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
	}
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 6, rows[0].Calls)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 40.0, *rows[0].EmptyPercent, 0.001)
	assert.Equal(t, []CallCount{{Name: "odd name", Count: 1}, {Name: `quote"name`, Count: 1}}, rows[0].Rejected)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-value")
	assert.NotContains(t, string(raw), "validating")
}

func TestCallLogWriteFailureWarnsOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(dir, []byte("file"), 0600))
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

func TestCallLogUnknownTool(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	_, err := ct.CallTool(t.Context(), callParams("future_tool", nil))
	require.Error(t, err)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "future_tool", rows[0].Tool)
}

func TestSummarizeCallsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := SummarizeCalls(ctx, t.TempDir(), "", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestRecordToolCallCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir := filepath.Join(t.TempDir(), "calls")
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: ToolSearchDocs}}
	require.NoError(t, recordToolCall(ctx, dir, req, nil, context.Canceled, time.Second))
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ToolSearchDocs, rows[0].Tool)
	assert.Equal(t, 100.0, rows[0].ErrorPercent)
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
	assert.Equal(t, []string{"odd name!", "query", longName}, records[0].Args)
	assert.Equal(t, strings.Repeat("z", 500), records[1].Tool)
	assert.NotContains(t, string(raw), "private-value")
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestCallLogEmptySearchTools(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{ToolSearchContent, map[string]any{"pattern": "unmatched", "mode": "substring"}},
		{ToolQueryRecall, map[string]any{"query": "unmatched"}},
	} {
		result, err := ct.CallTool(t.Context(), callParams(tc.name, tc.args))
		require.NoError(t, err)
		require.False(t, result.IsError, "%s: %+v", tc.name, result.Content)
	}
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.NotNil(t, row.EmptyPercent)
		assert.Equal(t, 100.0, *row.EmptyPercent, row.Tool)
	}
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
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, ToolListSessions, rows[0].Tool)
	assert.Equal(t, 3, rows[0].Calls)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.Equal(t, 50.0, *rows[0].EmptyPercent)
	assert.Equal(t, ToolGetMessages, rows[1].Tool)
	assert.Equal(t, 50.0, rows[1].TruncatedPercent)
}

func TestCallLogConceptSearch(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	svc := &fakeContentSearchService{SessionService: service.NewDirectBackend(d, nil), result: &service.ContentSearchResult{CandidateTruncated: true}}
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: svc, CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	for range 2 {
		result, err := ct.CallTool(t.Context(), callParams(ToolSearchContent, map[string]any{"pattern": "secret", "activity_cutoff": "bad-date"}))
		require.NoError(t, err)
		require.True(t, result.IsError)
	}
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchContent, map[string]any{"concepts": []string{"secret", "other concept"}, "include_active": true}))
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Rejected)
	assert.InDelta(t, 100.0/3, rows[0].TruncatedPercent, 0.001)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 100.0/3, *rows[0].EmptyPercent, 0.001)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret")
	assert.NotContains(t, string(raw), "bad-date")
	assert.NotContains(t, string(raw), "invalid activity_cutoff")
}

func TestCallLogCursors(t *testing.T) {
	for _, tc := range []struct {
		cursor any
		want   bool
	}{{0, false}, {3, true}, {-1, false}, {"", false}, {"next", true}} {
		dir := t.TempDir()
		args, err := json.Marshal(map[string]any{"cursor": tc.cursor})
		require.NoError(t, err)
		req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: ToolListSessions, Arguments: args}}
		require.NoError(t, recordToolCall(t.Context(), dir, req, nil, nil, 0))
		files, err := os.ReadDir(dir)
		require.NoError(t, err)
		raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
		require.NoError(t, err)
		var rec callRecord
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &rec))
		assert.Equal(t, tc.want, rec.Continuation)
	}
}

func TestCallLogValidationValueIsNotARejectedName(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{
		"query": "term", "cursor": `unexpected additional properties ["cursor"]`,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Rejected)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "unexpected additional properties")
}

func TestCallLogKeepsOldRecords(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().AddDate(0, 0, -91).Format("2006-01-02")
	rec := `{"tool":"search_docs","outcome":"empty","empty":true}` + "\n"
	path := filepath.Join(dir, old+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(rec), 0600))
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ToolSearchDocs, rows[0].Tool)
	assert.Equal(t, 2, rows[0].Calls)
	rows, err = SummarizeCalls(t.Context(), dir, old, old)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, rows[0].Calls)
	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestCallLogRecoversAfterPartialAppend(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, today+".jsonl"), []byte(`{"tool":`), 0600))
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ToolSearchDocs, rows[0].Tool)
	assert.Equal(t, 1, rows[0].Calls)
}
