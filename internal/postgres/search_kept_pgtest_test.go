//go:build pgtest

package postgres

import (
	"database/sql"
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

func TestSearchKeptStandbyReadsFresh(t *testing.T) {
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
	counter := &analyticsReadCounter{storeExecutor: reader.pg}
	reader.pg = counter
	filter := db.SearchFilter{Query: "needle"}
	for range 2 {
		page, err := reader.Search(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Contains(t, page.Results[0].Snippet, "first")
	}
	require.Equal(t, 2, counter.count())
	write("needle second")
	catchUp()
	page, err := reader.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "second")
}
