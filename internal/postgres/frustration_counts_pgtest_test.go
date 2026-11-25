//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func waitFrustrationSave(store *Store) {
	store.runtimeState().frustrationSaves.wg.Wait()
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
	store := f.open(t)
	result, err := store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	waitFrustrationSave(store)
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
	store = f.open(t)
	result, err = store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	waitFrustrationSave(store)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
	var version, count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT rules_version, marker_count FROM session_frustration_counts WHERE session_id = 'one'`).Scan(&version, &count))
	require.Equal(t, frustrationMarkerRulesVersion+1, version)
	require.Equal(t, 7, count)
	stale, err := f.open(t).staleFrustrationSessions(f.ctx, f.versionColumn)
	require.NoError(t, err)
	require.NotContains(t, stale, "one")
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'one'`)
	require.NoError(t, err)
	stale, err = f.open(t).staleFrustrationSessions(f.ctx, f.versionColumn)
	require.NoError(t, err)
	require.Contains(t, stale, "one")
	store = f.open(t)
	result, err = store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	waitFrustrationSave(store)
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
		store := f.open(t)
		_, err := store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		waitFrustrationSave(store)
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
		waitFrustrationSave(reader)
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
		store := f.open(t)
		result, err = store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		require.NoError(t, err)
		waitFrustrationSave(store)
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
	writer := f.open(t)
	_, err = writer.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	waitFrustrationSave(writer)
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

func TestFrustrationSavesWithLockedCount(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	require.NoError(t, f.local.UpsertSession(f.ctx, db.Session{ID: "three", Agent: "claude", Project: "other", Machine: "workstation", MessageCount: 1}))
	require.NoError(t, f.local.InsertMessages(f.ctx, []db.Message{{SessionID: "three", Ordinal: 0, Role: "user", Content: "this is broken"}}))
	require.NoError(t, f.local.UpdateSessionSignals(f.ctx, "three", db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	_, err := f.syncer.Push(f.ctx, false, nil)
	require.NoError(t, err)
	initial := f.open(t)
	_, err = initial.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	waitFrustrationSave(initial)
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id = 'one' FOR UPDATE`)
	require.NoError(t, err)
	store := f.open(t)
	store.DB().SetMaxOpenConns(15)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{Project: "sample"}, f.versionColumn)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(f.ctx, 500*time.Millisecond)
	evidence, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{Project: "sample"}, "frustration_marker_count", 10)
	cancel()
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 2)
	for _, session := range evidence.Sessions {
		assert.Equal(t, 1, session.SignalTotal)
		assert.Equal(t, "this is broken", session.Excerpt)
	}
	if f.versionColumn == "xmin" {
		var blocker int
		require.NoError(t, tx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&blocker))
		require.Eventually(t, func() bool {
			var blocked bool
			err := f.pg.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked)
			return err == nil && blocked
		}, 10*time.Second, 10*time.Millisecond)
	}
	for range 15 {
		store.storeFrustrationCountsForRead(versions, map[string]int{"one": 1, "two": 1}, f.versionColumn)
	}
	for range 15 {
		store.storeFrustrationCountsForRead(map[string]string{"one": "older"}, map[string]int{"one": 1}, f.versionColumn)
	}
	queue := &store.runtimeState().frustrationSaves
	queue.mu.Lock()
	queued := len(queue.queued)
	pending := len(queue.pending)
	_, older := queue.queued[frustrationSaveKey{"one", "older", f.versionColumn}]
	queue.mu.Unlock()
	require.Equal(t, 3, queued)
	if f.versionColumn == "xmin" {
		require.Equal(t, 1, pending)
	}
	require.True(t, older)
	require.Never(t, func() bool { return store.DB().Stats().InUse >= 15 }, time.Second, 10*time.Millisecond)
	ctx, cancel = context.WithTimeout(f.ctx, 500*time.Millisecond)
	defer cancel()
	result, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: "other"})
	require.NoError(t, err)
	require.Equal(t, 1, result.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, tx.Rollback())
	waitFrustrationSave(store)
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE rules_version = $1 AND marker_count = 1`, frustrationMarkerRulesVersion).Scan(&count))
	require.Equal(t, 3, count)
	restarted := f.open(t)
	counter := &signalRowsCounter{storeExecutor: restarted.pg}
	restarted.pg = counter
	_, err = restarted.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Zero(t, counter.scans.Load())
}

func TestFrustrationSaveYieldsSingleConnection(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("lock wait inspection requires PostgreSQL")
	}
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	store.DB().SetMaxOpenConns(1)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	var blocker int
	require.NoError(t, tx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&blocker))
	store.storeFrustrationCountsForRead(versions, map[string]int{"one": 1, "two": 1}, f.versionColumn)
	require.Eventually(t, func() bool {
		var blocked bool
		err := f.pg.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked)
		return err == nil && blocked
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	evidence, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{}, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 2)
	require.NoError(t, tx.Rollback())
	waitFrustrationSave(store)
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE rules_version=$1 AND marker_count=1`, frustrationMarkerRulesVersion).Scan(&count))
	require.Equal(t, 2, count)
}

func TestFrustrationFillYieldsSingleConnection(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("lock wait inspection requires PostgreSQL")
	}
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	store.DB().SetMaxOpenConns(1)
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	var blocker int
	require.NoError(t, tx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&blocker))
	done := make(chan error, 1)
	go func() { done <- store.fillFrustrationCounts(f.ctx, []string{"one", "two"}, f.versionColumn) }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := f.pg.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked)
		return err == nil && blocked
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	evidence, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{}, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 2)
	require.ErrorContains(t, <-done, "lock timeout")
	require.NoError(t, tx.Rollback())
	waitFrustrationSave(store)
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE rules_version=$1 AND marker_count=1`, frustrationMarkerRulesVersion).Scan(&count))
	require.Equal(t, 2, count)
}

func TestFrustrationSaveShutdown(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("lock wait inspection requires PostgreSQL")
	}
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	var blocker int
	require.NoError(t, tx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&blocker))
	store.storeFrustrationCountsForRead(versions, map[string]int{"one": 1, "two": 1}, f.versionColumn)
	require.Eventually(t, func() bool {
		var blocked bool
		err := f.pg.QueryRowContext(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked)
		return err == nil && blocked
	}, 10*time.Second, 10*time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("close waited for the locked save")
	}
}

func TestFrustrationSaveRetriesAfterTimeout(t *testing.T) {
	t.Parallel()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	store.storeFrustrationCountsForRead(map[string]string{"one": versions["one"]}, map[string]int{"one": 1}, f.versionColumn)
	store.storeFrustrationCountsForRead(map[string]string{"two": versions["two"]}, map[string]int{"two": 1}, f.versionColumn)
	require.Eventually(t, func() bool {
		var count int
		err := f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE session_id='two' AND rules_version=$1`, frustrationMarkerRulesVersion).Scan(&count)
		return err == nil && count == 1
	}, frustrationFillChunkTimeout+15*time.Second, 100*time.Millisecond)
	require.NoError(t, tx.Rollback())
	waitFrustrationSave(store)
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE rules_version=$1 AND marker_count=1`, frustrationMarkerRulesVersion).Scan(&count))
	require.Equal(t, 2, count)
}

func TestFrustrationFillWaitsForUsageWarm(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server, _ := testPGServer(t)
		if server.cockroach {
			t.Skip("Usage warm-up requires PostgreSQL")
		}
		f := newFrustrationFixture(t, "frustration_wait_usage")
		store := f.open(t)
		_, err := f.pg.ExecContext(f.ctx, `INSERT INTO usage_events(session_id,source,model,output_tokens,occurred_at) VALUES ('one','provider','model',20,'2026-01-01')`)
		require.NoError(t, err)
		probe := attachUsageCacheProbe(t, store, t.TempDir())
		store.usageCache.DeferWork()
		started, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		probe.before = func(ctx context.Context, _ []string) error {
			select {
			case <-started:
			default:
				close(started)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		require.NoError(t, store.WarmUsageCache(f.ctx))
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("Usage warm-up did not start")
		}
		stop := store.StartFrustrationFill(f.ctx)
		defer func() { unblock(); stop() }()
		time.Sleep(500 * time.Millisecond)
		var count int
		require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts`).Scan(&count))
		require.Zero(t, count, "fill started while Usage was warming")
		unblock()
		require.Eventually(t, func() bool {
			return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1`).Scan(&count) == nil && count == 2
		}, 20*time.Second, 100*time.Millisecond)
	})
	t.Run("background failure", func(t *testing.T) {
		server, _ := testPGServer(t)
		if server.cockroach {
			t.Skip("Usage warm-up requires PostgreSQL")
		}
		f := newFrustrationFixture(t, "agentsview")
		store := f.open(t)
		_, err := f.pg.ExecContext(f.ctx, `INSERT INTO usage_events(session_id,source,model,output_tokens,occurred_at) VALUES ('one','provider','model',20,'2026-01-01')`)
		require.NoError(t, err)
		probe := attachUsageCacheProbe(t, store, t.TempDir())
		store.usageCache.SetWarmRetryDelayForTest(time.Minute)
		store.usageCache.DeferWork()
		failed := make(chan struct{})
		var once sync.Once
		probe.before = func(context.Context, []string) error {
			once.Do(func() { close(failed) })
			return fmt.Errorf("usage source failed")
		}
		require.NoError(t, store.WarmUsageCache(f.ctx))
		stop := store.StartFrustrationFill(f.ctx)
		defer stop()
		select {
		case <-failed:
		case <-time.After(10 * time.Second):
			t.Fatal("Usage warm-up did not reach the failing source")
		}
		require.Eventually(t, func() bool {
			var count int
			return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1`).Scan(&count) == nil && count == 2
		}, 5*time.Second, 100*time.Millisecond)
		ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, store.usageCache.WaitWarm(ctx), context.DeadlineExceeded)
	})
	t.Run("failure", func(t *testing.T) {
		server, _ := testPGServer(t)
		if server.cockroach {
			t.Skip("Usage warm-up requires PostgreSQL")
		}
		f := newFrustrationFixture(t, "agentsview")
		store := f.open(t)
		path := filepath.Join(t.TempDir(), "unavailable")
		require.NoError(t, os.WriteFile(path, []byte("unavailable"), 0600))
		attachUsageCacheProbe(t, store, path)
		store.usageCache.SetWarmRetryDelayForTest(time.Second)
		store.usageCache.DeferWork()
		t.Setenv("TMPDIR", path)
		require.Error(t, store.WarmUsageCache(f.ctx))
		stop := store.StartFrustrationFill(f.ctx)
		defer stop()
		require.Eventually(t, func() bool {
			var count int
			return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1`).Scan(&count) == nil && count == 2
		}, 10*time.Second, 100*time.Millisecond)
	})
}

func addFrustrationFillSessions(t *testing.T, f *frustrationFixture) []string {
	t.Helper()
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count) SELECT 'filler-' || n, 'first', 'sample', 'claude', '2026-01-01', 1 FROM generate_series(1, 499) n`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO messages (session_id, ordinal, role, content) SELECT id, 0, 'user', 'hello' FROM sessions WHERE id LIKE 'filler-%'`)
	require.NoError(t, err)
	ids := []string{"one"}
	for i := 1; i <= 499; i++ {
		ids = append(ids, fmt.Sprintf("filler-%d", i))
	}
	return append(ids, "two")
}

func TestFrustrationFillSkipsTimedOutChunk(t *testing.T) {
	previous := frustrationFillChunkTimeout
	frustrationFillChunkTimeout = 4 * time.Second
	defer func() { frustrationFillChunkTimeout = previous }()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	ids := addFrustrationFillSessions(t, f)
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('one', 'stale', 0, 0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	scans := &signalRowsCounter{storeExecutor: store.pg}
	store.pg = scans
	fillCtx, stopFill := context.WithCancel(f.ctx)
	fillDone := make(chan error, 1)
	workerStopped := make(chan struct{})
	go func() {
		defer close(workerStopped)
		fillDone <- store.fillFrustrationCounts(fillCtx, ids, f.versionColumn)
	}()
	defer func() { stopFill(); <-workerStopped }()
	require.Eventually(t, func() bool { return scans.scans.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	select {
	case err := <-fillDone:
		if f.versionColumn == "xmin" {
			require.ErrorContains(t, err, "lock timeout")
		} else {
			require.ErrorIs(t, err, context.DeadlineExceeded)
		}
	case <-ctx.Done():
		t.Fatal("fill did not finish after the chunk deadline")
	}
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT marker_count FROM session_frustration_counts WHERE session_id='two'`).Scan(&count))
	require.Equal(t, 1, count, "later chunk should finish after a timeout")
}

func TestFrustrationFillDeploymentGates(t *testing.T) {
	testPGURL(t)
	for _, scenario := range []string{"read-only", "read-only-transaction", "writable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFrustrationFixture(t, "frustration_gate_"+strings.ReplaceAll(scenario, "-", "_"))
			if f.versionColumn != "xmin" {
				t.Skip("PostgreSQL roles required")
			}
			grants := `GRANT SELECT ON ALL TABLES IN SCHEMA ` + f.schema + ` TO ROLE`
			if scenario != "read-only" {
				grants += `; GRANT INSERT, UPDATE ON session_frustration_counts TO ROLE`
			}
			if scenario == "read-only-transaction" {
				grants += `; ALTER USER ROLE SET default_transaction_read_only = on`
			}
			url := newUsageJournalRole(t, f.syncer, grants)
			store, err := NewStore(f.ctx, url, f.schema, true)
			require.NoError(t, err)
			defer store.Close()
			writable, err := store.canFillFrustrationCounts(f.ctx)
			require.NoError(t, err)
			require.Equal(t, scenario == "writable", writable)
		})
	}
}

type signalRowsCounter struct {
	storeExecutor
	reads   atomic.Int32
	scans   atomic.Int32
	entered chan struct{}
	gate    chan struct{}
}

// flightWaitContext signals when the second request starts waiting on its flight.
type flightWaitContext struct {
	context.Context
	armed   atomic.Bool
	once    sync.Once
	waiting chan struct{}
}

func (c *flightWaitContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

func (c *signalRowsCounter) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	row := c.storeExecutor.QueryRowContext(ctx, query, args...)
	if wait, ok := ctx.(*flightWaitContext); ok && strings.Contains(query, "dashboard_common") {
		wait.armed.Store(true)
	}
	return row
}

func (c *signalRowsCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "health_score, health_grade, outcome") {
		if c.reads.Add(1) == 1 && c.gate != nil {
			close(c.entered)
			select {
			case <-c.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if strings.Contains(query, "SELECT session_id, NULL") {
		c.scans.Add(1)
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func TestFrustrationFillConcurrentSignalsShareOneLoad(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	require.NotEmpty(t, store.keptAnalyticsVersion(f.ctx, "signals|{}"))
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	tx, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	counter := &signalRowsCounter{storeExecutor: store.pg, entered: make(chan struct{}), gate: make(chan struct{})}
	store.pg = counter
	stop := store.StartFrustrationFill(f.ctx)
	defer func() { tx.Rollback(); stop() }()
	require.Eventually(t, func() bool { return counter.scans.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	type response struct {
		result db.SignalsAnalyticsResponse
		err    error
	}
	done := make(chan response, 2)
	call := func(ctx context.Context) {
		result, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
		done <- response{result, err}
	}
	shortCtx, shortCancel := context.WithTimeout(f.ctx, time.Second)
	defer shortCancel()
	longCtx, longCancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer longCancel()
	go call(shortCtx)
	select {
	case <-counter.entered:
	case <-time.After(10 * time.Second):
		close(counter.gate)
		t.Fatal("signal rows query did not start")
	}
	wait := &flightWaitContext{Context: longCtx, waiting: make(chan struct{})}
	go call(wait)
	select {
	case <-wait.waiting:
	case <-time.After(10 * time.Second):
		close(counter.gate)
		t.Fatal("second request did not join the shared load")
	}
	close(counter.gate)
	for range 2 {
		select {
		case got := <-done:
			require.NoError(t, got.err)
			require.Equal(t, 2, got.result.QualityHealth.Totals.FrustrationMarkerCount)
		case <-time.After(10 * time.Second):
			t.Fatal("request waited for fill")
		}
	}
	// One row read confirms both requests reached the kept.Reads flight during the fill.
	require.EqualValues(t, 1, counter.reads.Load())
	require.EqualValues(t, 2, counter.scans.Load())
}
