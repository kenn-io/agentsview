//go:build pgtest

package postgres

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func TestPGActivityReportSavedPricingVariants(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	for i, rate := range []int64{20_000_000, 40_000_000, 20_000_000, 40_000_000} {
		store := openPGActivityInputStore(t, target, syncer.schema, directory)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: rate}})
		got := pgKeptBuild(t, store, f, q, i < 2)
		want := money.MustParseDollars("0.01")
		if rate == 40_000_000 {
			want = money.MustParseDollars("0.02")
		}
		assert.Equal(t, want, got.Report.Totals.Cost)
	}
}

func TestPGActivityReportSaved(t *testing.T) {
	for _, mode := range []string{"restart", "sweep", "unavailable", "memo-rejected"} {
		t.Run(mode, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("CACHE_DIRECTORY", cache)
			local, syncer, _ := newUsageJournalFixture(t)
			pgKeptSession(t, local, "a", "project-a", "2026-06-14")
			require.NoError(t, local.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("a", 14)))
			pgKeptPush(t, syncer, false)
			target, directory := testPGURL(t), t.TempDir()
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			first := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, first.Close()) })
			var denied *activityIdentityError
			if mode == "unavailable" {
				denied = denyActivityIdentity(t, first)
			}
			if mode == "memo-rejected" {
				first.runtimeState().activityKept.MaxBytes = 1
			}
			want := pgKeptBuild(t, first, f, q, true)
			files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
			require.NoError(t, err)
			if mode == "unavailable" || mode == "memo-rejected" {
				require.Empty(t, files)
			} else {
				require.Len(t, files, 1)
			}
			if mode == "unavailable" {
				lookups := denied.queries
				require.Positive(t, lookups)
				requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
				assert.Equal(t, lookups, denied.queries)
			}
			if mode == "unavailable" || mode == "memo-rejected" {
				return
			}
			if mode == "restart" {
				pgKeptSession(t, local, "b", "project-b", "2026-07-14")
				pgKeptPush(t, syncer, false)
			}
			if mode == "sweep" {
				active := files[0]
				first.saveActivityReport("old1", "v", pgActivityKept{identity: "id", revision: 1})
				files, err = filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
				require.NoError(t, err)
				require.Len(t, files, 2)
				old := time.Now().Add(-31 * 24 * time.Hour)
				for _, file := range files {
					require.NoError(t, os.Chtimes(file, old, old))
				}
				pgKeptBuild(t, first, f, q, false)
				files, err = filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
				require.NoError(t, err)
				require.Equal(t, []string{active}, files)
				return
			}
			restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, restarted.Close()) })
			got := pgKeptBuild(t, restarted, f, q, false)
			if mode == "restart" {
				requirePGKeptEqual(t, want, got)
				pgKeptSession(t, local, "b", "project-a", "2026-06-14")
				pgKeptPush(t, syncer, false)
				updated := pgKeptBuild(t, restarted, f, q, true)
				assert.Len(t, updated.Sessions, 2)
			}
		})
	}
}

func TestPGActivityReportSavedIdentityRetry(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	first := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	pgKeptBuild(t, first, f, q, true)
	pgKeptSession(t, local, "b", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	identityError := &activityIdentityError{storeExecutor: restarted.pg}
	restarted.pg = identityError
	updated := pgKeptBuild(t, restarted, f, q, true)
	require.Equal(t, 1, identityError.queries)
	require.Len(t, updated.Sessions, 2)
	requirePGKeptEqual(t, updated, pgKeptBuild(t, restarted, f, q, false))
	require.GreaterOrEqual(t, identityError.queries, 2)
	again := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, again.Close()) })
	requirePGKeptEqual(t, updated, pgKeptBuild(t, again, f, q, false))
}

func TestPGActivityReportSavedMidnight(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	now := time.Now().UTC()
	timezone := "UTC"
	if now.Hour() < 12 {
		timezone = "Etc/GMT-12"
	}
	end := now.Add(-time.Minute)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", From: end.Add(-time.Hour).Format(time.RFC3339), To: end.Format(time.RFC3339), Timezone: timezone,
	}, now)
	require.NoError(t, err)
	require.False(t, q.Partial)
	pgKeptBuild(t, store, db.AnalyticsFilter{Timezone: timezone}, q, true)
	require.Len(t, store.runtimeState().activityKept.Snapshot(), 1)
	files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files)
}
