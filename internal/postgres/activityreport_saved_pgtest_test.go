//go:build pgtest

package postgres

import (
	"fmt"
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

func TestPGActivityReportSaved(t *testing.T) {
	for _, mode := range []string{"restart", "sweep", "unavailable", "memo-rejected", "legacy", "replaced", "repeated", "identity-retry", "pricing", "today"} {
		t.Run(mode, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("CACHE_DIRECTORY", cache)
			local, syncer, _ := newUsageJournalFixture(t)
			pgKeptSession(t, local, "a", "project-a", "2026-06-14")
			if mode == "repeated" {
				pgKeptSession(t, local, "b", "project-b", "2026-07-14")
			}
			require.NoError(t, local.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("a", 14)))
			pgKeptPush(t, syncer, false)
			ctx := t.Context()
			if mode == "legacy" {
				_, err := syncer.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='1' WHERE key='usage_change_journal_version'; DELETE FROM sync_metadata WHERE key='usage_history_token' OR key LIKE 'usage_history_checkpoint:%'`)
				require.NoError(t, err)
			}
			target, directory := testPGURL(t), t.TempDir()
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			first := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, first.Close()) })
			if mode == "pricing" {
				for i, rate := range []int64{20_000_000, 40_000_000, 20_000_000, 40_000_000} {
					store := first
					if i > 0 {
						store = openPGActivityInputStore(t, target, syncer.schema, directory)
						t.Cleanup(func() { require.NoError(t, store.Close()) })
					}
					store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: rate}})
					got := pgKeptBuild(t, store, f, q, i < 2)
					want := money.MustParseDollars("0.01")
					if rate == 40_000_000 {
						want = money.MustParseDollars("0.02")
					}
					assert.Equal(t, want, got.Report.Totals.Cost)
				}
				return
			}
			if mode == "today" {
				now := time.Now().UTC()
				timezone := "UTC"
				if now.Hour() < 12 {
					timezone = "Etc/GMT-12"
				}
				end := now.Add(-time.Minute)
				var err error
				q, err = activity.ResolveQuery(activity.QueryInput{
					Preset: "custom", From: end.Add(-time.Hour).Format(time.RFC3339), To: end.Format(time.RFC3339), Timezone: timezone,
				}, now)
				require.NoError(t, err)
				require.False(t, q.Partial)
				f.Timezone = timezone
			}
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
			if mode == "unavailable" || mode == "memo-rejected" || mode == "legacy" || mode == "today" {
				require.Empty(t, files)
				if mode == "today" {
					require.Len(t, first.runtimeState().activityKept.Snapshot(), 1)
				} else if mode == "unavailable" {
					lookups := 0
					if denied != nil {
						lookups = denied.queries
						require.Positive(t, lookups)
					}
					requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
					if denied != nil {
						assert.Equal(t, lookups, denied.queries)
					}
				}
				return
			}
			require.Len(t, files, 1)
			var saved []byte
			switch mode {
			case "restart":
				pgKeptSession(t, local, "b", "project-b", "2026-07-14")
				pgKeptPush(t, syncer, false)
			case "sweep":
				old := time.Now().Add(-31 * 24 * time.Hour)
				require.NoError(t, os.Chtimes(files[0], old, old))
				requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
				info, err := os.Stat(files[0])
				require.NoError(t, err)
				require.True(t, info.ModTime().After(old))
			case "identity-retry":
				pgKeptSession(t, local, "b", "project-a", "2026-06-14")
				pgKeptPush(t, syncer, false)
			case "repeated":
				saved, err = os.ReadFile(files[0])
				require.NoError(t, err)
				for range 2 {
					_, err := first.pg.ExecContext(ctx, `UPDATE sessions SET message_count=message_count+1 WHERE id='b'`)
					require.NoError(t, err)
					requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
					after, err := os.ReadFile(files[0])
					require.NoError(t, err)
					require.NotEqual(t, saved, after)
					saved = after
				}
			case "replaced":
				var revision int64
				require.NoError(t, first.pg.QueryRowContext(ctx, `SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'`).Scan(&revision))
				tx, err := first.beginTx(ctx, nil)
				require.NoError(t, err)
				_, err = tx.ExecContext(ctx, `UPDATE messages SET token_usage='{"output_tokens":600}' WHERE session_id='a' AND role='assistant'`)
				require.NoError(t, err)
				_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at=now() WHERE id='a'`)
				require.NoError(t, err)
				_, err = tx.ExecContext(ctx, `UPDATE sync_metadata SET value=$1::bigint::text WHERE key='usage_change_revision'`, revision-1)
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
				var after int64
				require.NoError(t, first.pg.QueryRowContext(ctx, `SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'`).Scan(&after))
				require.Equal(t, revision, after)
			}
			restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
			t.Cleanup(func() { require.NoError(t, restarted.Close()) })
			if mode == "sweep" {
				old := time.Now().Add(-31 * 24 * time.Hour)
				require.NoError(t, os.Chtimes(files[0], old, old))
			}
			var identityError *activityIdentityError
			if mode == "identity-retry" {
				identityError = &activityIdentityError{storeExecutor: restarted.pg}
				restarted.pg = identityError
			}
			got := pgKeptBuild(t, restarted, f, q, mode == "sweep" || mode == "replaced" || mode == "identity-retry")
			switch mode {
			case "restart":
				requirePGKeptEqual(t, want, got)
				pgKeptSession(t, local, "a", "project-a", "2026-06-15")
				pgKeptPush(t, syncer, false)
				assert.Empty(t, pgKeptBuild(t, restarted, f, q, true).Sessions)
			case "replaced":
				assert.Equal(t, 600, got.Report.Totals.OutputTokens)
			case "identity-retry":
				require.Equal(t, 1, identityError.queries)
				require.Len(t, got.Sessions, 2)
				requirePGKeptEqual(t, got, pgKeptBuild(t, restarted, f, q, false))
				require.GreaterOrEqual(t, identityError.queries, 2)
				again := openPGActivityInputStore(t, target, syncer.schema, directory)
				t.Cleanup(func() { require.NoError(t, again.Close()) })
				requirePGKeptEqual(t, got, pgKeptBuild(t, again, f, q, false))
			default:
				requirePGKeptEqual(t, want, got)
			}
		})
	}
}

func TestPGActivityMemoHitDoesNotRestoreEvictedReport(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var store Store
	store.activityReports.MaxFiles = 8
	require.NoError(t, store.activityReports.Open("postgres-activity-reports", "mirror", 4096, 0))
	for i := range 9 {
		selection := fmt.Sprintf("selection-%d|identity|version", i)
		require.True(t, store.activityReports.Put(selection, "identity|version", []byte("report")))
		if i == 0 {
			files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			old := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(files[0], old, old))
		}
	}
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 8)
	entry := pgActivityKept{identity: "identity", checkpoint: "checkpoint"}
	store.keepActivityReport("selection-0", "version", entry, "identity", false)
	_, ok := store.activityReports.Get("selection-0|identity|version", "identity|version")
	require.False(t, ok)
	after, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Equal(t, files, after)
}
