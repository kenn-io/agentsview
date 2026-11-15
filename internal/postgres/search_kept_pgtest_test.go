//go:build pgtest

package postgres

import (
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSearchKeptHostedReadsFresh(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "published", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("published needle")))
	h, tracer := tracedHostedReader(t, f, "needle")
	reads := 0
	tracer.after = func() { reads++ }
	filter := db.SearchFilter{Query: "needle", Limit: 20}
	for range 2 {
		page, err := h.Search(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Equal(t, "codex:portable", page.Results[0].SessionID)
	}
	require.Equal(t, 2, reads)
}

func TestSearchKeptStandbyRetainsAndRefreshes(t *testing.T) {
	standbyURL := testPGStandbyURL(t)
	local, syncer, primary := newUsageJournalFixture(t)
	ctx := t.Context()
	write := func(content string) {
		t.Helper()
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{{SessionID: "session", Role: "user", Content: content}}))
		pgKeptPush(t, syncer, false)
	}
	standby, err := sql.Open("pgx", standbyURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, standby.Close()) })
	catchUp := func() {
		t.Helper()
		var lsn string
		require.NoError(t, primary.pg.QueryRowContext(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn))
		require.Eventually(t, func() bool {
			var caughtUp bool
			return standby.QueryRowContext(ctx, `SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn`, lsn).Scan(&caughtUp) == nil && caughtUp
		}, 20*time.Second, 50*time.Millisecond)
	}
	write("needle first")
	catchUp()
	reader, err := NewStore(ctx, standbyURL, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	var loads atomic.Int32
	reader.runtimeState().search.Size = func(db.SearchPage) int64 { loads.Add(1); return 1 }
	filter := db.SearchFilter{Query: "needle"}
	for range 2 {
		page, err := reader.Search(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Contains(t, page.Results[0].Snippet, "first")
	}
	require.EqualValues(t, 1, loads.Load())
	write("needle second")
	catchUp()
	page, err := reader.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "second")
}

func TestSearchKeptMessagePushInvalidates(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	write := func(content string) {
		t.Helper()
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{{SessionID: "session", Role: "user", Content: content}}))
		pgKeptPush(t, syncer, false)
	}
	write("needle first")
	var loads atomic.Int32
	store.runtimeState().search.Size = func(db.SearchPage) int64 { loads.Add(1); return 1 }
	filter := db.SearchFilter{Query: "needle"}
	for range 2 {
		page, err := store.Search(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Contains(t, page.Results[0].Snippet, "first")
	}
	require.EqualValues(t, 1, loads.Load())
	var before time.Time
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT updated_at FROM sessions WHERE id='session'`).Scan(&before))
	write("needle second")
	var after time.Time
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT updated_at FROM sessions WHERE id='session'`).Scan(&after))
	require.True(t, after.After(before))
	page, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "second")
}

func TestSearchKeptRestoredHistory(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	write := func(content string) {
		t.Helper()
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{{SessionID: "session", Role: "user", Content: content}}))
		pgKeptPush(t, syncer, false)
	}
	write("needle first")
	filter := db.SearchFilter{Query: "needle"}
	page, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "first")
	var revision int64
	var oldToken string
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT (SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'), (SELECT value FROM sync_metadata WHERE key='usage_history_token')`).Scan(&revision, &oldToken))
	write("needle second")
	_, err = store.pg.ExecContext(ctx, `UPDATE sync_metadata SET value=$1::bigint::text WHERE key='usage_change_revision'`, revision)
	require.NoError(t, err)
	var newToken string
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT value FROM sync_metadata WHERE key='usage_history_token'`).Scan(&newToken))
	require.NotEqual(t, oldToken, newToken)
	page, err = store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "second")
}

func TestSearchKeptMissingHistoryBypasses(t *testing.T) {
	for _, mode := range []string{"version 1", "missing token", "malformed token", "borrowed"} {
		t.Run(mode, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			ctx := t.Context()
			pgKeptSession(t, local, "session", "project", "2026-06-14")
			pgKeptPush(t, syncer, false)
			switch mode {
			case "version 1":
				_, err := store.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='1' WHERE key='usage_change_journal_version'`)
				require.NoError(t, err)
			case "missing token":
				_, err := store.pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key='usage_history_token'`)
				require.NoError(t, err)
			case "malformed token":
				_, err := store.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='invalid' WHERE key='usage_history_token'`)
				require.NoError(t, err)
			case "borrowed":
				tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				defer tx.Rollback()
				store = store.readView(tx)
			}
			counter := &analyticsReadCounter{storeExecutor: store.pg}
			store.pg = counter
			filter := db.SearchFilter{Query: "session"}
			for range 2 {
				_, err := store.Search(ctx, filter)
				require.NoError(t, err)
			}
			require.Equal(t, 2, counter.count())
		})
	}
}
