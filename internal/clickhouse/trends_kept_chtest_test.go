//go:build chtest

package clickhouse

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestTrendsKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	first, syncer, local := newPushedStore(t)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	terms := []db.TrendTermInput{{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}}
	read := func(store *Store, f db.AnalyticsFilter) db.TrendsTermsResponse {
		t.Helper()
		result, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
		require.NoError(t, err)
		return result
	}
	count := func(store *Store) uint64 {
		t.Helper()
		n, _ := usageQueryCountsOf(t, store, "startsWith(trimLeft(query, ' \\n\\t\\r'), 'SELECT m.content, COALESCE(m.timestamp')")
		return n
	}
	var reports []db.TrendsTermsResponse
	for _, zone := range []string{"UTC", "America/New_York"} {
		f := db.AnalyticsFilter{Timezone: zone, From: "2026-01-01", To: "2026-01-31"}
		want := read(first, f)
		reports = append(reports, want)
		require.Positive(t, count(first))
		fresh := newTestStore(t, syncer.conn, syncer.target)
		require.Equal(t, read(fresh, f), want)
		restarted := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, restarted.openActivityReportDisk(syncer.target))
		before := count(restarted)
		require.Equal(t, want, read(restarted, f))
		require.Equal(t, before, count(restarted))
	}
	require.NotEqual(t, reports[0].Buckets, reports[1].Buckets)
	empty := db.AnalyticsFilter{Timezone: "UTC", Project: "no-such-project"}
	wantEmpty := read(first, empty)
	emptyRestart := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, emptyRestart.openActivityReportDisk(syncer.target))
	beforeEmpty := count(emptyRestart)
	require.Equal(t, wantEmpty, read(emptyRestart, empty))
	require.Equal(t, beforeEmpty, count(emptyRestart))
	f := db.AnalyticsFilter{Timezone: "UTC", From: "2026-01-01", To: "2026-01-31"}
	beforePush := read(first, f)
	appendMessage(t, local, fixtureAlphaID, "alpha", "2026-01-12T05:00:00Z")
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	updated := read(restarted, f)
	require.Greater(t, updated.Series[0].Total, beforePush.Series[0].Total)
	require.Equal(t, read(newTestStore(t, syncer.conn, syncer.target), f), updated)
	again := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, again.openActivityReportDisk(syncer.target))
	before := count(again)
	require.Equal(t, updated, read(again, f))
	require.Equal(t, before, count(again))

	files := func() []string {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
		require.NoError(t, err)
		return paths
	}
	prior := files()
	for _, filter := range []db.AnalyticsFilter{
		{Timezone: "UTC", ActiveSince: "2026-01-01T00:00:00Z"},
		{Timezone: "UTC", Termination: "active"},
		{Timezone: "Local"},
	} {
		read(first, filter)
	}
	require.Equal(t, prior, files())
}

func TestTrendsKeptPush(t *testing.T) {
	local, target := seedFixture(t)
	ctx := t.Context()
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	session, err := local.GetSession(ctx, fixtureBetaID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, fixtureBetaID)
	require.NoError(t, err)
	messages[0].Content = "alpha"
	messages[0].ContentLength = 5
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureBetaID, messages))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var sized atomic.Int64
	store.trendsReads.Size = func(db.TrendsTermsResponse) int64 { sized.Add(1); return 1 }
	f := db.AnalyticsFilter{Project: session.Project, Timezone: "UTC", From: "2026-01-01", To: "2026-01-31"}
	terms := []db.TrendTermInput{{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}}
	read := func(filter db.AnalyticsFilter) db.TrendsTermsResponse {
		r, err := store.GetTrendsTerms(ctx, filter, terms, "week")
		require.NoError(t, err)
		return r
	}
	first := read(f)
	require.Equal(t, 1, first.Series[0].Total)
	require.EqualValues(t, 1, sized.Load())
	require.Equal(t, first, read(f))
	require.EqualValues(t, 1, sized.Load())
	session.Project = "changed-project"
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: *session, DataVersion: 1, ReplaceMessages: false}})
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, read(f).MessageCount)
	f.Project = "changed-project"
	require.Equal(t, 1, read(f).Series[0].Total)
	messages[0].Content = "bravo"
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureBetaID, messages))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, read(f).Series[0].Total)
}
