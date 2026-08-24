package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceBaselineSchemaUsesLocalCompanionTable(t *testing.T) {
	d := testDB(t)
	var sessionColumns int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name = 'source_baseline_path'`,
	).Scan(&sessionColumns))
	assert.Zero(t, sessionColumns,
		"machine-local watcher proof must not expand the shared session model")

	var baselineTables int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'local_session_source_baselines'`,
	).Scan(&baselineTables))
	assert.Equal(t, 1, baselineTables)
	var indexSQL string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_local_source_baselines_ownership'`,
	).Scan(&indexSQL))
	assert.Contains(t, strings.ToLower(strings.Join(strings.Fields(indexSQL), " ")),
		"machine, agent, file_path, session_id")
}

func TestSourceMissingSchemaStartsAndMigratesWithoutDataLoss(t *testing.T) {
	t.Run("new database", func(t *testing.T) {
		d := testDB(t)
		assertSourceTombstoneSchema(t, d)
	})

	t.Run("existing database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "archive.db")
		d := testDBAtPath(t, path, "pre-migration archive")
		insertSession(t, d, "preserved", "project")
		insertSession(t, d, "legacy-missing", "project")
		require.NoError(t, d.Close())

		conn, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = conn.Exec("DROP INDEX IF EXISTS idx_sessions_agent_file_path_active")
		require.NoError(t, err)
		_, err = conn.Exec("DROP TABLE IF EXISTS local_session_source_baselines")
		require.NoError(t, err)
		_, err = conn.Exec("ALTER TABLE sessions DROP COLUMN source_missing_at")
		require.NoError(t, err)
		_, err = conn.Exec(`
			UPDATE sessions
			SET deleted_at = '2026-08-01T00:00:00Z',
			    deletion_cause = 'source_missing'
			WHERE id = 'legacy-missing'`)
		require.NoError(t, err)
		_, err = conn.Exec(`
			CREATE INDEX idx_sessions_agent_file_path_active
			ON sessions(agent, file_path)
			WHERE file_path IS NOT NULL AND deleted_at IS NULL`)
		require.NoError(t, err)
		require.NoError(t, conn.Close())

		migrated, err := Open(path)
		require.NoError(t, err)
		defer migrated.Close()
		assertSourceTombstoneSchema(t, migrated)
		preserved, err := migrated.GetSession(t.Context(), "preserved")
		require.NoError(t, err)
		assert.NotNil(t, preserved, "schema migration must preserve archive rows")
		var baselines int
		require.NoError(t, migrated.getReader().QueryRow(
			"SELECT count(*) FROM local_session_source_baselines WHERE session_id = ?", "preserved",
		).Scan(&baselines))
		assert.Zero(t, baselines,
			"migration must not make a historical row deletion-eligible")
		legacy, err := migrated.GetSession(t.Context(), "legacy-missing")
		require.NoError(t, err)
		assert.NotNil(t, legacy,
			"migration must restore visibility to legacy source-missing rows")
		assertSessionState(t, migrated, "legacy-missing", false, true)
	})
}

func assertSourceTombstoneSchema(t *testing.T, d *DB) {
	t.Helper()
	var causeColumns int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name = 'deletion_cause'`,
	).Scan(&causeColumns))
	assert.Equal(t, 1, causeColumns)
	var sourceMissingColumns int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name = 'source_missing_at'`,
	).Scan(&sourceMissingColumns))
	assert.Equal(t, 1, sourceMissingColumns)
	var baselineColumns int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name = 'source_baseline_path'`,
	).Scan(&baselineColumns))
	assert.Zero(t, baselineColumns)

	rows, err := d.getReader().Query(
		"SELECT name FROM pragma_index_info('idx_sessions_agent_file_path_active') ORDER BY seqno",
	)
	require.NoError(t, err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"agent", "file_path", "id"}, columns)
	var indexSQL string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_sessions_agent_file_path_active'
	`).Scan(&indexSQL))
	normalizedIndexSQL := strings.ToLower(strings.Join(strings.Fields(indexSQL), " "))
	assert.Contains(t, normalizedIndexSQL,
		"where file_path is not null and deleted_at is null")
	var baselineIndexSQL string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_local_source_baselines_ownership'
	`).Scan(&baselineIndexSQL))
	assert.Contains(t,
		strings.ToLower(strings.Join(strings.Fields(baselineIndexSQL), " ")),
		"machine, agent, file_path, session_id",
	)
}

func TestSourceMissingStateSurvivesRebuildOrphanCopy(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source := testDBAtPath(t, sourcePath, "source archive")
	physicalPath := filepath.Join(dir, "missing.jsonl")
	insertSessionWithSourcePath(t, source, "session", "claude", physicalPath)
	baselineSessionSource(t, source, defaultMachine, "claude", physicalPath)
	changed, err := source.MarkSessionSourceMissing(
		t.Context(), defaultMachine, "claude", "session", physicalPath,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, source.Close())

	destination := testDBAtPath(t, filepath.Join(dir, "destination.db"), "destination archive")
	defer destination.Close()
	copied, err := destination.CopyOrphanedDataFrom(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, 1, copied)
	assertSessionState(t, destination, "session", false, true)
	active, err := destination.GetSession(t.Context(), "session")
	require.NoError(t, err)
	assert.NotNil(t, active)
}

func TestSessionPushEnumerationKeepsSourceStateLocal(t *testing.T) {
	d := testDB(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	insertSessionWithSourcePath(t, d, "session", "claude", path)
	baselineSessionSource(t, d, defaultMachine, "claude", path)
	changed, err := d.MarkSessionSourceMissing(
		t.Context(), defaultMachine, "claude", "session", path,
	)
	require.NoError(t, err)
	require.True(t, changed)

	full, err := d.GetSessionFull(t.Context(), "session")
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.SourceMissingAt)
	assert.Nil(t, full.DeletionCause)

	sessions, err := d.ListSessionsModifiedBetween(
		t.Context(), "", "", nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.NotNil(t, sessions[0].SourceMissingAt)
	assert.Nil(t, sessions[0].DeletionCause)
}

func TestRebuildMetadataDoesNotRehideReappearedSource(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	physicalPath := filepath.Join(dir, "session.jsonl")
	source := testDBAtPath(t, sourcePath, "source archive")
	insertSessionWithSourcePath(t, source, "session", "claude", physicalPath)
	baselineSessionSource(t, source, defaultMachine, "claude", physicalPath)
	changed, err := source.MarkSessionSourceMissing(
		t.Context(), defaultMachine, "claude", "session", physicalPath,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, source.Close())

	destination := testDBAtPath(t, filepath.Join(dir, "destination.db"), "destination archive")
	defer destination.Close()
	insertSessionWithSourcePath(t, destination, "session", "claude", physicalPath)
	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))
	assertSessionState(t, destination, "session", false, false)
	active, err := destination.GetSession(context.Background(), "session")
	require.NoError(t, err)
	assert.NotNil(t, active)
}

func assertSessionState(
	t *testing.T, d *DB, id string, wantDeleted, wantSourceMissing bool,
) {
	t.Helper()
	var deletedAt, cause, sourceMissingAt sql.NullString
	require.NoError(t, d.getReader().QueryRow(
		"SELECT deleted_at, deletion_cause, source_missing_at "+
			"FROM sessions WHERE id = ?", id,
	).Scan(&deletedAt, &cause, &sourceMissingAt))
	assert.Equal(t, wantDeleted, deletedAt.Valid)
	assert.False(t, cause.Valid)
	assert.Equal(t, wantSourceMissing, sourceMissingAt.Valid)
}
