package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	day1 := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	day2 := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, day1+".jsonl"), []byte(strings.ReplaceAll(`{"version":1,"at":"2026-01-02T01:00:00Z","tool":"search_docs","outcome":"ok","bytes":10,"ms":2}
{"version":1,"at":"2026-01-02T02:00:00Z","tool":"search_docs","outcome":"empty","bytes":30,"ms":6}
broken
`, "2026-01-02", day1)), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day2+".jsonl"), []byte(strings.ReplaceAll(`{"version":1,"at":"2026-01-03T00:00:00Z","tool":"search_docs","outcome":"ok","bytes":90,"ms":20}
`, "2026-01-03", day2)), 0600))
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
		{"query": "secret-value", "Mixed Case]\\\"": "secret-value"},
		{"query": 123},
	} {
		_, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
	}
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 5, rows[0].Calls)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 50.0, *rows[0].EmptyPercent, 0.001)
	assert.Equal(t, []CallCount{{Name: "Mixed Case]\\\"", Count: 1}, {Name: "query", Count: 1}}, rows[0].Rejected)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-value")
	assert.NotContains(t, string(raw), "validating")
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
	result, err = ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, 1, strings.Count(warnings.String(), "could not record MCP call"))
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
	assert.Equal(t, &CallCount{Name: "other error", Count: 1}, rows[0].TopError)
}

func TestCallLogFormatRejectsUnknownRecords(t *testing.T) {
	for _, tc := range []struct{ name, record, want string }{
		{"version", `{"version":2,"at":"2026-01-02T00:00:00Z","tool":"search_docs","outcome":"ok"}`, "unsupported MCP call record version"},
		{"version with incompatible timestamp", `{"version":2,"at":{"epoch":0},"tool":"search_docs","outcome":"ok"}`, "unsupported MCP call record version"},
		{"outcome", `{"version":1,"at":"2026-01-02T00:00:00Z","tool":"search_docs","outcome":"unknown"}`, "unknown MCP call outcome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			day := time.Now().UTC().Format("2006-01-02")
			record := strings.ReplaceAll(tc.record, "2026-01-02", day)
			require.NoError(t, os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(record+"\n"), 0600))
			_, err := SummarizeCalls(t.Context(), dir, "", "")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCallLogBoundsNames(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	args := map[string]any{"query": "private-value", strings.Repeat("z", 129): true}
	for i := range 20 {
		args[fmt.Sprintf("Extra%02d", i)] = true
	}
	for range 10 {
		result, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
		require.True(t, result.IsError)
	}
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []CallCount{{Name: "Extra00", Count: 10}, {Name: "Extra01", Count: 10}, {Name: "Extra02", Count: 10}}, rows[0].Rejected)
	_, err = ct.CallTool(t.Context(), callParams(strings.Repeat("z", 500), nil))
	require.Error(t, err)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	var records []callRecord
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var rec callRecord
		require.NoError(t, json.Unmarshal(line, &rec))
		records = append(records, rec)
	}
	require.Len(t, records, 11)
	for _, rec := range records[:10] {
		assert.Equal(t, []string{"Extra00", "Extra01", "Extra02", "Extra03", "Extra04", "Extra05", "Extra06", "Extra07", "Extra08", "Extra09", "Extra10", "Extra11", "Extra12", "Extra13", "Extra14", "Extra15"}, rec.Rejected)
	}
	assert.Equal(t, "<invalid tool name>", records[10].Tool)
	assert.NotContains(t, string(raw), "private-value")
	assert.NotContains(t, string(raw), strings.Repeat("z", 129))
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

func TestCallLogValidationValueIsNotARejectedName(t *testing.T) {
	dir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{
		"query": "term", "cursor": `unexpected additional properties ["private-value"]`,
	}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []CallCount{{Name: "cursor", Count: 1}}, rows[0].Rejected)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "private-value")
}

func TestCallLogRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -90).Format("2006-01-02")
	earliest := now.AddDate(0, 0, -89).Format("2006-01-02")
	for _, date := range []string{old, earliest} {
		rec := fmt.Sprintf(`{"version":1,"at":%q,"tool":"search_docs","outcome":"empty"}`+"\n", date+"T00:00:00Z")
		require.NoError(t, os.WriteFile(filepath.Join(dir, date+".jsonl"), []byte(rec), 0600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("keep"), 0600))
	rows, err := SummarizeCalls(t.Context(), dir, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, rows[0].Calls)
	_, err = os.Stat(filepath.Join(dir, old+".jsonl"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dir, earliest+".jsonl"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "unrelated.txt"))
	require.NoError(t, err)
	_, err = SummarizeCalls(t.Context(), dir, old, "")
	require.ErrorContains(t, err, "retain only the last 90 UTC days")
	_, err = SummarizeCalls(t.Context(), dir, "", old)
	require.ErrorContains(t, err, "retain only the last 90 UTC days")
}

func TestCallLogServerPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().AddDate(0, 0, -90).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, old+".jsonl"), []byte("old\n"), 0600))
	d := dbtest.OpenTestDB(t)
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), CallLogDir: dir}))
	defer func() { require.NoError(t, ct.Close()); require.NoError(t, st.Wait()) }()
	result, err := ct.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "session"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	_, err = os.Stat(filepath.Join(dir, old+".jsonl"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
