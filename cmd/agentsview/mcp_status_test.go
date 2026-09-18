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
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", home)
	cleanup, err := mcpdiscovery.Publish(filepath.Join(home, "mcp"), "127.0.0.1:9876", "", "http://127.0.0.1:4321")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(cleanup()) })
	command := newMCPStatusCommand()
	command.SetArgs([]string{"--json"})
	var output bytes.Buffer
	command.SetOut(&output)
	require.NoError(command.Execute())
	var rows []mcpdiscovery.Endpoint
	require.NoError(json.Unmarshal(output.Bytes(), &rows))
	require.Len(rows, 1)
	assert.Equal("http://127.0.0.1:9876/mcp", rows[0].URL)
	assert.Equal("http://127.0.0.1:4321", rows[0].BackendURL)
}
