//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestFrustrationCountsRestartAndRefresh(t *testing.T) {
	const schema = "agentsview_frustration_counts_test"
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
	open := func() *Store {
		store, err := NewStore(ctx, url, schema, true)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		return store
	}
	store := open()
	versionColumn, err := sessionVersionColumnPG(ctx, store.pg)
	require.NoError(t, err)
	first, err := open().GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, first.QualityHealth.Totals.FrustrationMarkerCount)
	var n int
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM session_frustration_counts`).Scan(&n))
	require.Equal(t, 2, n)
	// A distinct stored count proves the fresh store uses the saved row.
	_, err = pg.ExecContext(ctx, `UPDATE session_frustration_counts SET marker_count = 7 WHERE session_id = 'one'`)
	require.NoError(t, err)
	fresh, err := open().GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 8, fresh.QualityHealth.Totals.FrustrationMarkerCount)
	_, err = pg.ExecContext(ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
	require.NoError(t, err)
	refreshed, err := open().GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, refreshed.QualityHealth.Totals.FrustrationMarkerCount)

	// After a push, a fresh store recounts the changed session and reuses the other.
	_, err = pg.ExecContext(ctx, `UPDATE session_frustration_counts SET marker_count = 7 WHERE session_id = 'one'`)
	require.NoError(t, err)
	session := db.Session{ID: "two", Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1}
	messages := []db.Message{{SessionID: "two", Ordinal: 0, Role: "user", Content: "Please explain this function."}}
	_, err = local.WriteSessionBatchContext(ctx, []db.SessionBatchWrite{{Session: session, Messages: messages, ReplaceMessages: true}})
	require.NoError(t, err)
	require.NoError(t, local.UpdateSessionSignals(ctx, "two", db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	afterPush, err := open().GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 7, afterPush.QualityHealth.Totals.FrustrationMarkerCount)

	// Restore the fixture for the filter and fallback checks below.
	_, err = pg.ExecContext(ctx, `UPDATE session_frustration_counts SET marker_count = 1 WHERE session_id = 'one'`)
	require.NoError(t, err)
	messages[0].Content = "this is broken"
	_, err = local.WriteSessionBatchContext(ctx, []db.SessionBatchWrite{{Session: session, Messages: messages, ReplaceMessages: true}})
	require.NoError(t, err)
	require.NoError(t, local.UpdateSessionSignals(ctx, "two", db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	_, err = open().GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	filter := db.AnalyticsFilter{}
	cached := open()
	actual, err := cached.GetAnalyticsSignals(ctx, filter)
	require.NoError(t, err)
	evidence, err := cached.GetAnalyticsSignalSessions(ctx, filter, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 2)
	ids := make([]string, 0, len(evidence.Sessions))
	for _, example := range evidence.Sessions {
		ids = append(ids, example.SessionID)
	}
	require.ElementsMatch(t, []string{"one", "two"}, ids)
	_, err = pg.ExecContext(ctx, `DELETE FROM session_frustration_counts`)
	require.NoError(t, err)
	scanning := open()
	expected, err := scanning.GetAnalyticsSignals(ctx, filter)
	require.NoError(t, err)
	wantEvidence, err := scanning.GetAnalyticsSignalSessions(ctx, filter, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
	require.Equal(t, wantEvidence, evidence)

	// A restricted reader can reuse one row and scan the stale one.
	_, err = pg.ExecContext(ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
	require.NoError(t, err)
	if versionColumn != "crdb_internal_mvcc_timestamp" {
		_, err = pg.ExecContext(ctx, `UPDATE session_frustration_counts SET marker_count = 7 WHERE session_id = 'two'`)
		require.NoError(t, err)
		roleURL := newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+schema+` TO ROLE`)
		reader, err := NewStore(ctx, roleURL, schema, true)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		restricted, err := reader.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		require.Equal(t, 8, restricted.QualityHealth.Totals.FrustrationMarkerCount)
		require.True(t, reader.runtimeState().frustrationCountsReadOnly.Load())
		var version int
		require.NoError(t, pg.QueryRowContext(ctx, `SELECT rules_version FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&version))
		require.Zero(t, version)
	}

	// A borrowed snapshot must still work when the optional table is absent.
	_, err = pg.ExecContext(ctx, `DROP TABLE session_frustration_counts`)
	require.NoError(t, err)
	owner := open()
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	view := owner.readView(tx)
	borrowed, err := view.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, borrowed.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, tx.Rollback())
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	_, err = open().GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'one'`)
	require.NoError(t, err)
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM session_frustration_counts`).Scan(&n))
	require.Equal(t, 1, n)
}
