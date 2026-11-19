//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
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
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	result, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
	if f.versionColumn == "xmin" {
		roleURL := newUsageJournalRole(t, f.syncer, `GRANT SELECT ON sessions, session_frustration_counts TO ROLE`)
		reader, err := NewStore(f.ctx, roleURL, f.schema, true)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		var one int
		err = reader.DB().QueryRowContext(f.ctx, `SELECT 1 FROM messages LIMIT 1`).Scan(&one)
		require.Error(t, err)
		restarted, err := reader.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		require.Equal(t, 2, restarted.QualityHealth.Totals.FrustrationMarkerCount)
	}
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
}

func TestFrustrationCountsUnavailable(t *testing.T) {
	// An empty database: the missing-table case repairs the schema with its
	// own EnsureSchema run, so the schema must not come from a template copy.
	testPGURL(t)
	t.Run("restricted reader", func(t *testing.T) {
		f := newFrustrationFixture(t, "agentsview_frustration_reader_test")
		if f.versionColumn != "xmin" {
			t.Skip("PostgreSQL roles required")
		}
		_, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
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
		require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
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
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
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
}

func TestFrustrationCountsBulkSaves(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
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
		require.NoError(t, store.storeFrustrationCounts(f.ctx, versions, counts, f.versionColumn))
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

func TestFrustrationEvidenceWithLockedCount(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	_, err := f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id = 'one' FOR UPDATE`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	evidence, err := f.open(t).GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{}, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 2)
	for _, example := range evidence.Sessions {
		require.Equal(t, 1, example.SignalTotal)
		require.Equal(t, "this is broken", example.Excerpt)
	}
}

func TestFrustrationFillLaunchAndPush(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	f.replace(t, "two", "all good")
	store := f.open(t)
	stop := store.StartFrustrationFill(f.ctx)
	defer stop()
	require.Eventually(t, func() bool {
		var total, sum int
		err := f.pg.QueryRowContext(f.ctx, `SELECT count(*), COALESCE(sum(marker_count),0) FROM session_frustration_counts`).Scan(&total, &sum)
		return err == nil && total == 2 && sum == 1
	}, 20*time.Second, 100*time.Millisecond)
	roleURL := newUsageJournalRole(t, f.syncer, `GRANT SELECT ON sessions, session_frustration_counts TO ROLE`)
	reader, err := NewStore(f.ctx, roleURL, f.schema, true)
	require.NoError(t, err)
	defer reader.Close()
	result, err := reader.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, result.QualityHealth.Totals.FrustrationMarkerCount)
	var before string
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT xmin::text FROM session_frustration_counts WHERE session_id='two'`).Scan(&before))
	f.replace(t, "one", "all good")
	require.Eventually(t, func() bool {
		var count int
		err := f.pg.QueryRowContext(f.ctx, `SELECT marker_count FROM session_frustration_counts WHERE session_id='one'`).Scan(&count)
		return err == nil && count == 0
	}, 20*time.Second, 100*time.Millisecond)
	var after string
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT xmin::text FROM session_frustration_counts WHERE session_id='two'`).Scan(&after))
	require.Equal(t, before, after)
	result, err = f.open(t).GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Zero(t, result.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestFrustrationFillJoinsCoveredRequests(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent) SELECT 'filler-' || n, 'test', 'sample', 'claude' FROM generate_series(1, 499) n`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent) VALUES ('outside', 'test', 'sample', 'claude'), ('saved', 'test', 'sample', 'claude')`)
	require.NoError(t, err)
	rows, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	var one, two db.SignalRow
	outside := db.SignalRow{ID: "outside"}
	for _, row := range rows {
		switch row.ID {
		case "one":
			one = row
		case "two":
			two = row
		}
	}
	require.Equal(t, "one", one.ID)
	require.Equal(t, "two", two.ID)
	var outsideVersion string
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT `+frustrationSessionVersion(f.versionColumn)+` FROM sessions WHERE id='outside'`).Scan(&outsideVersion))
	versions["outside"] = outsideVersion
	var savedVersion string
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT `+frustrationSessionVersion(f.versionColumn)+` FROM sessions WHERE id='saved'`).Scan(&savedVersion))
	versions["saved"] = savedVersion
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('saved', $1, $2, 0)`, savedVersion, frustrationMarkerRulesVersion)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) SELECT id, 'stale', 0, 0 FROM sessions WHERE id IN ('one', 'two')`)
	require.NoError(t, err)
	roleURL := newUsageJournalRole(t, f.syncer, `GRANT SELECT ON sessions, session_frustration_counts TO ROLE`)
	reader, err := NewStore(f.ctx, roleURL, f.schema, true)
	require.NoError(t, err)
	defer reader.Close()
	reader.runtime = store.runtimeState()
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	txTwo, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer txTwo.Rollback()
	_, err = txTwo.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='two' FOR UPDATE`)
	require.NoError(t, err)
	ids := []string{"one"}
	for i := 1; i <= 499; i++ {
		ids = append(ids, fmt.Sprintf("filler-%d", i))
	}
	ids = append(ids, "two")
	fillerRows, err := f.pg.QueryContext(f.ctx, `SELECT id, `+frustrationSessionVersion(f.versionColumn)+` FROM sessions WHERE id LIKE 'filler-%'`)
	require.NoError(t, err)
	for fillerRows.Next() {
		var id, version string
		require.NoError(t, fillerRows.Scan(&id, &version))
		versions[id] = version
	}
	require.NoError(t, fillerRows.Err())
	require.NoError(t, fillerRows.Close())
	fillDone := make(chan error, 1)
	go func() { fillDone <- store.fillFrustrationCounts(f.ctx, ids, f.versionColumn) }()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	coveredRows := make([]db.SignalRow, 0, len(ids))
	for _, id := range ids {
		coveredRows = append(coveredRows, db.SignalRow{ID: id})
	}
	coveredDone := make(chan error, 1)
	go func() {
		coveredDone <- reader.populateFrustrationMarkers(f.ctx, coveredRows, versions, f.versionColumn)
	}()
	select {
	case err := <-coveredDone:
		t.Fatalf("covered request returned before fill: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	mixedRows := []db.SignalRow{{ID: "saved"}, one}
	mixedDone := make(chan error, 1)
	go func() { mixedDone <- reader.populateFrustrationMarkers(f.ctx, mixedRows, versions, f.versionColumn) }()
	select {
	case err := <-mixedDone:
		t.Fatalf("request returned before its missing chunk: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	lastRows := []db.SignalRow{two}
	lastDone := make(chan error, 1)
	go func() { lastDone <- store.populateFrustrationMarkers(f.ctx, lastRows, versions, f.versionColumn) }()
	select {
	case err := <-lastDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("request for a queued chunk waited for unrelated work")
	}
	require.Equal(t, 1, lastRows[0].FrustrationMarkerCount)
	outsideRows := []db.SignalRow{outside}
	outsideDone := make(chan error, 1)
	go func() { outsideDone <- store.populateFrustrationMarkers(f.ctx, outsideRows, versions, f.versionColumn) }()
	select {
	case err := <-outsideDone:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond): // The held fill cannot finish.
		t.Fatal("uncovered request waited for the fill")
	}
	require.Zero(t, outsideRows[0].FrustrationMarkerCount)
	require.NoError(t, tx.Rollback())
	require.NoError(t, <-mixedDone)
	require.Zero(t, mixedRows[0].FrustrationMarkerCount)
	require.Equal(t, 1, mixedRows[1].FrustrationMarkerCount)
	oneRows := []db.SignalRow{one}
	oneDone := make(chan error, 1)
	go func() { oneDone <- reader.populateFrustrationMarkers(f.ctx, oneRows, versions, f.versionColumn) }()
	select {
	case err := <-oneDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("request for the completed chunk waited for another chunk")
	}
	require.Equal(t, 1, oneRows[0].FrustrationMarkerCount)
	select {
	case err := <-coveredDone:
		t.Fatalf("request spanning chunks returned while the second was held: %v", err)
	default:
	}
	require.NoError(t, txTwo.Rollback())
	require.NoError(t, <-fillDone)
	require.NoError(t, <-coveredDone)
	require.Equal(t, 1, coveredRows[0].FrustrationMarkerCount)
	require.Equal(t, 1, coveredRows[len(coveredRows)-1].FrustrationMarkerCount)
}

func TestFrustrationFillTimeoutAndRetry(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	rows, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	var one db.SignalRow
	for _, row := range rows {
		if row.ID == "one" {
			one = row
		}
	}
	require.Equal(t, "one", one.ID)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('one', 'stale', 0, 0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	stop := store.StartFrustrationFill(f.ctx)
	defer stop()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	requestRows := []db.SignalRow{one}
	require.NoError(t, store.populateFrustrationMarkers(ctx, requestRows, versions, f.versionColumn))
	require.NoError(t, ctx.Err())
	require.Equal(t, 1, requestRows[0].FrustrationMarkerCount)
	require.NoError(t, tx.Rollback())
	require.Eventually(t, func() bool {
		var count int
		err := f.pg.QueryRowContext(f.ctx, `SELECT marker_count FROM session_frustration_counts WHERE session_id='one' AND rules_version=$1`, frustrationMarkerRulesVersion).Scan(&count)
		return err == nil && count == 1
	}, 20*time.Second, 100*time.Millisecond)
}

func TestFrustrationFillJournalRecovery(t *testing.T) {
	testPGURL(t)
	for _, scenario := range []string{"absent", "oid", "revision"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newFrustrationFixture(t, "frustration_journal_"+scenario)
			store := f.open(t)
			if scenario == "absent" {
				_, err := f.pg.ExecContext(f.ctx, dropUsageChangeJournalSQL)
				require.NoError(t, err)
			}
			stop := store.StartFrustrationFill(f.ctx)
			defer stop()
			if scenario != "absent" {
				require.Eventually(t, func() bool {
					var count int
					return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts`).Scan(&count) == nil && count == 2
				}, 20*time.Second, 100*time.Millisecond)
				_, err := f.pg.ExecContext(f.ctx, `DELETE FROM session_frustration_counts`)
				require.NoError(t, err)
			}
			switch scenario {
			case "absent", "oid":
				if scenario == "oid" {
					_, err := f.pg.ExecContext(f.ctx, dropUsageChangeJournalSQL)
					require.NoError(t, err)
				}
				require.True(t, ensureUsageChangeJournalPG(f.ctx, f.pg))
			case "revision":
				_, err := f.pg.ExecContext(f.ctx, `UPDATE sync_metadata SET value='1' WHERE key='usage_change_revision'`)
				require.NoError(t, err)
			}
			require.Eventually(t, func() bool {
				var count int
				return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1`).Scan(&count) == nil && count == 2
			}, 20*time.Second, 100*time.Millisecond)
		})
	}
}

func TestFrustrationFillCanceledRequest(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	rows, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) SELECT id, 'stale', 0, 0 FROM sessions`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	fillDone := make(chan error, 1)
	go func() { fillDone <- store.fillFrustrationCounts(f.ctx, []string{"one", "two"}, f.versionColumn) }()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	require.ErrorIs(t, store.populateFrustrationMarkers(ctx, rows, versions, f.versionColumn), context.Canceled)
	require.NoError(t, tx.Rollback())
	require.NoError(t, <-fillDone)
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1 AND rules_version=$1`, frustrationMarkerRulesVersion).Scan(&count))
	require.Equal(t, 2, count)
}

func TestFrustrationFillDeploymentGates(t *testing.T) {
	testPGURL(t)
	for _, scenario := range []string{"read-only", "hosted", "borrowed", "standby"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			if scenario == "standby" && os.Getenv("TEST_PG_STANDBY_URL") == "" {
				t.Skip("TEST_PG_STANDBY_URL must point to a standby of TEST_PG_URL")
			}
			f := newFrustrationFixture(t, "frustration_gate_"+strings.ReplaceAll(scenario, "-", "_"))
			owner := f.open(t)
			var store *Store
			var role string
			switch scenario {
			case "read-only":
				url := newUsageJournalRole(t, f.syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+f.schema+` TO ROLE`)
				var err error
				store, err = NewStore(f.ctx, url, f.schema, true)
				require.NoError(t, err)
				defer store.Close()
				require.NoError(t, store.pg.QueryRowContext(f.ctx, `SELECT current_user`).Scan(&role))
			case "hosted":
				store = &Store{pg: f.pg, hostedRelations: true}
			case "borrowed":
				tx, err := f.pg.BeginTx(f.ctx, nil)
				require.NoError(t, err)
				defer tx.Rollback()
				store = owner.readView(tx)
			case "standby":
				url := testPGStandbyURL(t)
				connection, err := sql.Open("pgx", url)
				require.NoError(t, err)
				defer connection.Close()
				var lsn string
				require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn))
				require.Eventually(t, func() bool {
					var caughtUp bool
					return connection.QueryRowContext(f.ctx, `SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn`, lsn).Scan(&caughtUp) == nil && caughtUp
				}, 20*time.Second, 50*time.Millisecond)
				store, err = NewStore(f.ctx, url, f.schema, true)
				require.NoError(t, err)
				defer store.Close()
				writable, err := store.canFillFrustrationCounts(f.ctx)
				require.NoError(t, err)
				require.False(t, writable)
			}
			stop := store.StartFrustrationFill(f.ctx)
			defer stop()
			select {
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			case <-time.After(6 * time.Second):
			}
			var count int
			require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts`).Scan(&count))
			require.Zero(t, count)
			if scenario == "read-only" {
				_, err := f.pg.ExecContext(f.ctx, `GRANT INSERT, UPDATE ON session_frustration_counts TO `+`"`+role+`"`)
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1`).Scan(&count) == nil && count == 2
				}, 20*time.Second, 100*time.Millisecond)
			}
		})
	}
}

func TestFrustrationFillSlowChunkAllowsProgress(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent) SELECT 'filler-' || n, 'test', 'sample', 'claude' FROM generate_series(1, 499) n`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('one', 'stale', 0, 0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	ids := []string{"one"}
	for i := 1; i <= 499; i++ {
		ids = append(ids, fmt.Sprintf("filler-%d", i))
	}
	ids = append(ids, "two")
	require.ErrorIs(t, store.fillFrustrationCounts(f.ctx, ids, f.versionColumn), context.DeadlineExceeded)
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT marker_count FROM session_frustration_counts WHERE session_id='two'`).Scan(&count))
	require.Equal(t, 1, count)
	retryDone := make(chan error, 1)
	go func() { retryDone <- store.fillFrustrationCounts(f.ctx, []string{"one"}, f.versionColumn) }()
	select {
	case err := <-retryDone:
		t.Fatalf("retry ended before the extended timeout: %v", err)
	case <-time.After(6 * time.Second):
	}
	require.NoError(t, tx.Rollback())
	require.NoError(t, <-retryDone)
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT marker_count FROM session_frustration_counts WHERE session_id='one'`).Scan(&count))
	require.Equal(t, 1, count)
}
