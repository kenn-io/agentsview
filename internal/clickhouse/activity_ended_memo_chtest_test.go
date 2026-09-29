//go:build chtest

package clickhouse

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// The report of an ended day is kept across a push that touches no session
// with activity or usage that day, and rebuilt after one that does; either
// way it matches a report built by a store that kept nothing.
func TestEndedActivityReportSurvivesUnrelatedPushes(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	push := func() {
		t.Helper()
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
		require.NoError(t, err)
	}
	_, err := store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.False(t, q.Partial)
	filter := db.AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true}
	requireFresh := func(got activity.Report) {
		t.Helper()
		fresh, err := NewStoreFromDB(store.DB()).GetActivityReport(ctx, filter, q)
		require.NoError(t, err)
		require.Equal(t, fresh, got)
	}
	first, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	requireFresh(first)
	usageReads := store.activityUsageQueries.Load()

	// A new session two days later changes the mirror but not the day.
	const laterID = "ended-memo-later"
	later := fixtureSession(laterID, "alpha", "later first", "2026-01-12T09:00:00.000Z", 1)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: later, DataVersion: 1, ReplaceMessages: true,
		Messages: []db.Message{usagePriceMessage(laterID, 0, "2026-01-12T09:00:00.000Z", "claude-test",
			`{"input_tokens":40,"output_tokens":4}`)},
	}})
	require.NoError(t, err)
	push()
	kept, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	require.Equal(t, usageReads, store.activityUsageQueries.Load(), "the kept report answers")
	require.Equal(t, first, kept)
	requireFresh(kept)

	// A turn on the day itself rebuilds the report.
	appendMessage(t, local, fixtureAlphaID, "one more turn", "2026-01-10T12:00:00.000Z")
	push()
	rebuilt, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	require.Greater(t, store.activityUsageQueries.Load(), usageReads)
	require.NotEqual(t, first, rebuilt)
	requireFresh(rebuilt)
}

// A store that starts after another kept an ended day's report on disk
// answers from the file without reading the day's rows, and rebuilds it
// after a push that touches the day; either way the report matches one
// built by a store that kept nothing.
func TestEndedActivityReportIsKeptOnDiskAcrossRestarts(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	withDisk := func() *Store {
		t.Helper()
		s := NewStoreFromDB(store.DB())
		require.NoError(t, s.openActivityReportDisk(Target{URL: "clickhouse://mirror:9000/", Database: "agentsview"}))
		return s
	}
	_, err := store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	filter := db.AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true}
	// Every consumer reads a report as its JSON, which the digest covers
	// with the session rows and bucket membership.
	digest := func(artifacts activity.CandidateArtifacts) string {
		t.Helper()
		d, err := activity.ArtifactDigest(artifacts)
		require.NoError(t, err)
		return d
	}
	requireFresh := func(got activity.CandidateArtifacts) {
		t.Helper()
		fresh, err := NewStoreFromDB(store.DB()).BuildActivityReportArtifacts(ctx, filter, q, nil)
		require.NoError(t, err)
		require.Equal(t, digest(fresh), digest(got))
	}
	built, err := withDisk().BuildActivityReportArtifacts(ctx, filter, q, nil)
	require.NoError(t, err)

	restarted := withDisk()
	var done []activity.Progress
	loaded, err := restarted.BuildActivityReportArtifacts(ctx, filter, q, func(p activity.Progress) { done = append(done, p) })
	require.NoError(t, err)
	require.Zero(t, restarted.activityUsageQueries.Load()+restarted.activityInputQueries.Load(), "the kept file answers")
	require.Equal(t, activity.ProgressDone, done[len(done)-1].Phase)
	require.Equal(t, digest(built), digest(loaded))
	requireFresh(loaded)

	appendMessage(t, local, fixtureAlphaID, "one more turn", "2026-01-10T12:00:00.000Z")
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	restarted = withDisk()
	rebuilt, err := restarted.BuildActivityReportArtifacts(ctx, filter, q, nil)
	require.NoError(t, err)
	require.NotZero(t, restarted.activityUsageQueries.Load())
	require.NotEqual(t, digest(built), digest(rebuilt))
	requireFresh(rebuilt)
}

// The report cache only saves work, so serve opens without it when the
// cache directory cannot be created.
func TestOpenServeStoreWithoutUsableReportCache(t *testing.T) {
	ctx := t.Context()
	_, syncer, _ := newPushedStore(t)
	serveTarget := storage.ReplicaTarget{URL: syncer.target.URL, Schema: syncer.target.Database}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	t.Setenv("CACHE_DIRECTORY", blocked)
	store, err := (Backend{}).OpenServeStore(ctx, serveTarget)
	require.NoError(t, err)
	require.NoError(t, store.Close())
}

// A day report is written to disk only when a client opens the day, and
// each open, from memory or from the file, refreshes its modification
// time. The sweep removes a report no one opened for 30 days and keeps
// one that was opened since.
func TestKeptActivityReportsExpireUnlessOpened(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, err := store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	s := NewStoreFromDB(store.DB())
	require.NoError(t, s.openActivityReportDisk(Target{URL: "clickhouse://mirror:9000/", Database: "agentsview"}))
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	filter := db.AnalyticsFilter{Timezone: "UTC"}
	day := func(date string) (activity.Query, string) {
		t.Helper()
		q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: date, Timezone: "UTC"}, now)
		require.NoError(t, err)
		f := filter
		f.IncludeSubagents, f.IncludeForks = true, true
		return q, s.reportDisk.path(activityReportSelection(f, q))
	}
	opened, openedFile := day("2026-01-10")
	unopened, unopenedFile := day("2026-01-11")
	_, neverFile := day("2026-01-12")
	for _, q := range []activity.Query{opened, unopened} {
		_, err := s.BuildActivityReportArtifacts(ctx, filter, q, nil)
		require.NoError(t, err)
	}
	require.FileExists(t, openedFile)
	require.FileExists(t, unopenedFile)
	require.NoFileExists(t, neverFile, "a day no one opened is not written")

	// Both files were last opened 31 days ago; then the client opens one
	// of the days again.
	longAgo := time.Now().Add(-31 * 24 * time.Hour)
	for _, path := range []string{openedFile, unopenedFile} {
		require.NoError(t, os.Chtimes(path, longAgo, longAgo))
	}
	_, err = s.BuildActivityReportArtifacts(ctx, filter, opened, nil)
	require.NoError(t, err)

	removed, err := s.reportDisk.sweep(time.Now())
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	require.FileExists(t, openedFile)
	require.NoFileExists(t, unopenedFile)
}
