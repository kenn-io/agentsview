package main

import (
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

// A copied archive must retain the originating machines before aggregation.
// Filtering or annotating the already combined totals cannot recover them.
func TestExportReportingByMachine(t *testing.T) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.ConfigureArtifactLocalMachine(t.Context(), "origin-a"))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO archive_metadata(key, value) VALUES ('archive_id', 'machine-test-archive')
			ON CONFLICT(key) DO UPDATE SET value=excluded.value;
			INSERT INTO sessions(id, project, machine, agent, message_count, started_at, ended_at) VALUES
			('native', 'same-project', 'origin-a', 'claude', 2, '2026-07-28T10:00:00Z', '2026-07-28T10:01:00Z'),
			('imported', 'same-project', 'origin-b', 'claude', 2, '2026-07-28T10:00:00Z', '2026-07-28T10:02:00Z');
			INSERT INTO messages(session_id, ordinal, role, content, timestamp, model) VALUES
			('native', 0, 'user', 'start', '2026-07-28T10:00:00Z', ''),
			('native', 1, 'assistant', 'end', '2026-07-28T10:01:00Z', 'model-a'),
			('imported', 0, 'user', 'start', '2026-07-28T10:00:00Z', ''),
			('imported', 1, 'assistant', 'end', '2026-07-28T10:02:00Z', 'model-a');
			INSERT INTO usage_events(session_id, source, model, output_tokens, occurred_at, dedup_key) VALUES
			('native', 'fixture', 'model-a', 100, '2026-07-28T10:01:00Z', 'native'),
			('imported', 'fixture', 'model-a', 900, '2026-07-28T10:02:00Z', 'imported');
			INSERT INTO cursor_usage_events(occurred_at, model, output_tokens, dedup_key)
			VALUES ('2026-07-28T10:03:00Z', 'standalone', 17, 'standalone');`)
		return err
	}))
	require.NoError(t, database.Close())

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	stdout, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
		"export", "day", "--schema-version", "5", "--by-machine", "2026-07-28")
	require.NoError(t, err)
	type machineHour struct {
		LocalMachineID string                 `json:"local_machine_id"`
		Machines       map[string]machineHour `json:"machines"`
		Usage          struct {
			Totals struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"totals"`
		} `json:"usage"`
	}
	var day struct {
		Hours []machineHour `json:"hours"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &day))
	require.Len(t, day.Hours, 24)
	assert.Equal(t, "origin-a", day.Hours[0].LocalMachineID)
	assert.Contains(t, day.Hours[0].Machines, "origin-a", "quiet native hours must replace stale imported copies too")
	assert.EqualValues(t, 1017, day.Hours[10].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 100, day.Hours[10].Machines["origin-a"].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 900, day.Hours[10].Machines["origin-b"].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 17, day.Hours[10].Machines["archive:machine-test-archive"].Usage.Totals.OutputTokens)
}
