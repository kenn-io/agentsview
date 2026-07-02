package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestDoctorSyncCurrentDatabaseReportsNormalStartupSync(t *testing.T) {
	dataDir := testDataDir(t)

	database, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err, "open db")
	require.NoError(t, database.Close(), "close db")

	out, err := executeCommand(newRootCommand(), "doctor", "sync")
	require.NoError(t, err, "doctor sync")
	_, statErr := os.Stat(filepath.Join(dataDir, "config.toml"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"doctor sync must not create config.toml")

	assert.Contains(t, out, "Sync Diagnostics")
	assert.Contains(t, out, "Data directory: "+dataDir)
	assert.Contains(t, out, "Database: "+filepath.Join(dataDir, "sessions.db"))
	assert.Contains(t, out,
		fmt.Sprintf("SQLite user_version: %d", db.CurrentDataVersion()))
	assert.Contains(t, out,
		fmt.Sprintf("Binary data version: %d", db.CurrentDataVersion()))
	assert.Contains(t, out,
		"Startup sync decision: normal initial sync (no data-version resync)")
	assert.Contains(t, out,
		"Likely cause: data-version resync is not expected")
}

func TestDoctorSyncStaleDatabaseReportsLikelyAbortedResync(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")

	database, err := db.Open(dbPath)
	require.NoError(t, err, "open db")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:           "stale-session",
		Project:      "proj",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 1,
		DataVersion:  0,
	}), "insert session")
	require.NoError(t, database.Close(), "close db")

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "raw sqlite open")
	_, err = conn.Exec("PRAGMA user_version = 0")
	require.NoError(t, err, "downgrade user_version")
	require.NoError(t, conn.Close(), "close raw sqlite")

	logPath := filepath.Join(dataDir, "debug.log")
	require.NoError(t, os.WriteFile(logPath, []byte(
		"2026/06/18 data version outdated; full resync required\n"+
			"2026/06/18 resync aborted: 0 synced, 3 failed\n",
	), 0o644), "write debug log")

	out, err := executeCommand(newRootCommand(), "doctor", "sync")
	require.NoError(t, err, "doctor sync")

	assert.Contains(t, out, "SQLite user_version: 0")
	assert.Contains(t, out,
		fmt.Sprintf("Binary data version: %d", db.CurrentDataVersion()))
	assert.Contains(t, out,
		"Startup sync decision: full data-version resync required")
	assert.Contains(t, out, "Session data versions:")
	assert.Contains(t, out, "version 0: 1")
	assert.Contains(t, out, "Recent debug.log evidence:")
	assert.Contains(t, out, "resync aborted: 0 synced, 3 failed")
	assert.Contains(t, out,
		"Likely cause: previous data-version resync likely aborted before completion")
}

func TestDoctorSyncNewerDatabaseReportsRefusedStartup(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")

	database, err := db.Open(dbPath)
	require.NoError(t, err, "open db")
	require.NoError(t, database.Close(), "close db")

	futureVersion := db.CurrentDataVersion() + 10
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "raw sqlite open")
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", futureVersion))
	require.NoError(t, err, "set future user_version")
	require.NoError(t, conn.Close(), "close raw sqlite")

	out, err := executeCommand(newRootCommand(), "doctor", "sync")
	require.NoError(t, err, "doctor sync")

	assert.Contains(t, out,
		fmt.Sprintf("SQLite user_version: %d", futureVersion))
	assert.Contains(t, out,
		fmt.Sprintf("Binary data version: %d", db.CurrentDataVersion()))
	assert.Contains(t, out,
		"Startup sync decision: refuse startup (database requires newer agentsview)")
	assert.Contains(t, out,
		"Likely cause: SQLite user_version is newer than this binary")
	assert.Contains(t, out, `Run "agentsview update"`)
}

func TestWriteDoctorSummaryMode(t *testing.T) {
	var buf bytes.Buffer
	writeDoctorSummaryMode(&buf, doctorSyncReport{
		doctorDBInspection: doctorDBInspection{
			AntigravityCLITotal:   12,
			AntigravityCLISummary: 5,
		},
	})
	out := buf.String()
	assert.Contains(t, out, "antigravity-cli")
	assert.Contains(t, out, "5")
	assert.Contains(t, out, "summary mode")
	assert.Contains(t, out, "agy-reader")
}

func TestWriteDoctorSummaryModeSilentWhenNone(t *testing.T) {
	var buf bytes.Buffer
	writeDoctorSummaryMode(&buf, doctorSyncReport{
		doctorDBInspection: doctorDBInspection{
			AntigravityCLITotal: 12, AntigravityCLISummary: 0,
		},
	})
	assert.NotContains(t, buf.String(), "summary mode")
}

func TestWriteDoctorSummaryModeSilentOnErr(t *testing.T) {
	var buf bytes.Buffer
	writeDoctorSummaryMode(&buf, doctorSyncReport{
		doctorDBInspection: doctorDBInspection{
			AntigravityCLITotal:   12,
			AntigravityCLISummary: 5,
			AntigravityCountsErr:  errors.New("query failed"),
		},
	})
	assert.Empty(t, buf.String())
}

func TestWriteDoctorUnknownSchema(t *testing.T) {
	var buf bytes.Buffer
	writeDoctorUnknownSchema(&buf, doctorSyncReport{
		doctorDBInspection: doctorDBInspection{
			AntigravityUnknownSchema: 3,
		},
	})
	out := buf.String()
	assert.Contains(t, out, "3 session(s) on unrecognized Antigravity schema")
	assert.Contains(t, out, "agy-schema:")
}

func TestWriteDoctorUnknownSchemaSilentWhenNone(t *testing.T) {
	var buf bytes.Buffer
	writeDoctorUnknownSchema(&buf, doctorSyncReport{
		doctorDBInspection: doctorDBInspection{
			AntigravityUnknownSchema: 0,
		},
	})
	assert.Empty(t, buf.String())
}

func TestWriteDoctorUnknownSchemaSilentOnErr(t *testing.T) {
	var buf bytes.Buffer
	writeDoctorUnknownSchema(&buf, doctorSyncReport{
		doctorDBInspection: doctorDBInspection{
			AntigravityUnknownSchema: 3,
			AntigravityCountsErr:     errors.New("query failed"),
		},
	})
	assert.Empty(t, buf.String())
}

func TestInspectDoctorDBCountsAntigravityCLISummaryMode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")

	database := dbtest.OpenTestDBAt(t, dbPath)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:                 "agy-summary",
		Agent:              "antigravity-cli",
		Machine:            "local",
		Project:            "proj",
		TranscriptFidelity: "summary",
	}), "upsert summary session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:                 "agy-full",
		Agent:              "antigravity-cli",
		Machine:            "local",
		Project:            "proj",
		TranscriptFidelity: "full",
	}), "upsert full session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:      "other-agent",
		Agent:   "claude-code",
		Machine: "local",
		Project: "proj",
	}), "upsert other-agent session")
	require.NoError(t, database.Close(), "close db")

	insp := inspectDoctorDB(dbPath)
	require.NoError(t, insp.AntigravityCountsErr, "antigravity counts query")
	assert.Equal(t, 2, insp.AntigravityCLITotal, "AntigravityCLITotal")
	assert.Equal(t, 1, insp.AntigravityCLISummary, "AntigravityCLISummary")
}

// TestInspectDoctorDBCountsUnknownSchema verifies the unrecognized-schema
// count spans both Antigravity agents and excludes known-range labels and
// other agents that carry a generic source_version.
func TestInspectDoctorDBCountsUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")

	database := dbtest.OpenTestDBAt(t, dbPath)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:            "agy-ide-unknown",
		Agent:         "antigravity",
		Machine:       "local",
		Project:       "proj",
		SourceVersion: "agy-schema:abc123def456",
	}), "upsert IDE unknown-schema session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:            "agy-cli-unknown",
		Agent:         "antigravity-cli",
		Machine:       "local",
		Project:       "proj",
		SourceVersion: "agy-schema:abc123def456",
	}), "upsert CLI unknown-schema session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:            "agy-known",
		Agent:         "antigravity-cli",
		Machine:       "local",
		Project:       "proj",
		SourceVersion: "1.0.7-1.0.10",
	}), "upsert known-range session")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:            "piebald-generic",
		Agent:         "piebald",
		Machine:       "local",
		Project:       "proj",
		SourceVersion: "piebald-appdb-v1",
	}), "upsert non-antigravity session")
	require.NoError(t, database.Close(), "close db")

	insp := inspectDoctorDB(dbPath)
	require.NoError(t, insp.AntigravityCountsErr, "antigravity counts query")
	assert.Equal(t, 2, insp.AntigravityUnknownSchema, "AntigravityUnknownSchema")
}

func TestDoctorSyncReportStatErrorDoesNotRenderAsMissingDatabase(t *testing.T) {
	report := doctorSyncReport{
		Config: config.Config{
			DataDir: "/data",
			DBPath:  "/data/sessions.db",
		},
		doctorDBInspection: doctorDBInspection{
			DBExists: false,
			DBError:  errors.New("stat /data/sessions.db: permission denied"),
		},
	}

	var out bytes.Buffer
	writeDoctorSyncReport(&out, report)
	got := out.String()

	assert.Contains(t, got, "Database exists: unknown")
	assert.Contains(t, got,
		"Startup sync decision: unknown (database could not be inspected)")
	assert.Contains(t, got, "Session data versions:")
	assert.Contains(t, got,
		"unavailable: stat /data/sessions.db: permission denied")
	assert.Contains(t, got,
		"Likely cause: database could not be inspected; check database path and permissions")
	assert.NotContains(t, got, "database will be created")
	assert.NotContains(t, got, "database does not exist yet")
}
