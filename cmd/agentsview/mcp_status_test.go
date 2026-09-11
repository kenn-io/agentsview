package main

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/mcpdiscovery"
)

func TestMCPStatusJSONReportsPublishedListener(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", home)
	cleanup, err := mcpdiscovery.Publish(filepath.Join(home, "mcp"), "127.0.0.1:9876", "", "http://127.0.0.1:4321")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })
	command := newMCPStatusCommand()
	command.SetArgs([]string{"--json"})
	var output bytes.Buffer
	command.SetOut(&output)
	require.NoError(t, command.Execute())
	var rows []mcpdiscovery.Endpoint
	require.NoError(t, json.Unmarshal(output.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "http://127.0.0.1:9876/mcp", rows[0].URL)
	assert.Equal(t, "http://127.0.0.1:4321", rows[0].BackendURL)
}
