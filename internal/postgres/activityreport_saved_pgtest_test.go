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
	for _, mode := range []string{"restart", "identity", "pricing", "unavailable", "identity-retry", "oversize", "midnight"} {
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
			if mode == "identity-retry" {
				first.pg = &activityIdentityError{storeExecutor: first.pg}
			}
			if mode == "oversize" {
				first.runtimeState().activityKept.MaxBytes = 1
			}
			want := pgKeptBuild(t, first, f, q, true)
			files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
			require.NoError(t, err)
			if mode == "unavailable" || mode == "identity-retry" || mode == "oversize" || mode == "midnight" {
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
				requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
				files, err = filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
				require.NoError(t, err)
				require.Len(t, files, 1)
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
			if mode == "identity" {
				f.IncludeSubagents, f.IncludeForks = true, true
				pricing, err := first.loadPricingMap(t.Context())
				require.NoError(t, err)
				digest, err := export.EffectivePricingDigest(pricing)
				require.NoError(t, err)
				version, err := first.activityKeptVersion(t.Context(), digest)
				require.NoError(t, err)
				identity, err := first.activitySourceIdentity(t.Context(), "")
				require.NoError(t, err)
				selection := kept.Selection(f, q)
				data, ok := first.activityReports.Get(selection, identity+"|"+version)
				require.True(t, ok)
				require.True(t, first.activityReports.Put(selection, "other|"+version, data))
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
