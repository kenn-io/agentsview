package main

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"strings"
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

func TestExportReportingUpgradesOldCursorUsageArchive(t *testing.T) {
	dataDir := testDataDir(t)
	path := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, path)
	require.NoError(t, database.ConfigureArtifactLocalMachine(t.Context(), "origin-a"))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO archive_metadata(key,value) VALUES ('archive_id','old-archive') ON CONFLICT(key) DO UPDATE SET value=excluded.value;
			INSERT INTO cursor_usage_events(occurred_at,model,output_tokens,dedup_key) VALUES ('2026-07-28T10:03:00Z','a',17,'old-a'),('2026-07-28T10:04:00Z','b',5,'old-b')`)
		return err
	}))
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `ALTER TABLE cursor_usage_events DROP COLUMN account_polled_at; ALTER TABLE cursor_usage_events DROP COLUMN account_user_id`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	read := func(version string) string {
		out, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now), "export", "day", "--schema-version", version, "--by-machine", "2026-07-28")
		require.NoError(t, err)
		var day struct {
			Hours []struct {
				Machines map[string]struct {
					Usage struct {
						Totals struct {
							Output int64 `json:"output_tokens"`
						} `json:"totals"`
					} `json:"usage"`
				} `json:"machines"`
				Usage struct {
					Totals struct {
						Output int64 `json:"output_tokens"`
					} `json:"totals"`
				} `json:"usage"`
			} `json:"hours"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &day))
		assert.EqualValues(t, 22, day.Hours[10].Usage.Totals.Output)
		assert.EqualValues(t, 22, day.Hours[10].Machines["archive:old-archive"].Usage.Totals.Output)
		for key := range day.Hours[10].Machines {
			assert.NotContains(t, key, "cursor-account:")
		}
		return out
	}
	first := read("6")
	assert.Equal(t, first, read("6"))
	read("5")
	raw, err = sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer raw.Close()
	var count int
	require.NoError(t, raw.QueryRowContext(t.Context(), `SELECT count(*) FROM cursor_usage_events WHERE account_user_id='' AND account_polled_at=''`).Scan(&count))
	assert.Equal(t, 2, count)
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
			UPDATE cursor_usage_events SET account_user_id='12', account_polled_at='2026-07-29T00:00:00.000000Z';`)
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
	assert.Equal(t, "2026-07-29T00:00:00.000000Z", result.Machines["cursor-account:12"].PolledAt)
	dayOut, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now), "export", "day", "--schema-version", "6", "--by-machine", "2026-07-28")
	require.NoError(t, err)
	var day struct {
		Hours []jsontext.Value `json:"hours"`
	}
	require.NoError(t, json.Unmarshal([]byte(dayOut), &day))
	assert.Equal(t, strings.TrimSpace(out), string(day.Hours[10]))
	scopedOut, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now), "export", "hour", "--schema-version", "6", "--by-machine", "--project-key", "other", "2026-07-28-10")
	require.NoError(t, err)
	var scoped struct {
		Machines map[string]jsontext.Value `json:"machines"`
	}
	require.NoError(t, json.Unmarshal([]byte(scopedOut), &scoped))
	assert.NotContains(t, scoped.Machines, "cursor-account:12")

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
