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
	syncer := &Sync{pg: pg, local: local, machine: "machine-a", schema: schema, schemaDone: true, projects: []string{"project-a"}, targetFingerprint: "target-a"}
	require.NoError(t, syncer.syncDiagnosticEvents(t.Context(), false, "empty"))
	var markerCount int
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT count(*) FROM sync_metadata WHERE key LIKE 'diagnostic_marker:%'`).Scan(&markerCount))
	assert.Zero(t, markerCount)
	when := time.Date(2026, 1, 1, 23, 55, 0, 0, time.UTC)
	_, err = local.AppendDiagnostic(t.Context(), db.DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: when})
	require.NoError(t, err)
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
	_, err = runner.CatchUp(t.Context())
	require.NoError(t, err)
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

func TestDiagnosticTieOrder(t *testing.T) {
	const schema = "agentsview_diagnostic_order_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))
	for _, id := range []string{"00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000001"} {
		_, err := pg.ExecContext(t.Context(), `INSERT INTO diagnostic_events(id,source,message,occurred_at,occurred_at_exact,accepted_at) VALUES($1,'worker','failure','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`, id)
		require.NoError(t, err)
	}
	signals, err := (&Store{pg: pg}).DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	require.Len(t, signals, 2)
	assert.Equal(t, "diagnostic:00000000-0000-0000-0000-000000000001", signals[0].SubjectID)
	assert.Equal(t, "diagnostic:00000000-0000-0000-0000-000000000002", signals[1].SubjectID)
}

func TestDiagnosticReadsWithoutTable(t *testing.T) {
	const schema = "agentsview_diagnostic_missing_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))
	_, err = pg.ExecContext(t.Context(), `DROP TABLE diagnostic_events`)
	require.NoError(t, err)
	store := &Store{pg: pg}
	dates, err := store.FrictionPendingDiagnosticDates(t.Context(), "2026-01-02", time.UTC)
	require.NoError(t, err)
	assert.Empty(t, dates)
	signals, err := store.DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	assert.Empty(t, signals)

	require.NoError(t, EnsureSchema(t.Context(), pg, schema))
	_, err = pg.ExecContext(t.Context(), `INSERT INTO diagnostic_events(id,source,message,occurred_at,occurred_at_exact,accepted_at) VALUES('00000000-0000-0000-0000-000000000001','worker','failure','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`)
	require.NoError(t, err)
	dates, err = store.FrictionPendingDiagnosticDates(t.Context(), "2026-01-02", time.UTC)
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-01-01"}, dates)
	signals, err = store.DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	require.Len(t, signals, 1)
	assert.Equal(t, "failure", signals[0].Text)
}

func TestDiagnosticPushConflictingArchives(t *testing.T) {
	const schema = "agentsview_diagnostic_conflict_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))

	event := db.DiagnosticEvent{ID: uuid.NewV7().String(), Source: "worker", Message: "launch failed", OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)}
	for _, tc := range []struct {
		name     string
		event    db.DiagnosticEvent
		conflict bool
	}{
		{name: "original", event: event},
		{name: "retry", event: event},
		{name: "different message", event: db.DiagnosticEvent{ID: event.ID, Source: event.Source, Message: "different", OccurredAt: event.OccurredAt}, conflict: true},
		{name: "different nanosecond", event: db.DiagnosticEvent{ID: event.ID, Source: event.Source, Message: event.Message, OccurredAt: event.OccurredAt.Add(time.Nanosecond)}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = local.Close() })
			_, err = local.AppendDiagnostic(t.Context(), tc.event)
			require.NoError(t, err)
			syncer := &Sync{pg: pg, local: local, targetFingerprint: "target"}
			err = syncer.syncDiagnosticEvents(t.Context(), false, tc.name)
			if tc.conflict {
				require.ErrorIs(t, err, db.ErrDiagnosticConflict)
				var marker bool
				require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM sync_metadata WHERE key=$1)`, "diagnostic_marker:"+tc.name+":").Scan(&marker))
				assert.False(t, marker)
			} else {
				require.NoError(t, err)
			}
		})
	}
	assert.Equal(t, 1, pgTableCount(t, t.Context(), pg, "diagnostic_events"))
}
