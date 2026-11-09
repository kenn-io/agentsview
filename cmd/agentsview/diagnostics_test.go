package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

func TestDiagnosticsAppendLocalAndServer(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	path := filepath.Join(dataDir, "sessions.db")
	require.NoError(t, dbtest.OpenTestDBAt(t, path).Close())
	id := uuid.NewV7().String()
	args := []string{"diagnostics", "append", "--source", "worker", "--message", "launch failed", "--at", "2026-01-01T23:55:00Z", "--id", id, "--json"}
	out, _, err := runInsightCommand(t, args...)
	require.NoError(t, err)
	var event db.DiagnosticEvent
	require.NoError(t, json.Unmarshal([]byte(out), &event))
	assert.Equal(t, id, event.ID)
	d := dbtest.OpenTestDBAt(t, path)
	ts := localServerProxy(t, d, dataDir, server.WithBasePath(""))
	out, _, err = runInsightCommand(t, append(args, "--server", ts.URL)...)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &event))
	assert.Equal(t, id, event.ID)
	events, err := d.DiagnosticEventsAfter(t.Context(), 0, 10)
	require.NoError(t, err)
	assert.Len(t, events, 1)
	_, _, err = runInsightCommand(t, "diagnostics", "append", "--source", "worker", "--message", "failure", "--pg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
}
