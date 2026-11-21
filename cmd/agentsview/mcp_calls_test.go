package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	mcpserver "go.kenn.io/agentsview/internal/mcp"
)

func TestMCPCallsReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", home)
	dir := filepath.Join(home, "mcp", "calls")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2026-01-02.jsonl"), []byte(`{"at":"2026-01-02T00:00:00Z","tool":"search_docs","outcome":"empty","bytes":10,"ms":4}
`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2026-01-03.jsonl"), []byte(`{"at":"2026-01-03T00:00:00Z","tool":"get_messages","outcome":"error","error":"other error","bytes":20,"ms":8}
`), 0600))
	cmd := newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", "2026-01-02", "--until", "2026-01-02", "--json"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var rows []mcpserver.CallSummary
	require.NoError(t, json.Unmarshal(output.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "search_docs", rows[0].Tool)
	require.NotNil(t, rows[0].EmptyPercent)
	assert.Equal(t, 100.0, *rows[0].EmptyPercent)
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", "2026-01-03", "--until", "2026-01-03"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "get_messages: 1 calls, 100.0% errors")
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", "2026-01-04", "--until", "2026-01-04"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "No MCP calls recorded in this range.\n", output.String())
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", "2026-01-04", "--until", "2026-01-02"})
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
