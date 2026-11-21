package main

import (
	"bytes"
	"encoding/json/v2"
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
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day1+".jsonl"), []byte(`{"tool":"search_docs","outcome":"empty","empty":true,"bytes":10,"ms":4}
`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, day2+".jsonl"), []byte(`{"tool":"get_messages","outcome":"error","bytes":20,"ms":8}
{"tool":"get_messages","outcome":"truncated","truncated":true,"bytes":20,"ms":8}
`), 0600))
	earliest := time.Now().UTC().AddDate(0, 0, -91).Format("2006-01-02")
	require.NoError(t, os.WriteFile(filepath.Join(dir, earliest+".jsonl"), []byte(`{"tool":"query_recall","outcome":"empty","empty":true,"bytes":10,"ms":4}
`), 0600))
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
	assert.Contains(t, output.String(), "get_messages: 2 calls, 50.0% errors, 50.0% truncated")
	cmd = newMCPCallsCommand()
	cmd.SetArgs([]string{"--since", day2, "--until", day2, "--format", "json"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	require.NoError(t, json.Unmarshal(output.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, 50.0, rows[0].TruncatedPercent)
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
