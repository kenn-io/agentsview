package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day1+".jsonl"), []byte(strings.ReplaceAll(`{"version":1,"at":"2026-01-02T00:00:00Z","tool":"search_docs","outcome":"empty","bytes":10,"ms":4}
`, "2026-01-02", day1)), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day2+".jsonl"), []byte(strings.ReplaceAll(`{"version":1,"at":"2026-01-03T00:00:00Z","tool":"get_messages","outcome":"error","error":"other error","bytes":20,"ms":8}
`, "2026-01-03", day2)), 0600))
	earliest := time.Now().UTC().AddDate(0, 0, -91).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, earliest+".jsonl"), []byte(strings.ReplaceAll(`{"version":1,"at":"2026-01-02T00:00:00Z","tool":"query_recall","outcome":"empty","bytes":10,"ms":4}
`, "2026-01-02", earliest)), 0600))
	cmd := newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", "91d"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "query_recall: 1 calls")
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", day1, "--until", day1, "--json"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var rows []mcpserver.CallSummary
	require.NoError(t, json.Unmarshal(output.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "search_docs", rows[0].Tool)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.Equal(t, 100.0, *rows[0].EmptyPercent)
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", day2, "--until", day2})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "get_messages: 1 calls, 100.0% errors")
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
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", day3, "--until", day1})
	assert.Error(t, cmd.Execute())
}

func TestMCPStdioWarnsWhenCallLogConfigFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("invalid = [\n"), 0600))
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
