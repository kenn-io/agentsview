//go:build chtest

package clickhouse

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestTrendsKeptPartialPush(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	store, syncer, local := newPushedStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	ctx := t.Context()
	f := db.AnalyticsFilter{Timezone: "UTC", Project: "alpha", From: "2026-01-01", To: "2026-01-31"}
	terms := []db.TrendTermInput{{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}}
	read := func(s *Store) db.TrendsTermsResponse {
		t.Helper()
		report, err := s.GetTrendsTerms(ctx, f, terms, "day")
		require.NoError(t, err)
		return report
	}
	before := read(store)
	require.Equal(t, 1, before.Series[0].Total)
	var sessionVersion uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT push_version FROM sessions WHERE id = ?", fixtureAlphaID).Scan(&sessionVersion))
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	messages[0].Content = "bravo"
	messages[0].ContentLength = len(messages[0].Content)
	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, messages))
	syncer.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error { return errors.New("interrupted publication") }}
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Errors)
	var unpublishedVersion uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT push_version FROM sessions WHERE id = ?", fixtureAlphaID).Scan(&unpublishedVersion))
	require.Equal(t, sessionVersion, unpublishedVersion)
	fresh := read(newTestStore(t, syncer.conn, syncer.target))
	require.Zero(t, fresh.Series[0].Total)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	require.Equal(t, fresh, read(restarted))

	appendMessage(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:30:00.000Z")
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Errors)
	want := read(newTestStore(t, syncer.conn, syncer.target))
	require.Equal(t, 1, want.Series[0].Total)
	require.Equal(t, want, read(store))

	require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, nil))
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Errors)
	want = read(newTestStore(t, syncer.conn, syncer.target))
	require.Zero(t, want.Series[0].Total)
	require.Equal(t, want, read(store))
}

func trendPartsTextQueryCount(t *testing.T, store *Store) uint64 {
	t.Helper()
	n, _ := usageQueryCountsOf(t, store, "startsWith(trimLeft(query, ' \\n\\t\\r'), 'SELECT m.session_id, m.content')")
	return n
}

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
	var reports []db.TrendsTermsResponse
	for _, zone := range []string{"UTC", "America/New_York"} {
		f := db.AnalyticsFilter{Timezone: zone, From: "2026-01-01", To: "2026-01-31"}
		beforePartsRead := trendPartsTextQueryCount(t, first)
		want := read(first, f)
		reports = append(reports, want)
		afterPartsRead := trendPartsTextQueryCount(t, first)
		require.Greater(t, afterPartsRead, beforePartsRead)
		restarted := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, restarted.openActivityReportDisk(syncer.target))
		before := trendPartsTextQueryCount(t, restarted)
		require.Equal(t, want, read(restarted, f))
		require.Equal(t, before, trendPartsTextQueryCount(t, restarted))
	}
	require.NotEqual(t, reports[0].Buckets, reports[1].Buckets)
	partsBefore, err := first.tablePartsFingerprint(t.Context(), []string{"sessions", "messages"})
	require.NoError(t, err)
	execPreparedUsage(t, first,
		"SYSTEM STOP MERGES sessions", "SYSTEM STOP MERGES messages",
		"INSERT INTO sessions SELECT * FROM sessions", "INSERT INTO messages SELECT * FROM messages",
		"SYSTEM START MERGES sessions", "SYSTEM START MERGES messages",
		"OPTIMIZE TABLE sessions FINAL", "OPTIMIZE TABLE messages FINAL")
	partsAfter, err := first.tablePartsFingerprint(t.Context(), []string{"sessions", "messages"})
	require.NoError(t, err)
	require.NotEqual(t, partsBefore, partsAfter)
	for i, zone := range []string{"UTC", "America/New_York"} {
		restarted := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, restarted.openActivityReportDisk(syncer.target))
		before := trendPartsTextQueryCount(t, restarted)
		require.Equal(t, reports[i], read(restarted, db.AnalyticsFilter{Timezone: zone, From: "2026-01-01", To: "2026-01-31"}))
		require.Equal(t, before, trendPartsTextQueryCount(t, restarted))
	}
	f := db.AnalyticsFilter{Timezone: "UTC", From: "2026-01-01", To: "2026-01-31"}
	beforePush := read(first, f)
	appendMessage(t, local, fixtureAlphaID, "alpha", "2026-01-12T05:00:00Z")
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	updated := read(restarted, f)
	require.Greater(t, updated.Series[0].Total, beforePush.Series[0].Total)
	require.Equal(t, read(newTestStore(t, syncer.conn, syncer.target), f), updated)
	again := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, again.openActivityReportDisk(syncer.target))
	before := trendPartsTextQueryCount(t, again)
	require.Equal(t, updated, read(again, f))
	require.Equal(t, before, trendPartsTextQueryCount(t, again))

	cacheFiles := func() []string {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
		require.NoError(t, err)
		return paths
	}
	prior := cacheFiles()
	for _, filter := range []db.AnalyticsFilter{
		{Timezone: "UTC", ActiveSince: "2026-01-01T00:00:00Z"},
		{Timezone: "UTC", Termination: "active"},
	} {
		read(first, filter)
	}
	require.Equal(t, prior, cacheFiles())
}

func TestTrendsKeptTimezoneProcess(t *testing.T) {
	if os.Getenv("TRENDS_TZ_CHILD") == "1" {
		target := Target{URL: os.Getenv("TRENDS_TZ_URL"), Database: os.Getenv("TRENDS_TZ_DATABASE")}
		store, err := NewStore(t.Context(), target)
		require.NoError(t, err)
		defer store.Close()
		require.NoError(t, store.openActivityReportDisk(target))
		f := db.AnalyticsFilter{Timezone: os.Getenv("TRENDS_TZ_FILTER"), Project: "alpha", From: "2026-01-01", To: "2026-01-31"}
		terms := []db.TrendTermInput{{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}}
		report, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
		require.NoError(t, err)
		require.Len(t, report.Series, 1)
		for _, point := range report.Series[0].Points {
			if point.Date == os.Getenv("TRENDS_TZ_DATE") {
				require.Positive(t, point.Count)
				return
			}
		}
		t.Fatal("expected date missing from report")
	}
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	store, syncer, _ := newPushedStore(t)
	zoneDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(zoneDir, "Test"), 0o700))
	goRoot, err := exec.CommandContext(t.Context(), "go", "env", "GOROOT").Output()
	require.NoError(t, err)
	zones, err := zip.OpenReader(filepath.Join(strings.TrimSpace(string(goRoot)), "lib", "time", "zoneinfo.zip"))
	require.NoError(t, err)
	defer zones.Close()
	readZone := func(name string) []byte {
		t.Helper()
		for _, file := range zones.File {
			if file.Name == name {
				r, err := file.Open()
				require.NoError(t, err)
				data, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				return data
			}
		}
		t.Fatal("zone missing from Go zoneinfo")
		return nil
	}
	run := func(filter, zone, date string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(zoneDir, "Test", "Shift"), readZone(zone), 0o600))
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTrendsKeptTimezoneProcess$")
		cmd.Env = append(os.Environ(), "TRENDS_TZ_CHILD=1", "TRENDS_TZ_URL="+syncer.target.URL, "TRENDS_TZ_DATABASE="+syncer.target.Database, "TRENDS_TZ_DATE="+date, "TRENDS_TZ_FILTER="+filter, "ZONEINFO="+zoneDir, "TZ=:"+filepath.Join(zoneDir, "Test", "Shift"))
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	run("Test/Shift", "Etc/GMT+12", "2026-01-09")
	before := trendPartsTextQueryCount(t, store)
	run("Test/Shift", "Etc/GMT-12", "2026-01-10")
	after := trendPartsTextQueryCount(t, store)
	require.Greater(t, after, before)
	run("Test/Shift", "Etc/GMT-12", "2026-01-10")
	require.Equal(t, after, trendPartsTextQueryCount(t, store))
}

func TestTrendsKeptMemoryHitRepairsDiskVersion(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	store, syncer, _ := newPushedStore(t)
	require.NoError(t, store.openActivityReportDisk(syncer.target))
	f := db.AnalyticsFilter{Timezone: "UTC", From: "2026-01-01", To: "2026-01-31"}
	terms := []db.TrendTermInput{{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}}
	read := func(s *Store) db.TrendsTermsResponse {
		t.Helper()
		report, err := s.GetTrendsTerms(t.Context(), f, terms, "day")
		require.NoError(t, err)
		return report
	}
	want := read(store)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	var reportFile string
	for _, file := range files {
		saved, err := os.ReadFile(file)
		require.NoError(t, err)
		_, payload, ok := strings.Cut(string(saved), "\n")
		if !ok {
			continue
		}
		var persisted db.TrendsTermsResponse
		if err := json.Unmarshal([]byte(payload), &persisted); err == nil && reflect.DeepEqual(want, persisted) {
			require.Empty(t, reportFile)
			reportFile = file
		}
	}
	require.NotEmpty(t, reportFile)
	old := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(reportFile, old, old))
	read(store)
	store.usageDisk.Sweep(time.Now())
	saved, err := os.ReadFile(reportFile)
	require.NoError(t, err)
	_, payload, ok := strings.Cut(string(saved), "\n")
	require.True(t, ok)
	var persisted db.TrendsTermsResponse
	require.NoError(t, json.Unmarshal([]byte(payload), &persisted))
	require.Equal(t, want, persisted)
	_, slot, err := trendsTermsKeys(f, terms, "day")
	require.NoError(t, err)
	stale, err := json.Marshal(want)
	require.NoError(t, err)
	store.usageDisk.Put(slot, "older-version", stale)
	_, ok = store.usageDisk.Get(slot, "older-version")
	require.True(t, ok)
	require.Equal(t, want, read(store))

	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	before := trendPartsTextQueryCount(t, restarted)
	require.Equal(t, want, read(restarted))
	require.Equal(t, before, trendPartsTextQueryCount(t, restarted))
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
