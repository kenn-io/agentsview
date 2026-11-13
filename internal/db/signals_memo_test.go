package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/testutil"
)

func seedFrustrationSession(t *testing.T, d *DB, id, project string) {
	t.Helper()
	insertSession(t, d, id, project)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: id, Role: "user", Content: "this is broken"}}))
	require.NoError(t, d.UpdateSessionSignals(t.Context(), id, SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}))
}

func TestSignalsFrustrationMemoReuse(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
	require.NoError(t, d.UpdateSessionSignals(ctx, "session", SessionSignalUpdate{ToolRetryCount: 1, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}))

	examples, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "tool_retries", 10)
	require.NoError(t, err)
	require.Len(t, examples.Sessions, 1)
	assert.Equal(t, "session", examples.Sessions[0].SessionID)
	assert.Empty(t, d.frustrationMemo.counts, "unrelated drill-down must skip the prompt scan")

	resp, err := d.analyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	require.Len(t, d.frustrationMemo.counts, 1)
	// A distinctive cached count proves the next request uses the memo.
	entry := d.frustrationMemo.counts["session"]
	d.frustrationMemo.Store(d.frustrationMemo.generation, "session", entry.version, 7)
	resp, err = d.analyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 7, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestSignalsFrustrationMemoInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *DB)
		want   int
	}{
		{"append", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "user", Content: "this is broken"}}))
		}, 2},
		{"replace", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "Please explain this function."}}))
		}, 0},
		{"recreate", func(t *testing.T, d *DB) {
			t.Helper()
			deleted, err := d.DeleteParserExcludedSessions(t.Context(), []string{"session"})
			require.NoError(t, err)
			require.Equal(t, 1, deleted)
			insertSession(t, d, "session", "test")
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "Please explain this function."}}))
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "test")
			require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
			examples, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "frustration_marker_count", 10)
			require.NoError(t, err)
			require.Len(t, examples.Sessions, 1)
			assert.Equal(t, 1, examples.Sessions[0].SignalTotal)
			tc.change(t, d)
			rows, err := d.signalRows(ctx, AnalyticsFilter{})
			require.NoError(t, err)
			require.NoError(t, d.populateFrustrationMarkers(ctx, rows))
			require.Len(t, rows, 1)
			assert.Equal(t, tc.want, rows[0].FrustrationMarkerCount)
		})
	}
}

// The Signals count is the number of prompts signals.IsFrustrationMarker
// flags: user rows that are neither system rows nor tool results. Each
// expected count is worked out by hand from the rules, so a change to the
// rules, the read, or the memo that alters a count fails here, before and
// after the transcript changes.
func TestSignalsFrustrationCountsFollowTranscriptChanges(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "test")
	count := func() int {
		t.Helper()
		// Message writes clear the stored signals until sync recomputes them.
		require.NoError(t, d.UpdateSessionSignals(ctx, "session", SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}))
		resp, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
		require.NoError(t, err)
		return resp.QualityHealth.Totals.FrustrationMarkerCount
	}
	require.NoError(t, d.InsertMessages(ctx, []Message{
		// Kelvin sign lowercases to k: "you broke".
		{SessionID: "session", Ordinal: 0, Role: "user", Content: "you bro\u212ae the build"},
		{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "this is broken"},
		// Two of five words are capitals, exactly the 0.4 threshold.
		{SessionID: "session", Ordinal: 2, Role: "user", Content: "AB CD ef gh ij"},
		{SessionID: "session", Ordinal: 3, Role: "user", SourceSubtype: "tool_result", Content: "WTF WTF WTF WTF"},
		{SessionID: "session", Ordinal: 4, Role: "user", IsSystem: true, Content: "this is broken"},
		{SessionID: "session", Ordinal: 5, Role: "user", Content: "```text\nFUCK FUCK FUCK\n```\nplease handle the log"},
		// The phrase needs an ASCII apostrophe.
		{SessionID: "session", Ordinal: 6, Role: "user", Content: "why won\u2019t it build"},
		// Whitespace runs collapse to one space: "come on".
		{SessionID: "session", Ordinal: 7, Role: "user", Content: "come\n\t on, run it"},
	}))
	assert.Equal(t, 3, count())

	require.NoError(t, d.InsertMessages(ctx, []Message{
		// Two of six words are capitals, under the threshold.
		{SessionID: "session", Ordinal: 8, Role: "user", Content: "ABC def GHI jkl mno pqr"},
		// Long s matches s under case folding: "does not work".
		{SessionID: "session", Ordinal: 9, Role: "user", Content: "doe\u017f not work"},
	}))
	assert.Equal(t, 4, count(), "an append must recount the session")

	require.NoError(t, d.ReplaceSessionMessages(ctx, "session", []Message{
		{SessionID: "session", Ordinal: 0, Role: "user", Content: "wtf"},
		{SessionID: "session", Ordinal: 1, Role: "user", Content: "STILL BROKEN!!"},
	}))
	assert.Equal(t, 1, count(), "a replacement must recount the session")
}

func TestSignalsFrustrationMemoReaderSwap(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
	_, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	entry := d.frustrationMemo.counts["session"]
	generation := d.frustrationMemo.generation
	require.NoError(t, d.Reopen())
	d.frustrationMemo.Store(generation, "session", entry.version, 7)
	assert.Empty(t, d.frustrationMemo.counts, "swap must clear counts and reject an in-flight store")
}

type cancelAfterScanContext struct {
	context.Context
	reader *sql.DB
	cancel context.CancelFunc
}

func (ctx cancelAfterScanContext) Err() error {
	// The driver checks Err while holding a connection. Cancel only once
	// the completed scan has closed its rows and returned that connection.
	if ctx.reader.Stats().InUse == 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestSignalsFrustrationMemoCanceled(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Role: "user", Content: "this is broken"}}))
	rows, err := d.signalRows(t.Context(), AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = d.populateFrustrationMarkers(cancelAfterScanContext{Context: ctx, reader: d.rawReader(), cancel: cancel}, rows)
	require.Equal(t, context.Canceled, err, "cancellation must come from the post-scan check, rather than a wrapped query error")
	assert.Equal(t, 1, rows[0].FrustrationMarkerCount, "the scan must finish before cancellation")
	assert.Empty(t, d.frustrationMemo.counts)
}

func TestSignalsFrustrationStoredRefresh(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"first", "second"} {
		seedFrustrationSession(t, d, id, "test")
	}
	_, err := d.analyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `CREATE TRIGGER keep_second BEFORE UPDATE ON session_frustration_markers WHEN OLD.session_id = 'second' BEGIN SELECT RAISE(ABORT, 'unchanged session updated'); END`)
	require.NoError(t, err)
	require.NoError(t, d.Reopen())
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "first", Ordinal: 1, Role: "user", Content: "this is broken"}}))
	require.NoError(t, d.UpdateSessionSignals(ctx, "first", SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}))
	resp, err := d.analyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 3, resp.QualityHealth.Totals.FrustrationMarkerCount)
	var firstCount int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count FROM session_frustration_markers WHERE session_id = 'first'`).Scan(&firstCount))
	assert.Equal(t, 2, firstCount)
	_, err = d.getWriter().ExecContext(ctx, `DROP TRIGGER keep_second`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE session_frustration_markers SET marker_version = ?, count = 99 WHERE session_id = 'first'`, signals.FrustrationMarkerVersion+1)
	require.NoError(t, err)
	// Version zero simulates a prior matcher version once the version advances.
	_, err = d.getWriter().ExecContext(ctx, `UPDATE session_frustration_markers SET count = 99, marker_version = 0 WHERE session_id = 'second'`)
	require.NoError(t, err)
	require.NoError(t, d.Reopen())
	resp, err = d.analyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 3, resp.QualityHealth.Totals.FrustrationMarkerCount)
	var futureVersion, futureCount int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT marker_version, count FROM session_frustration_markers WHERE session_id = 'first'`).Scan(&futureVersion, &futureCount))
	assert.Equal(t, signals.FrustrationMarkerVersion+1, futureVersion)
	assert.Equal(t, 99, futureCount)
	var count, version int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT marker_version, count FROM session_frustration_markers WHERE session_id = 'second'`).Scan(&version, &count))
	assert.Equal(t, signals.FrustrationMarkerVersion, version)
	assert.Equal(t, 1, count)
}

func TestSignalsFrustrationStoreRejectsChangedTranscript(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "test")
	rows, err := d.signalRows(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Role: "user", Content: "this is broken"}}))
	require.NoError(t, d.storeFrustrationMarkers(ctx, d.frustrationMemo.Generation(), []string{"session"}, map[string]int{"session": 0}, rows))
	var count int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count(*) FROM session_frustration_markers`).Scan(&count))
	assert.Zero(t, count)
}

func TestSignalsFrustrationReadOnlyStoredReuseAndLegacyFallback(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(fmt.Sprintf("stored=%t", stored), func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			seedFrustrationSession(t, d, "session", "test")
			if stored {
				_, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
				require.NoError(t, err)
			} else {
				_, err := d.getWriter().ExecContext(ctx, `DROP TABLE session_frustration_markers`)
				require.NoError(t, err)
			}
			ro, err := OpenReadOnly(ctx, d.Path())
			require.NoError(t, err)
			defer ro.Close()
			if stored {
				ro.rawReader().SetMaxOpenConns(1)
				conn, err := ro.rawReader().Conn(ctx)
				require.NoError(t, err)
				var sqlite *sqlite3.SQLiteConn
				require.NoError(t, conn.Raw(func(raw any) error {
					sqlite = raw.(*sqlite3.SQLiteConn)
					sqlite.RegisterAuthorizer(func(op int, table, column, _ string) int {
						if op == sqlite3.SQLITE_READ && table == "messages" && column == "content" {
							return sqlite3.SQLITE_DENY
						}
						return sqlite3.SQLITE_OK
					})
					return nil
				}))
				require.NoError(t, conn.Close())
				defer sqlite.RegisterAuthorizer(nil)
			}
			resp, err := ro.GetAnalyticsSignals(ctx, AnalyticsFilter{})
			require.NoError(t, err)
			assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
		})
	}
}

func TestSignalsFrustrationCleanup(t *testing.T) {
	for _, action := range []string{"usage text", "usage signals"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			seedFrustrationSession(t, d, "session", "test")
			rows, err := d.signalRows(ctx, AnalyticsFilter{})
			require.NoError(t, err)
			require.NoError(t, d.populateFrustrationMarkers(ctx, rows))
			var count int
			require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count FROM session_frustration_markers WHERE session_id = 'session'`).Scan(&count))
			require.Equal(t, 1, count)
			d.SetArchiveContent(config.ArchiveContentUsage)
			switch action {
			case "usage text":
				name := "renamed"
				require.NoError(t, d.RenameSession(ctx, "session", &name))
			case "usage signals":
				require.NoError(t, d.UpdateSessionSignals(ctx, "session", SessionSignalUpdate{}))
			}
			require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count(*) FROM session_frustration_markers WHERE session_id = 'session'`).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestSignalsFrustrationUsagePolicyKeepsRetainedPrompts(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedFrustrationSession(t, d, "unsaved", "test")
	d.SetArchiveContent(config.ArchiveContentUsage)
	resp, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count(*) FROM session_frustration_markers`).Scan(&count))
	assert.Zero(t, count)
}

func TestSignalsFrustrationFailedStoreRetriesOnEvidence(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedFrustrationSession(t, d, "session", "test")
	_, err := d.getWriter().ExecContext(ctx, `CREATE TRIGGER reject_frustration_store BEFORE INSERT ON session_frustration_markers BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)
	resp, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	_, err = d.getWriter().ExecContext(ctx, `DROP TRIGGER reject_frustration_store`)
	require.NoError(t, err)
	evidence, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 1)
	assert.Equal(t, 1, evidence.Sessions[0].SignalTotal)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count FROM session_frustration_markers WHERE session_id = 'session'`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestSignalsFrustrationCanceledWhileSaving(t *testing.T) {
	for _, blocker := range []string{"connection", "mutex"} {
		t.Run(blocker, func(t *testing.T) {
			d := testDB(t)
			seedFrustrationSession(t, d, "session", "test")
			var blocked func() bool
			var release func()
			if blocker == "connection" {
				writer := d.rawWriter()
				conn, err := writer.Conn(t.Context())
				require.NoError(t, err)
				waits := writer.Stats().WaitCount
				blocked = func() bool { return writer.Stats().WaitCount > waits }
				release = func() { _ = conn.Close() }
			} else {
				d.mu.Lock()
				blocked = func() bool { return testutil.GoroutineBlockedOnMutexIn("lockctx.Lock") }
				release = d.mu.Unlock
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				defer close(done)
				_, err := d.analyticsSignals(ctx, AnalyticsFilter{})
				done <- err
			}()
			defer func() { cancel(); release(); <-done }()
			require.Eventually(t, blocked, 10*time.Second, 5*time.Millisecond)
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "analytics waited after cancellation")
			}
		})
	}
}
