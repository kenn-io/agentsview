//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"testing"
	"time"

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
}

func TestFrustrationCountsAfterPush(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_push_test")
	_, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET marker_count = 7 WHERE session_id = 'one'`)
	require.NoError(t, err)
	f.replace(t, "two", "Please explain this function.")
	result, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 7, result.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestFrustrationCountsEvidence(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_evidence_test")
	filter := db.AnalyticsFilter{Project: "sample"}
	_, err := f.open(t).GetAnalyticsSignals(f.ctx, filter)
	require.NoError(t, err)
	evidence, err := f.open(t).GetAnalyticsSignalSessions(f.ctx, filter, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 2)
	for _, example := range evidence.Sessions {
		require.Equal(t, 1, example.SignalTotal)
		require.Equal(t, "this is broken", example.Excerpt)
	}
	_, err = f.pg.ExecContext(f.ctx, `DELETE FROM session_frustration_counts`)
	require.NoError(t, err)
	scanned, err := f.open(t).GetAnalyticsSignalSessions(f.ctx, filter, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Equal(t, scanned, evidence)
}

func TestFrustrationCountsRestrictedReader(t *testing.T) {
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
}

func TestFrustrationCountsMissingAndBorrowed(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_missing_test")
	owner := f.open(t)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	result, err := owner.readView(tx).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, tx.Rollback())
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
	require.NoError(t, f.local.DeleteSession(f.ctx, "one"))
	_, err = f.syncer.Push(f.ctx, true, nil)
	require.NoError(t, err)
	require.NoError(t, store.storeFrustrationCounts(f.ctx, versions, map[string]int{"one": 9, "two": 1}, f.versionColumn))
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&count))
	require.Zero(t, count)
}

func TestFrustrationCountsWaitingSave(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview_frustration_waiting_test")
	if f.versionColumn != "xmin" {
		t.Skip("PostgreSQL lock inspection required")
	}
	store := f.open(t)
	_, err := store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `UPDATE session_frustration_counts SET marker_count = 1 WHERE session_id = 'one'`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		done <- store.storeFrustrationCounts(f.ctx, map[string]string{"one": versions["one"]}, map[string]int{"one": 9}, f.versionColumn)
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := f.pg.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%WITH candidates AS MATERIALIZED%')`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	_, err = tx.ExecContext(f.ctx, `UPDATE sessions SET updated_at = updated_at + interval '1 second' WHERE id = 'one'`)
	require.NoError(t, err)
	var newer string
	require.NoError(t, tx.QueryRowContext(f.ctx, `SELECT `+frustrationSessionVersion(f.versionColumn)+` FROM sessions WHERE id = 'one'`).Scan(&newer))
	_, err = tx.ExecContext(f.ctx, `UPDATE session_frustration_counts SET session_version = $1, marker_count = 0 WHERE session_id = 'one'`, newer)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
	var saved string
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT session_version, marker_count FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&saved, &count))
	require.Equal(t, newer, saved)
	require.Zero(t, count)
}
