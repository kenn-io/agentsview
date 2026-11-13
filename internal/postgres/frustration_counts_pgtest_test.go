//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type frustrationFixture struct {
	syncer                     *Sync
	local                      *db.DB
	pg                         *sql.DB
	ctx                        context.Context
	url, schema, versionColumn string
}

func newFrustrationFixture(t *testing.T, schema string) *frustrationFixture {
	t.Helper()
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, schema)
	url := testPGURL(t)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	for _, id := range []string{"one", "two"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1}))
		require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "this is broken"}}))
		require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	}
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	column, err := sessionVersionColumnPG(ctx, pg)
	require.NoError(t, err)
	return &frustrationFixture{syncer, local, pg, ctx, url, schema, column}
}

func (f *frustrationFixture) open(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(f.ctx, f.url, f.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func (f *frustrationFixture) replace(t *testing.T, id, content string) {
	t.Helper()
	session := db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1}
	_, err := f.local.WriteSessionBatchContext(f.ctx, []db.SessionBatchWrite{{Session: session, Messages: []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: content}}, ReplaceMessages: true}})
	require.NoError(t, err)
	require.NoError(t, f.local.UpdateSessionSignals(f.ctx, id, db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	_, err = f.syncer.Push(f.ctx, false, nil)
	require.NoError(t, err)
}

func TestFrustrationCountsRulesAndRestart(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_rules_test")
	result, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = $1, marker_count = 7 WHERE session_id = 'one'`, frustrationMarkerRulesVersion+1)
	require.NoError(t, err)
	result, err = f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
	var version, count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT rules_version, marker_count FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&version, &count))
	require.Equal(t, frustrationMarkerRulesVersion+1, version)
	require.Equal(t, 7, count)
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
	require.NoError(t, err)
	result, err = f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT rules_version, marker_count FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&version, &count))
	require.Equal(t, frustrationMarkerRulesVersion, version)
	require.Equal(t, 1, count)

	f.replace(t, "two", "Please explain this function.")
	result, err = f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, result.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestFrustrationCountsUnavailable(t *testing.T) {
	t.Run("restricted reader", func(t *testing.T) {
		f := newFrustrationFixture(t, "agentsview_frustration_reader_test")
		if f.versionColumn != "xmin" {
			t.Skip("PostgreSQL roles required")
		}
		_, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
		require.NoError(t, err)
		_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET marker_count = 7 WHERE session_id = 'two'`)
		require.NoError(t, err)
		roleURL := newUsageJournalRole(t, f.syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+f.schema+` TO ROLE`)
		reader, err := NewStore(f.ctx, roleURL, f.schema, true)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)
		result, err := reader.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		require.Equal(t, 8, result.QualityHealth.Totals.FrustrationMarkerCount)
		require.NotContains(t, output.String(), "storing frustration counts")
	})
	t.Run("missing table and borrowed snapshot", func(t *testing.T) {
		f := newFrustrationFixture(t, "agentsview_frustration_missing_test")
		owner := f.open(t)
		tx, err := f.pg.BeginTx(f.ctx, nil)
		require.NoError(t, err)
		result, err := owner.readView(tx).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
		require.NoError(t, tx.Rollback())
		var count int
		require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts`).Scan(&count))
		require.Zero(t, count)
		_, err = f.pg.ExecContext(f.ctx, `DROP TABLE session_frustration_counts`)
		require.NoError(t, err)
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)
		result, err = f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
		require.NotContains(t, output.String(), "storing frustration counts")
		require.NotContains(t, output.String(), "loading frustration counts")
		f.syncer.schemaDone = false
		require.NoError(t, f.syncer.EnsureSchema(f.ctx))
		var exists bool
		require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT to_regclass('session_frustration_counts') IS NOT NULL`).Scan(&exists))
		require.True(t, exists)
	})
}

func TestFrustrationCountsDelayedSave(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_delayed_test")
	store := f.open(t)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	f.replace(t, "one", "Please explain this function.")
	_, err = f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `DELETE FROM session_frustration_counts WHERE session_id = 'two'`)
	require.NoError(t, err)
	require.NoError(t, store.storeFrustrationCounts(f.ctx, versions, map[string]int{"one": 9, "two": 1}, f.versionColumn))
	var saved string
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT session_version, marker_count FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&saved, &count))
	require.NotEqual(t, versions["one"], saved)
	require.Zero(t, count)
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT session_version, marker_count FROM session_frustration_counts WHERE session_id = 'two'`).Scan(&saved, &count))
	require.Equal(t, versions["two"], saved)
	require.Equal(t, 1, count)
	result, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, result.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, f.local.DeleteSession(f.ctx, "one"))
	_, err = f.syncer.Push(f.ctx, true, nil)
	require.NoError(t, err)
	require.NoError(t, store.storeFrustrationCounts(f.ctx, versions, map[string]int{"one": 9, "two": 1}, f.versionColumn))
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&count))
	require.Zero(t, count)
}

func TestFrustrationCountsBulkSaves(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_bulk_test")
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent) SELECT 'bulk-' || n, 'test', 'sample', 'claude' FROM generate_series(1, 10000) n`)
	require.NoError(t, err)
	rows, err := f.pg.QueryContext(f.ctx, `SELECT id, `+frustrationSessionVersion(f.versionColumn)+` FROM sessions WHERE id LIKE 'bulk-%'`)
	require.NoError(t, err)
	versions := make(map[string]string, 10000)
	counts := make(map[string]int, 10000)
	for rows.Next() {
		var id, version string
		require.NoError(t, rows.Scan(&id, &version))
		versions[id] = version
		counts[id] = 1
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, versions, 10000)
	store := f.open(t)
	t.Run("refresh", func(t *testing.T) {
		_, err := f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts (session_id, session_version, rules_version, marker_count) SELECT id, 'old', $1, 0 FROM sessions WHERE id LIKE 'bulk-%'`, frustrationMarkerRulesVersion)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
		defer cancel()
		require.NoError(t, store.storeFrustrationCounts(ctx, versions, counts, f.versionColumn))
		var refreshed int
		require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE session_id LIKE 'bulk-%' AND marker_count = 1 AND session_version <> 'old'`).Scan(&refreshed))
		require.Equal(t, 10000, refreshed)
	})
	t.Run("concurrent cold saves", func(t *testing.T) {
		_, err := f.pg.ExecContext(f.ctx, `DELETE FROM session_frustration_counts`)
		require.NoError(t, err)
		start := make(chan struct{})
		results := make(chan error, 8)
		for range 8 {
			go func() {
				<-start
				results <- store.storeFrustrationCounts(f.ctx, versions, counts, f.versionColumn)
			}()
		}
		close(start)
		for range 8 {
			assert.NoError(t, <-results)
		}
		var count int
		require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count = 1`).Scan(&count))
		require.Equal(t, len(versions), count)
	})
}
