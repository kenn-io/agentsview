package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"
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
	assert.Equal(t, time.Date(2026, 1, 1, 23, 55, 0, 0, time.UTC), event.OccurredAt)
	d := dbtest.OpenTestDBAt(t, path)
	ts := localServerProxy(t, d, dataDir, server.WithBasePath(""))
	out, _, err = runInsightCommand(t, append(args, "--server", ts.URL)...)
	require.NoError(t, err)
	var remoteEvent db.DiagnosticEvent
	require.NoError(t, json.Unmarshal([]byte(out), &remoteEvent))
	assert.Equal(t, id, remoteEvent.ID)
	assert.Equal(t, time.Date(2026, 1, 1, 23, 55, 0, 0, time.UTC), remoteEvent.OccurredAt)
	retryID := uuid.NewV7().String()
	retryArgs := []string{"diagnostics", "append", "--source", "worker", "--message", "retry failure", "--id", retryID, "--server", ts.URL, "--json"}
	first, _, err := runInsightCommand(t, retryArgs...)
	require.NoError(t, err)
	retry, _, err := runInsightCommand(t, retryArgs...)
	require.NoError(t, err)
	assert.JSONEq(t, first, retry)
	events, err := d.DiagnosticEventsAfter(t.Context(), 0, 10)
	require.NoError(t, err)
	assert.Len(t, events, 2)

	for _, extra := range [][]string{nil, {"--server", ts.URL}} {
		args := append([]string{"diagnostics", "append", "--source", "worker", "--message", "failure", "--at", "0001-01-01T00:00:00Z"}, extra...)
		_, _, err = runInsightCommand(t, args...)
		if len(extra) == 0 {
			require.ErrorIs(t, err, db.ErrInvalidDiagnostic)
		} else {
			require.ErrorContains(t, err, "occurred_at")
		}
	}
	_, _, err = runInsightCommand(t, "diagnostics", "append", "--source", "worker", "--message", "failure", "--pg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
}
