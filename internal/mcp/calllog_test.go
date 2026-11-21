package mcp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, &CallCount{Name: "invalid arguments", Count: 1}, rows[0].TopError)
	assert.Equal(t, &CallCount{Name: "session not found", Count: 1}, rows[1].TopError)
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2026-01-02.jsonl"), []byte(`{"at":"2026-01-02T01:00:00Z","tool":"search_docs","outcome":"ok","bytes":10,"ms":2}
{"at":"2026-01-02T02:00:00Z","tool":"search_docs","outcome":"empty","bytes":30,"ms":6}
broken
`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2026-01-03.jsonl"), []byte(`{"at":"2026-01-03T00:00:00Z","tool":"search_docs","outcome":"ok","bytes":90,"ms":20}
`), 0600))
	rows, err := SummarizeCalls(t.Context(), dir, "2026-01-02", "2026-01-02")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Calls)
	assert.Equal(t, 20, rows[0].MedianBytes)
	assert.Equal(t, 4, rows[0].MedianMs)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.Equal(t, 50.0, *rows[0].EmptyPercent)
	rows, err = SummarizeCalls(t.Context(), dir, "2026-01-02", "2026-01-03")
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
		{"query": "secret-value", "Mixed Case]\\\"": "secret-value"},
		{"query": 123},
	} {
		_, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
	}
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 4, rows[0].Calls)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 100.0/3, *rows[0].EmptyPercent, 0.001)
	assert.Equal(t, []CallCount{{Name: "Mixed Case]\\\"", Count: 1}}, rows[0].Rejected)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-value")
	assert.NotContains(t, string(raw), "validating")
}

func TestCallLogLongRecords(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	name := strings.Repeat("x", 70000)
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{"query": "secret", name: true}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []CallCount{{Name: name, Count: 1}}, rows[0].Rejected)
}

func TestCallLogWriteFailureWarns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(dir, []byte("file"), 0600))
	var warnings bytes.Buffer
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: filepath.Join(dir, "calls"), ErrorWriter: &warnings}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, warnings.String(), "could not record MCP call")
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
	require.ErrorIs(t, recordToolCall(ctx, dir, req, nil, nil, time.Second), context.Canceled)
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}
