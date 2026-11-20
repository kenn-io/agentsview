//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
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
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = $1, session_version = 'stale', marker_count = 7 WHERE session_id = 'one'`, frustrationMarkerRulesVersion+1)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `UPDATE session_frustration_counts SET rules_version = 0 WHERE session_id = 'two'`)
	require.NoError(t, err)
	dash, rest, err := f.open(t).staleFrustrationSessions(f.ctx, f.versionColumn)
	require.NoError(t, err)
	require.Equal(t, []string{"two"}, append(dash, rest...))
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
}

func addFrustrationFillSessions(t *testing.T, f *frustrationFixture) []string {
	t.Helper()
	_, err := f.pg.ExecContext(f.ctx, `UPDATE sessions SET machine=CASE WHEN id='one' THEN 'first' ELSE 'later' END, started_at='2026-01-01'`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count) SELECT 'filler-' || n, 'first', 'sample', 'claude', '2026-01-01', 1 FROM generate_series(1, 499) n`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count) VALUES ('outside', 'outside', 'sample', 'claude', '2026-01-01', 1), ('saved', 'saved', 'sample', 'claude', '2026-01-01', 1)`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO messages (session_id, ordinal, role, content) SELECT id, 0, 'user', 'hello' FROM sessions WHERE id LIKE 'filler-%' OR id IN ('outside', 'saved')`)
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
}

func (c *frustrationScanCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if ctx.Value(frustrationScanKey{}) != nil && strings.Contains(query, "SELECT session_id, NULL") {
		c.scans.Add(1)
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func TestFrustrationFillJoinsCoveredRequests(t *testing.T) {
	previous := frustrationJoinWait
	frustrationJoinWait = 30 * time.Second
	defer func() { frustrationJoinWait = previous }()
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	ids := addFrustrationFillSessions(t, f)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	firstRows, _, err := store.signalRows(f.ctx, db.AnalyticsFilter{Machine: "first"}, f.versionColumn)
	require.NoError(t, err)
	laterRows, _, err := store.signalRows(f.ctx, db.AnalyticsFilter{Machine: "later"}, f.versionColumn)
	require.NoError(t, err)
	outsideRows, _, err := store.signalRows(f.ctx, db.AnalyticsFilter{Machine: "outside"}, f.versionColumn)
	require.NoError(t, err)
	savedRows, _, err := store.signalRows(f.ctx, db.AnalyticsFilter{Machine: "saved"}, f.versionColumn)
	require.NoError(t, err)
	require.Len(t, firstRows, 500)
	require.Len(t, laterRows, 1)
	require.Len(t, outsideRows, 1)
	require.Len(t, savedRows, 1)
	var one db.SignalRow
	for _, row := range firstRows {
		if row.ID == "one" {
			one = row
		}
	}
	require.Equal(t, "one", one.ID)
	two := laterRows[0]
	savedVersion := versions["saved"]
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('saved', $1, $2, 0)`, savedVersion, frustrationMarkerRulesVersion)
	require.NoError(t, err)
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
	txTwo, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer txTwo.Rollback()
	_, err = txTwo.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='two' FOR UPDATE`)
	require.NoError(t, err)
	fillDone := make(chan error, 1)
	go func() { fillDone <- store.fillFrustrationCounts(f.ctx, ids, nil, f.versionColumn) }()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	coveredRows := append(append([]db.SignalRow{}, firstRows...), laterRows...)
	coveredDone := make(chan error, 1)
	go func() {
		coveredDone <- store.populateFrustrationMarkers(coveredCtx, coveredRows, versions, f.versionColumn)
	}()
	select {
	case err := <-coveredDone:
		t.Fatalf("covered request returned before fill: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	mixedRows := append(append([]db.SignalRow{}, savedRows...), firstRows...)
	mixedDone := make(chan error, 1)
	go func() {
		mixedDone <- store.populateFrustrationMarkers(coveredCtx, mixedRows, versions, f.versionColumn)
	}()
	select {
	case err := <-mixedDone:
		t.Fatalf("request returned before its missing chunk: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	narrowRows := []db.SignalRow{one}
	narrowDone := make(chan error, 1)
	go func() { narrowDone <- store.populateFrustrationMarkers(f.ctx, narrowRows, versions, f.versionColumn) }()
	select {
	case err := <-narrowDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("narrow request waited for an unrelated locked row")
	}
	require.Equal(t, 1, narrowRows[0].FrustrationMarkerCount)
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
	outsideDone := make(chan error, 1)
	go func() { outsideDone <- store.populateFrustrationMarkers(f.ctx, outsideRows, versions, f.versionColumn) }()
	select {
	case err := <-outsideDone:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond): // The held fill cannot finish.
		t.Fatal("uncovered request waited for the fill")
	}
	require.NoError(t, tx.Rollback())
	require.NoError(t, <-mixedDone)
	require.Zero(t, mixedRows[0].FrustrationMarkerCount)
	for _, row := range mixedRows {
		if row.ID == "one" {
			require.Equal(t, 1, row.FrustrationMarkerCount)
		}
	}
	require.NoError(t, txTwo.Rollback())
	require.NoError(t, <-fillDone)
	require.NoError(t, <-coveredDone)
	for _, row := range coveredRows {
		if row.ID == "one" {
			require.Equal(t, 1, row.FrustrationMarkerCount)
		}
	}
	require.Equal(t, 1, coveredRows[len(coveredRows)-1].FrustrationMarkerCount)
	require.Zero(t, counter.scans.Load(), "covered requests must reuse filled counts")
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
		fillDone <- store.fillFrustrationCounts(fillCtx, ids, nil, f.versionColumn)
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

func TestFrustrationFillCompletedBetweenLoadAndJoin(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	state := &store.runtimeState().frustrationFill
	state.mu.Lock()
	completed := state.completed
	state.mu.Unlock()
	ids := []string{"one", "two"}
	counts, err := store.loadStoredFrustrationCounts(f.ctx, ids, versions, f.versionColumn)
	require.NoError(t, err)
	require.Empty(t, counts)
	require.NoError(t, store.fillFrustrationCounts(f.ctx, ids, nil, f.versionColumn))
	require.True(t, store.joinFrustrationFill(f.ctx, ids, completed))
}

func TestFrustrationFillExcludesDefaultHiddenSessions(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count, relationship_type) VALUES ('sub', 'test', 'sample', 'claude', '2026-01-01', 1, 'subagent'), ('fork', 'test', 'sample', 'claude', '2026-01-01', 1, 'fork'), ('empty', 'test', 'sample', 'claude', '2026-01-01', 0, '')`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO messages (session_id, ordinal, role, content) VALUES ('sub', 0, 'user', 'this is broken'), ('fork', 0, 'user', 'this is broken')`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('one', 'stale', 0, 0)`)
	require.NoError(t, err)
	dash, rest, err := store.staleFrustrationSessions(f.ctx, f.versionColumn)
	require.NoError(t, err)
	ids := append(dash, rest...)
	require.Equal(t, []string{"one", "two"}, ids)
	rows, versions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	counter := &frustrationScanCounter{storeExecutor: store.pg}
	store.pg = counter
	requestCtx := context.WithValue(f.ctx, frustrationScanKey{}, true)
	gate, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	fillCtx, cancelFill := context.WithCancel(f.ctx)
	fillDone := make(chan error, 1)
	fillStopped := make(chan struct{})
	go func() {
		defer close(fillStopped)
		fillDone <- store.fillFrustrationCounts(fillCtx, ids, nil, f.versionColumn)
	}()
	defer func() { cancelFill(); gate.Rollback(); <-fillStopped }()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- store.populateFrustrationMarkers(requestCtx, rows, versions, f.versionColumn) }()
	select {
	case err := <-done:
		t.Fatalf("default request returned before its fill: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, gate.Rollback())
	require.NoError(t, <-fillDone)
	require.NoError(t, <-done)
	require.Zero(t, counter.scans.Load())
	filtered, filteredVersions, err := store.signalRows(f.ctx, db.AnalyticsFilter{IncludeSubagents: true, IncludeForks: true}, f.versionColumn)
	require.NoError(t, err)
	require.Len(t, filtered, 4)
	require.NoError(t, store.populateFrustrationMarkers(requestCtx, filtered, filteredVersions, f.versionColumn))
	require.EqualValues(t, 1, counter.scans.Load())
	for _, row := range filtered {
		require.Equal(t, 1, row.FrustrationMarkerCount)
	}
}

func TestFrustrationFillSeparatesDashboardSessions(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	_, err := f.pg.ExecContext(f.ctx, `UPDATE sessions SET started_at=now(), is_automated=false WHERE id IN ('one','two')`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO sessions (id, machine, project, agent, started_at, message_count, is_automated) VALUES ('auto', 'test', 'sample', 'claude', now(), 1, true), ('old', 'test', 'sample', 'claude', '2017-01-01', 1, false)`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO messages (session_id, ordinal, role, content) VALUES ('auto', 0, 'user', 'this is broken'), ('old', 0, 'user', 'this is broken')`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('one', 'stale', 0, 0)`)
	require.NoError(t, err)
	dash, rest, err := store.staleFrustrationSessions(f.ctx, f.versionColumn)
	require.NoError(t, err)
	require.Equal(t, []string{"one", "two"}, dash)
	require.Equal(t, []string{"auto", "old"}, rest)
	now := time.Now().UTC()
	filter := db.AnalyticsFilter{AutomatedScope: "human", From: now.AddDate(-1, 0, 0).Format("2006-01-02"), To: now.AddDate(0, 0, 1).Format("2006-01-02")}
	rows, versions, err := store.signalRows(f.ctx, filter, f.versionColumn)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	counter := &frustrationScanCounter{storeExecutor: store.pg}
	store.pg = counter
	requestCtx := context.WithValue(f.ctx, frustrationScanKey{}, true)
	gate, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	fillCtx, cancelFill := context.WithCancel(f.ctx)
	fillDone := make(chan error, 1)
	fillStopped := make(chan struct{})
	go func() {
		defer close(fillStopped)
		fillDone <- store.fillFrustrationCounts(fillCtx, dash, rest, f.versionColumn)
	}()
	defer func() { cancelFill(); gate.Rollback(); <-fillStopped }()
	require.Eventually(t, func() bool {
		state := &store.runtimeState().frustrationFill
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["auto"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- store.populateFrustrationMarkers(requestCtx, rows, versions, f.versionColumn) }()
	select {
	case err := <-done:
		t.Fatalf("dashboard request returned before its fill: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, gate.Rollback())
	require.NoError(t, <-done)
	require.NoError(t, <-fillDone)
	require.Zero(t, counter.scans.Load())
	all, allVersions, err := store.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	require.Len(t, all, 4)
	require.NoError(t, store.populateFrustrationMarkers(requestCtx, all, allVersions, f.versionColumn))
	for _, row := range all {
		require.Equal(t, 1, row.FrustrationMarkerCount)
	}
}

type signalsLoadCounter struct {
	storeExecutor
	reads   atomic.Int32
	started chan struct{}
}

func (c *signalsLoadCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "quality_signal_version") && c.reads.Add(1) == 1 {
		close(c.started)
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func TestFrustrationFillShortDeadlineSharedSignals(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	store := f.open(t)
	require.NotEmpty(t, store.keptAnalyticsVersion(f.ctx, "signals"))
	_, err := f.pg.ExecContext(f.ctx, `INSERT INTO session_frustration_counts(session_id, session_version, rules_version, marker_count) VALUES ('one', 'stale', 0, 0)`)
	require.NoError(t, err)
	gate, err := f.pg.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(f.ctx, `SELECT 1 FROM session_frustration_counts WHERE session_id='one' FOR UPDATE`)
	require.NoError(t, err)
	counter := &signalsLoadCounter{storeExecutor: store.pg, started: make(chan struct{})}
	store.pg = counter
	fillCtx, cancelFill := context.WithCancel(f.ctx)
	fillDone := make(chan error, 1)
	fillStopped := make(chan struct{})
	go func() {
		defer close(fillStopped)
		fillDone <- store.fillFrustrationCounts(fillCtx, []string{"one", "two"}, nil, f.versionColumn)
	}()
	defer func() { cancelFill(); gate.Rollback(); <-fillStopped }()
	state := &store.runtimeState().frustrationFill
	require.Eventually(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pending["one"] != nil && state.pending["two"] != nil
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	type result struct {
		value db.SignalsAnalyticsResponse
		err   error
	}
	first := make(chan result, 1)
	go func() {
		value, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
		first <- result{value, err}
	}()
	select {
	case <-counter.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := make(chan result, 1)
	go func() {
		value, err := store.GetAnalyticsSignals(f.ctx, db.AnalyticsFilter{})
		second <- result{value, err}
	}()
	one, two := <-first, <-second
	require.NoError(t, one.err)
	require.NoError(t, two.err)
	require.Equal(t, 2, one.value.QualityHealth.Totals.FrustrationMarkerCount)
	require.Equal(t, one.value, two.value)
	require.EqualValues(t, 1, counter.reads.Load())
	state.mu.Lock()
	pending := len(state.pending)
	state.mu.Unlock()
	require.Equal(t, 2, pending)
	again, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, one.value, again)
	require.EqualValues(t, 1, counter.reads.Load())

	require.NoError(t, gate.Rollback())
	require.NoError(t, <-fillDone)
	reader := f.open(t)
	readerState := &reader.runtimeState().frustrationFill
	readerChunk := &frustrationChunk{ids: []string{"one", "two"}, done: make(chan struct{})}
	readerState.mu.Lock()
	readerState.pending = map[string]*frustrationChunk{"one": readerChunk, "two": readerChunk}
	readerState.chunks = []*frustrationChunk{readerChunk}
	readerState.mu.Unlock()
	defer readerState.release(readerChunk)
	rows, versions, err := reader.signalRows(f.ctx, db.AnalyticsFilter{}, f.versionColumn)
	require.NoError(t, err)
	storedCtx, stop := context.WithTimeout(f.ctx, 500*time.Millisecond)
	defer stop()
	require.NoError(t, reader.populateFrustrationMarkers(storedCtx, rows, versions, f.versionColumn))
	for _, row := range rows {
		require.Equal(t, 1, row.FrustrationMarkerCount)
	}
}

func TestFrustrationFillIncludesSessionsWithoutDates(t *testing.T) {
	f := newFrustrationFixture(t, "agentsview")
	_, err := f.pg.ExecContext(f.ctx, `UPDATE sessions SET started_at=NULL, created_at=NULL WHERE id='one'`)
	require.NoError(t, err)
	store := f.open(t)
	dash, rest, err := store.staleFrustrationSessions(f.ctx, f.versionColumn)
	require.NoError(t, err)
	require.NotContains(t, dash, "one")
	require.Contains(t, rest, "one")
	stop := store.StartFrustrationFill(f.ctx)
	defer stop()
	require.Eventually(t, func() bool {
		var count int
		return f.pg.QueryRowContext(f.ctx, `SELECT count(*) FROM session_frustration_counts WHERE marker_count=1`).Scan(&count) == nil && count == 2
	}, 20*time.Second, 50*time.Millisecond)
}
