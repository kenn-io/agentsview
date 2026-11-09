//go:build pgtest

package postgres

import (
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/review"
)

func TestDiagnosticPushFilteredAndReset(t *testing.T) {
	const schema = "agentsview_diagnostics_inbox_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	when := time.Date(2026, 1, 1, 23, 55, 0, 0, time.UTC)
	_, err = local.AppendDiagnostic(t.Context(), db.DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: when})
	require.NoError(t, err)
	syncer := &Sync{pg: pg, local: local, machine: "machine-a", schema: schema, schemaDone: true, projects: []string{"project-a"}, targetFingerprint: "target-a"}
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, pgTableCount(t, t.Context(), pg, "diagnostic_events"))
	store := &Store{pg: pg}
	signals, err := store.DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	require.Len(t, signals, 1)
	assert.Equal(t, "worker", signals[0].Label)
	runner := &review.Runner{Store: store, Diagnostics: store, Loc: time.UTC, Now: func() time.Time { return when.Add(48 * time.Hour) }, BackfillDays: 7}
	reports, err := runner.CatchUp(t.Context())
	require.NoError(t, err)
	for _, report := range reports {
		assert.Empty(t, report.Snapshot.P0Alerts)
	}
	digest, err := store.GetFrictionDigest(t.Context(), "2026-01-01")
	require.NoError(t, err)
	require.NotNil(t, digest)
	assert.Contains(t, string(digest.Markdown), "`worker`: `launch failed` x1")
	assert.Zero(t, digest.SessionsScanned)
	_, err = pg.ExecContext(t.Context(), `DELETE FROM diagnostic_events`)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key LIKE 'diagnostic_marker:%'`)
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, pgTableCount(t, t.Context(), pg, "diagnostic_events"))
}
