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
		waitCtx, cancel := context.WithTimeout(f.ctx, 250*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, store.usageCache.WaitWarm(waitCtx), context.DeadlineExceeded)

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

type frustrationScanKey struct{}

type frustrationScanCounter struct {
	storeExecutor
	scans atomic.Int32
	mu    sync.Mutex
	ids   []string
}

func (c *frustrationScanCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if ctx.Value(frustrationScanKey{}) != nil && strings.Contains(query, "SELECT session_id, NULL") {
		c.scans.Add(1)
		c.mu.Lock()
		c.ids = append(c.ids, args[0].([]string)...)
		c.mu.Unlock()
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func TestFrustrationFillJoinsCoveredRequests(t *testing.T) {
	t.Run("covered", func(t *testing.T) {
		previous := frustrationJoinWait
		frustrationJoinWait = 30 * time.Second
		defer func() { frustrationJoinWait = previous }()
		f := newFrustrationFixture(t, "agentsview")
		store := f.open(t)
		ids := addFrustrationFillSessions(t, f)
		_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
		require.NoError(t, err)
		signalRowsFor := func(ids ...string) []db.SignalRow {
			rows := make([]db.SignalRow, len(ids))
			for i, id := range ids {
				rows[i].ID = id
			}
			return rows
		}
		firstRows, laterRows := signalRowsFor(ids[:500]...), signalRowsFor("two")
		two := laterRows[0]
		_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) SELECT id, 'stale', 0, 0 FROM sessions WHERE id IN ('one', 'two')`)
		require.NoError(t, err)
		counter := &frustrationScanCounter{storeExecutor: store.pg}
		store.pg = counter
		coveredCtx := context.WithValue(f.ctx, frustrationScanKey{}, true)
		tx, err := f.pg.BeginTx(f.ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		_, err = tx.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
		require.NoError(t, err)
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		completed := state.completed
		state.mu.Unlock()
		fillDone := make(chan error, 1)
		go func() { fillDone <- store.fillFrustrationCounts(f.ctx, ids, f.versionColumn) }()
		require.Eventually(t, func() bool {
			state := &store.runtimeState().frustrationFill
			state.mu.Lock()
			defer state.mu.Unlock()
			return state.pending["one"] != nil && state.pending["two"] != nil
		}, 10*time.Second, 10*time.Millisecond)
		lastRows := []db.SignalRow{two}
		lastDone := make(chan error, 1)
		go func() {
			lastDone <- store.populateFrustrationMarkers(f.ctx, lastRows, versions, f.versionColumn, false)
		}()
		select {
		case err := <-lastDone:
			require.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("request for a queued chunk waited for unrelated work")
		}
		require.Equal(t, 1, lastRows[0].FrustrationMarkerCount)
		_, err = f.pg.ExecContext(f.ctx, `DELETE FROM session_frustration_counts WHERE session_id='two'`)
		require.NoError(t, err)
		store.runtimeState().frustrationMarkers.Swap(func() {})
		coveredRows := append(append([]db.SignalRow{}, firstRows...), laterRows...)
		coveredDone := make(chan error, 1)
		go func() {
			coveredDone <- store.populateFrustrationMarkers(coveredCtx, coveredRows, versions, f.versionColumn, false)
		}()
		select {
		case err := <-coveredDone:
			t.Fatalf("covered request returned before fill: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		require.NoError(t, tx.Rollback())
		require.NoError(t, <-fillDone)
		joined, err := store.joinFrustrationFill(f.ctx, []string{"one", "two"}, completed, false)
		require.NoError(t, err)
		require.True(t, joined)
		require.NoError(t, <-coveredDone)
		for _, row := range coveredRows {
			if row.ID == "one" {
				require.Equal(t, 1, row.FrustrationMarkerCount)
			}
		}
		require.Equal(t, 1, coveredRows[len(coveredRows)-1].FrustrationMarkerCount)
		require.Zero(t, counter.scans.Load(), "covered requests must reuse filled counts")

	})
	t.Run("partial", func(t *testing.T) {
		f := newFrustrationFixture(t, "agentsview")
		store := f.open(t)
		_, err := f.pg.ExecContext(f.ctx, `UPDATE sessions SET started_at='2026-01-03' WHERE id IN ('one','two')`)
		require.NoError(t, err)
		// Two fixture sessions and these fillers make one full chunk.
		_, err = f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count) SELECT 'filler-' || n, 'test', 'sample', 'claude', '2026-01-02', 1 FROM generate_series(1,$1) n`, maxPGVars-2)
		require.NoError(t, err)
		_, err = f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count) VALUES ('inside', 'test', 'sample', 'claude', '2026-01-01 12:00:00+00', 1), ('outside', 'test', 'other', 'claude', '2026-01-01 11:00:00+00', 1)`)
		require.NoError(t, err)
		_, err = f.pg.ExecContext(f.ctx, `INSERT INTO messages (session_id, ordinal, role, content) SELECT id, 0, 'user', 'this is broken' FROM sessions WHERE id LIKE 'filler-%' OR id IN ('inside','outside')`)
		require.NoError(t, err)
		_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0), ('outside','stale',0,0)`)
		require.NoError(t, err)
		first, err := f.pg.BeginTx(f.ctx, nil)
		require.NoError(t, err)
		defer first.Rollback()
		_, err = first.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
		require.NoError(t, err)
		last, err := f.pg.BeginTx(f.ctx, nil)
		require.NoError(t, err)
		defer last.Rollback()
		_, err = last.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='outside' FOR UPDATE`)
		require.NoError(t, err)
		counter := &frustrationScanCounter{storeExecutor: store.pg}
		store.pg = counter
		stop := store.StartFrustrationFill(f.ctx)
		defer func() { first.Rollback(); last.Rollback(); stop() }()
		state := &store.runtimeState().frustrationFill
		require.Eventually(t, func() bool { state.mu.Lock(); defer state.mu.Unlock(); return state.pending["outside"] != nil }, 10*time.Second, 10*time.Millisecond)
		filter := db.AnalyticsFilter{Project: "sample"}
		rows, versions, err := store.signalRows(f.ctx, filter, f.versionColumn)
		require.NoError(t, err)
		require.Len(t, rows, maxPGVars+1)
		ctx := context.WithValue(f.ctx, frustrationScanKey{}, true)
		done := make(chan error, 1)
		go func() { done <- store.populateFrustrationMarkers(ctx, rows, versions, f.versionColumn, false) }()
		select {
		case err := <-done:
			t.Fatalf("request returned before covered chunk: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		require.NoError(t, first.Rollback())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(4 * time.Second):
			t.Fatal("request waited for the unrelated row")
		}
		require.EqualValues(t, 1, counter.scans.Load())
		counter.mu.Lock()
		scannedIDs := append([]string(nil), counter.ids...)
		counter.mu.Unlock()
		require.Equal(t, []string{"inside"}, scannedIDs)
		for _, row := range rows {
			require.Equal(t, 1, row.FrustrationMarkerCount)
		}
	})
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
	fillCtx, stopFill := context.WithCancel(f.ctx)
	fillDone := make(chan error, 1)
	workerStopped := make(chan struct{})
	go func() {
		defer close(workerStopped)
		fillDone <- store.fillFrustrationCounts(fillCtx, ids, f.versionColumn)
	}()
	defer func() { stopFill(); <-workerStopped }()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	select {
	case err := <-fillDone:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-ctx.Done():
		t.Fatal("fill did not finish after the chunk deadline")
	}
	var count int
	require.NoError(t, f.pg.QueryRowContext(f.ctx, `SELECT marker_count FROM session_frustration_counts WHERE session_id='two'`).Scan(&count))
	require.Equal(t, 1, count, "later chunk should finish after a timeout")
	state := &store.runtimeState().frustrationFill
	state.mu.Lock()
	for _, chunk := range state.chunks {
		assert.Empty(t, chunk.ids, "completed fill retains session IDs")
	}
	state.mu.Unlock()
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

func TestFrustrationFillRequestReservesScanTime(t *testing.T) {
	previous := frustrationJoinWait
	frustrationJoinWait = time.Second
	defer func() { frustrationJoinWait = previous }()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, err := f.pg.ExecContext(f.ctx, `UPDATE sync_metadata SET value='0' WHERE key='usage_change_journal_version'`)
	require.NoError(t, err)
	require.Empty(t, store.keptAnalyticsVersion(f.ctx, "signals|{}"))
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	gate, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	counter := &frustrationScanCounter{storeExecutor: store.pg}
	store.pg = counter
	stop := store.StartFrustrationFill(f.ctx)
	defer func() { gate.Rollback(); stop() }()
	state := &store.runtimeState().frustrationFill
	require.Eventually(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.WithValue(f.ctx, frustrationScanKey{}, true))
	defer cancel()
	type response struct {
		result db.SignalsAnalyticsResponse
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
		done <- response{result, err}
	}()
	var got response
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("request did not fall back while the fill was blocked")
	}
	require.NoError(t, got.err)
	require.Equal(t, 2, got.result.QualityHealth.Totals.FrustrationMarkerCount)
	require.EqualValues(t, 1, counter.scans.Load(), "request must scan while fill remains blocked")
}

type blockedSignalRows struct {
	storeExecutor
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockedSignalRows) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "health_score, health_grade, outcome") {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.storeExecutor.QueryContext(ctx, query, args...)
}

func TestFrustrationSignalsShortDeadlineJoinsEarlierFlight(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	require.NotEmpty(t, store.keptAnalyticsVersion(f.ctx, "signals|{}"))
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('one','stale',0,0)`)
	require.NoError(t, err)
	gate, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	_, err = gate.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	probe := &blockedSignalRows{storeExecutor: store.pg, started: make(chan struct{}), release: make(chan struct{})}
	store.pg = probe
	firstCtx, cancelFirst := context.WithCancel(f.ctx)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = store.GetAnalyticsSignals(firstCtx, db.AnalyticsFilter{})
	}()
	release := sync.OnceFunc(func() { close(probe.release) })
	defer func() { release(); gate.Rollback(); <-firstDone; cancelFirst() }()
	select {
	case <-probe.started:
	case <-time.After(10 * time.Second):
		t.Fatal("first request did not start reading signal rows")
	}
	stop := store.StartFrustrationFill(f.ctx)
	defer func() { gate.Rollback(); stop() }()
	state := &store.runtimeState().frustrationFill
	require.Eventually(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	release()
	result, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, result.QualityHealth.Totals.FrustrationMarkerCount)
}

type signalRowsCounter struct {
	storeExecutor
	reads atomic.Int32
}

func (c *signalRowsCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "health_score, health_grade, outcome") {
		c.reads.Add(1)
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func TestFrustrationSignalsPartialChunkUsesSharedLoad(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, err := f.pg.ExecContext(f.ctx, `UPDATE sessions SET project='other' WHERE id='two'`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id,session_version,rules_version,marker_count) VALUES ('two','stale',0,0)`)
	require.NoError(t, err)
	gate, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	_, err = gate.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='two' FOR UPDATE`)
	require.NoError(t, err)
	counter := &signalRowsCounter{storeExecutor: store.pg}
	store.pg = counter
	stop := store.StartFrustrationFill(f.ctx)
	defer func() { gate.Rollback(); stop() }()
	state := &store.runtimeState().frustrationFill
	require.Eventually(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	require.NotEmpty(t, store.keptAnalyticsVersion(f.ctx, "signals|{}"))
	result, err := store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{Project: "sample"})
	require.NoError(t, err)
	require.Equal(t, 1, result.QualityHealth.Totals.FrustrationMarkerCount)
	require.EqualValues(t, 1, counter.reads.Load())
}
