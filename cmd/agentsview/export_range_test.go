package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExportRangeCanonicalMetadata(t *testing.T) {
	for _, tt := range []struct {
		name string
		seed func(*testing.T)
		want string
	}{
		{
			name: "empty",
			seed: seedExportReportingArchive,
			want: "{\"closed_through\":\"2026-07-29T14:00:00Z\",\"earliest_date\":null,\"schema_version\":1}\n",
		},
		{
			name: "dated archive",
			seed: func(t *testing.T) {
				t.Helper()
				seedExportSessionsArchive(t)
			},
			want: "{\"closed_through\":\"2026-07-29T14:00:00Z\",\"earliest_date\":\"2026-06-01\",\"schema_version\":1}\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.seed(t)
			stdout, stderr, err := executeExportSessionsCommand(newExportReportingTestRoot(
				time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)), "export", "range")
			require.NoError(t, err)
			assert.Empty(t, stderr)
			assert.Equal(t, tt.want, stdout)
		})
	}
}

func TestExportRangeDoesNotUpgradeArchive(t *testing.T) {
	path := filepath.Join(testDataDir(t), "sessions.db")
	database := dbtest.OpenTestDBAt(t, path)
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `DROP TABLE session_project_identity_snapshots`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), "export", "range")
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "range must not migrate an older archive")
}
