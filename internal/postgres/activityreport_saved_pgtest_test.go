//go:build pgtest

package postgres

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

func TestPGActivityReportSavedRestart(t *testing.T) {
	for _, mode := range []string{"restart", "format", "pricing", "unavailable", "identity-retry", "oversize", "midnight"} {
		t.Run(mode, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("CACHE_DIRECTORY", cache)
			local, syncer, _ := newUsageJournalFixture(t)
			pgKeptSession(t, local, "a", "project-a", "2026-06-14")
			require.NoError(t, local.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("a", 14)))
			pgKeptPush(t, syncer, false)
			target, directory := testPGURL(t), t.TempDir()
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			if mode == "midnight" {
				q = pgDayQuery(t, time.Now().UTC().Format("2006-01-02"), "UTC")
			}
			first := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, first.Close()) })
			var denied *activityIdentityError
			if mode == "unavailable" {
				_, err := first.pg.ExecContext(t.Context(), `CREATE FUNCTION activity_identity_denied() RETURNS text LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'permission denied' USING ERRCODE = '42501'; END $$`)
				require.NoError(t, err)
				denied = &activityIdentityError{storeExecutor: first.pg, denied: true}
				first.pg = denied
			}
			if mode == "oversize" {
				first.runtimeState().activityKept.MaxBytes = 1
			}
			want := pgKeptBuild(t, first, f, q, true)
			files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
			require.NoError(t, err)
			if mode == "unavailable" || mode == "oversize" || mode == "midnight" {
				require.Empty(t, files)
			} else {
				require.Len(t, files, 1)
			}
			if mode == "unavailable" {
				lookups := denied.queries
				requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
				assert.Equal(t, lookups, denied.queries)
			}
			if mode == "identity-retry" {
				pgKeptSession(t, local, "b", "project-a", "2026-06-14")
				pgKeptPush(t, syncer, false)
				restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
				t.Cleanup(func() { require.NoError(t, restarted.Close()) })
				restarted.pg = &activityIdentityError{storeExecutor: restarted.pg}
				updated := pgKeptBuild(t, restarted, f, q, true)
				require.Len(t, updated.Sessions, 2)
				requirePGKeptEqual(t, updated, pgKeptBuild(t, restarted, f, q, false))
				again := openPGActivityInputStore(t, target, syncer.schema, directory)
				t.Cleanup(func() { require.NoError(t, again.Close()) })
				requirePGKeptEqual(t, updated, pgKeptBuild(t, again, f, q, false))
				return
			}
			if mode == "midnight" {
				return
			}
			if mode == "restart" {
				first.saveActivityReport("old1", "v", pgActivityKept{identity: "id", revision: 1})
				files, err = filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
				require.NoError(t, err)
				require.Len(t, files, 2)
				old := time.Now().Add(-31 * 24 * time.Hour)
				for _, file := range files {
					require.NoError(t, os.Chtimes(file, old, old))
				}
				pgKeptBuild(t, first, f, q, false)
				_, ok := first.loadActivityReport("old1", "v", "id")
				require.False(t, ok)
				files, err = filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
				require.NoError(t, err)
				require.Len(t, files, 1)
			}
			if mode == "format" {
				f.IncludeSubagents, f.IncludeForks = true, true
				pricing, err := first.loadPricingMap(t.Context())
				require.NoError(t, err)
				digest, err := export.EffectivePricingDigest(pricing)
				require.NoError(t, err)
				version, _, err := first.activityKeptVersion(t.Context(), digest)
				require.NoError(t, err)
				identity, err := first.activitySourceIdentity(t.Context(), "")
				require.NoError(t, err)
				selection := kept.Selection(f, q)
				diskSelection := pgActivityDiskSelection(selection, version, identity)
				data, ok := first.activityReports.Get(diskSelection, "pg-activity-report-v1")
				require.True(t, ok)
				require.True(t, first.activityReports.Put(diskSelection, "pg-activity-report-v0", data))
			}
			restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, restarted.Close()) })
			if mode == "pricing" {
				restarted.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 40_000_000}})
			}
			got := pgKeptBuild(t, restarted, f, q, mode != "restart")
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

func TestPGActivityReportSavedPricingVariants(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	first := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	want := pgKeptBuild(t, first, f, q, true)
	other := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	other.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 40_000_000}})
	priced := pgKeptBuild(t, other, f, q, true)
	restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, q, false))
	restartedOther := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restartedOther.Close()) })
	restartedOther.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 40_000_000}})
	requirePGKeptEqual(t, priced, pgKeptBuild(t, restartedOther, f, q, false))
}

func TestPGActivityReportSavedSweepOnMemoHit(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptSession(t, local, "b", "project-a", "2026-06-15")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := db.AnalyticsFilter{Timezone: "UTC"}
	pgKeptBuild(t, store, f, pgDayQuery(t, "2026-06-14", "UTC"), true)
	files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	expired := files[0]
	q := pgDayQuery(t, "2026-06-15", "UTC")
	want := pgKeptBuild(t, store, f, q, true)
	old := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(expired, old, old))
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	_, err = os.Stat(expired)
	require.ErrorIs(t, err, os.ErrNotExist)
}
