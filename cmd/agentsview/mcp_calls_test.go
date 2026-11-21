package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	mcpserver "go.kenn.io/agentsview/internal/mcp"
)

func TestMCPCallsReport(t *testing.T) {
	day1 := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	day2 := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	day3 := time.Now().UTC().Format("2006-01-02")
	home := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", home)
	dir := filepath.Join(home, "mcp", "calls")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day1+".jsonl"), []byte(`{"tool":"search_docs","outcome":"empty","empty":true,"bytes":10,"ms":4}
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day2+".jsonl"), []byte(`{"tool":"get_messages","outcome":"error","bytes":20,"ms":8}
{"tool":"get_messages","outcome":"truncated","truncated":true,"bytes":20,"ms":8}
`), 0o600))
	earliest := time.Now().UTC().AddDate(0, 0, -91).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, earliest+".jsonl"), []byte(`{"tool":"query_recall","outcome":"empty","empty":true,"bytes":10,"ms":4}
`), 0o600))
	cmd := newMCPCallsCommand()
	var output bytes.Buffer
	cmd.SetArgs([]string{"--since", day1, "--until", day1, "--json"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var rows []mcpserver.CallSummary
	require.NoError(t, json.Unmarshal(output.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "search_docs", rows[0].Tool)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.InDelta(t, 100.0, *rows[0].EmptyPercent, 0.001)
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", day2, "--until", day2})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "get_messages: 2 calls\n  50.0% errors, 50.0% truncated, empty: -\n  rejected: -\n  median: 20 bytes, 8 ms\n", output.String())
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", day3, "--until", day3})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "No MCP calls recorded in this range.\n", output.String())
	cmd = newMCPCallsCommand()
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "search_docs: 1 calls")
	assert.NotContains(t, output.String(), "query_recall")
}

func TestMCPStdioWarnsWhenCallLogConfigFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("invalid = [\n"), 0o600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" {
			_, _ = w.Write([]byte(`{"api_version":4}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	input, writer, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	oldInput := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldInput; _ = input.Close() })
	cmd := newMCPCommand()
	cmd.SetArgs([]string{"--server", srv.URL})
	cmd.SetContext(t.Context())
	var warnings bytes.Buffer
	cmd.SetErr(&warnings)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, warnings.String(), "MCP calls will not be recorded")
}
