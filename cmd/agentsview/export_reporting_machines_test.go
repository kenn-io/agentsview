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

func TestExportReportingCursorAccounts(t *testing.T) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.ConfigureArtifactLocalMachine(t.Context(), "origin-a"))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO archive_metadata(key, value) VALUES ('archive_id', 'test-archive') ON CONFLICT(key) DO UPDATE SET value=excluded.value;
			INSERT INTO cursor_usage_events(id, agent, occurred_at, model, output_tokens, user_id, dedup_key) VALUES
			(1, 'cursor', '2026-07-28T10:03:00Z', 'model-a', 10, '12', 'a'),
			(2, 'cursor', '2026-07-28T10:04:00Z', 'model-b', 20, '34', 'b'),
			(3, 'cursor', '2026-07-28T10:05:00Z', 'model-c', 3, '', 'c'),
			(4, 'xai', '2026-07-28T10:06:00Z', 'model-d', 4, '12', 'd'),
			(5, 'cursor', '2026-07-28T10:07:00Z', 'model-e', 5, '12', 'e'),
			(6, 'cursor', '2026-07-28T11:07:00Z', 'model-f', 6, '12', 'f'),
			(7, 'cursor', '2026-07-28T10:08:00Z', 'model-g', 8, 'invalid', 'g'),
			(8, 'cursor', '2026-07-28T09:08:00Z', 'model-h', 9, '12', 'h');
			INSERT INTO cursor_usage_polls VALUES ('2026-07-28T10:00:00Z', '2026-07-28T11:00:00Z', '2026-07-29T00:00:00Z', 7);
			UPDATE cursor_usage_polls SET event_max_id=4;
			INSERT INTO cursor_usage_polls VALUES ('2026-07-28T09:00:00Z', '2026-07-28T10:00:00Z', '2026-07-29T00:00:00Z', 8);
			INSERT INTO cursor_usage_polls VALUES ('2026-07-28T11:00:00Z', '2026-07-28T12:00:00Z', '2026-07-28T11:30:00Z', 8);`)
		return err
	}))
	require.NoError(t, database.Close())
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	type hour struct {
		PolledAt string `json:"polled_at"`
		Joint    struct {
			Cells []any `json:"cells"`
		} `json:"joint"`
		Activity struct {
			Totals struct {
				NewModels int `json:"new_models"`
			} `json:"totals"`
		} `json:"activity"`
		Machines map[string]hour `json:"machines"`
		Usage    struct {
			Totals struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"totals"`
		} `json:"usage"`
	}
	read := func(args ...string) (struct {
		Hours []hour `json:"hours"`
	}, error,
	) {
		var day struct {
			Hours []hour `json:"hours"`
		}
		out, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now), args...)
		if err == nil {
			err = json.Unmarshal([]byte(out), &day)
		}
		return day, err
	}
	day, err := read("export", "day", "--schema-version", "6", "--by-machine", "2026-07-28")
	require.NoError(t, err)
	assert.EqualValues(t, 10, day.Hours[10].Machines["cursor-account:12"].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 20, day.Hours[10].Machines["cursor-account:34"].Usage.Totals.OutputTokens)
	assert.Equal(t, "2026-07-29T00:00:00Z", day.Hours[10].Machines["cursor-account:12"].PolledAt)
	assert.Empty(t, day.Hours[10].Machines["cursor-account:12"].Joint.Cells)
	assert.Zero(t, day.Hours[10].Machines["cursor-account:12"].Activity.Totals.NewModels)
	assert.EqualValues(t, 20, day.Hours[10].Machines["archive:test-archive"].Usage.Totals.OutputTokens)
	assert.NotContains(t, day.Hours[11].Machines, "cursor-account:12")
	assert.Contains(t, day.Hours[9].Machines, "archive:test-archive")
	assert.Zero(t, day.Hours[9].Machines["archive:test-archive"].Usage.Totals.OutputTokens)
	scoped, err := read("export", "day", "--schema-version", "6", "--by-machine", "--project-key", "other", "2026-07-28")
	require.NoError(t, err)
	assert.NotContains(t, scoped.Hours[10].Machines, "cursor-account:12")
	database = dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO cursor_usage_polls VALUES ('2026-07-28T10:00:00Z', '2026-07-28T11:00:00Z', '2026-07-29T01:00:00Z', 6)`)
		return err
	}))
	require.NoError(t, database.Close())
	day, err = read("export", "day", "--schema-version", "6", "--by-machine", "2026-07-28")
	require.NoError(t, err)
	assert.EqualValues(t, 15, day.Hours[10].Machines["cursor-account:12"].Usage.Totals.OutputTokens)
	assert.EqualValues(t, 15, day.Hours[10].Machines["archive:test-archive"].Usage.Totals.OutputTokens)
	_, err = read("export", "day", "--schema-version", "6", "2026-07-28")
	require.Error(t, err)
	day, err = read("export", "day", "--schema-version", "5", "--by-machine", "2026-07-28")
	require.NoError(t, err)
	assert.EqualValues(t, 50, day.Hours[10].Machines["archive:test-archive"].Usage.Totals.OutputTokens)
	database = dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE cursor_usage_polls`)
		return err
	}))
	require.NoError(t, database.Close())
	day, err = read("export", "day", "--schema-version", "6", "--by-machine", "2026-07-28")
	require.NoError(t, err)
	assert.EqualValues(t, 50, day.Hours[10].Machines["archive:test-archive"].Usage.Totals.OutputTokens)
}

func TestExportReportingCursorAccountMachineCollision(t *testing.T) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.ConfigureArtifactLocalMachine(t.Context(), "cursor-account:12"))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO archive_metadata(key, value) VALUES ('archive_id', 'test-archive') ON CONFLICT(key) DO UPDATE SET value=excluded.value;
			INSERT INTO pg_sync_state(key, value) VALUES ('machine_alias:peer', 'cursor-account:12');
			INSERT INTO sessions(id, project, machine, agent, message_count, started_at, ended_at) VALUES
			('native', 'project', 'local', 'claude', 2, '2026-07-28T10:00:00Z', '2026-07-28T10:01:00Z'),
			('peer', 'project', 'peer', 'claude', 2, '2026-07-28T10:00:00Z', '2026-07-28T10:02:00Z'),
			('direct', 'project', 'cursor-account:12', 'claude', 2, '2026-07-28T10:00:00Z', '2026-07-28T10:01:00Z'),
			('escaped', 'project', 'machine:2939296221cda1ef57dff3bf4333caa28eafdc25e0bc81db56cfd1ad9b755e9e', 'claude', 2, '2026-07-28T10:00:00Z', '2026-07-28T10:03:00Z');
			INSERT INTO messages(session_id, ordinal, role, content, timestamp, model) VALUES
			('native', 0, 'user', 'start', '2026-07-28T10:00:00Z', ''), ('native', 1, 'assistant', 'end', '2026-07-28T10:01:00Z', 'model-a'),
			('peer', 0, 'user', 'start', '2026-07-28T10:00:00Z', ''), ('peer', 1, 'assistant', 'end', '2026-07-28T10:02:00Z', 'model-a'),
			('direct', 0, 'user', 'start', '2026-07-28T10:00:00Z', ''), ('direct', 1, 'assistant', 'end', '2026-07-28T10:01:00Z', 'model-a'),
			('escaped', 0, 'user', 'start', '2026-07-28T10:00:00Z', ''), ('escaped', 1, 'assistant', 'end', '2026-07-28T10:03:00Z', 'model-a');
			INSERT INTO cursor_usage_events(occurred_at, model, output_tokens, user_id, dedup_key) VALUES ('2026-07-28T10:04:00Z', 'model-b', 10, '12', 'account');
			INSERT INTO cursor_usage_polls VALUES ('2026-07-28T10:00:00Z', '2026-07-28T11:00:00Z', '2026-07-29T00:00:00Z', 1);`)
		return err
	}))
	require.NoError(t, database.Close())
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	out, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now), "export", "hour", "--schema-version", "6", "--by-machine", "2026-07-28-10")
	require.NoError(t, err)
	var result struct {
		LocalMachineID string `json:"local_machine_id"`
		Machines       map[string]struct {
			PolledAt string `json:"polled_at"`
			Activity struct {
				Totals struct {
					AgentMinutes float64 `json:"agent_minutes"`
					NewSessions  int     `json:"new_sessions"`
					NewProjects  int     `json:"new_projects"`
					NewModels    int     `json:"new_models"`
				} `json:"totals"`
			} `json:"activity"`
			Joint struct {
				Cells []any `json:"cells"`
			} `json:"joint"`
			Usage struct {
				Totals struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"totals"`
			} `json:"usage"`
		} `json:"machines"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	const machine = "machine:2939296221cda1ef57dff3bf4333caa28eafdc25e0bc81db56cfd1ad9b755e9e"
	assert.Equal(t, machine, result.LocalMachineID)
	assert.InDelta(t, 4, result.Machines[machine].Activity.Totals.AgentMinutes, 0.001)
	assert.NotEmpty(t, result.Machines[machine].Joint.Cells)
	assert.InDelta(t, 3, result.Machines["machine:99b14951da0e7986cffad677018196fe8842ed68aa0eac7824b9c304fba6f930"].Activity.Totals.AgentMinutes, 0.001)
	assert.EqualValues(t, 10, result.Machines["cursor-account:12"].Usage.Totals.OutputTokens)
	assert.Equal(t, "2026-07-29T00:00:00Z", result.Machines["cursor-account:12"].PolledAt)

	out, _, err = executeExportSessionsCommand(newExportReportingTestRoot(now), "export", "hour", "--schema-version", "5", "--by-machine", "2026-07-28-10")
	require.NoError(t, err)
	result.Machines = nil
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "cursor-account:12", result.LocalMachineID)
	assert.InDelta(t, 4, result.Machines["cursor-account:12"].Activity.Totals.AgentMinutes, 0.001)
	assert.Equal(t, 3, result.Machines["cursor-account:12"].Activity.Totals.NewSessions)
	assert.Equal(t, 1, result.Machines["cursor-account:12"].Activity.Totals.NewProjects)
	assert.Equal(t, 1, result.Machines["cursor-account:12"].Activity.Totals.NewModels)
	assert.NotEmpty(t, result.Machines["cursor-account:12"].Joint.Cells)
	assert.InDelta(t, 3, result.Machines[machine].Activity.Totals.AgentMinutes, 0.001)
	assert.EqualValues(t, 10, result.Machines["archive:test-archive"].Usage.Totals.OutputTokens)
}
