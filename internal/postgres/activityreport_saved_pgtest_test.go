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

func TestPGActivityReportSaved(t *testing.T) {
	for _, mode := range []string{"restart", "sweep", "legacy", "replaced", "pricing", "today", "local"} {
		t.Run(mode, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("CACHE_DIRECTORY", cache)
			local, syncer, _ := newUsageJournalFixture(t)
			pgKeptSession(t, local, "a", "project-a", "2026-06-14")
			require.NoError(t, local.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("a", 14)))
			pgKeptPush(t, syncer, false)
			ctx := t.Context()
			if mode == "legacy" {
				_, err := syncer.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='1' WHERE key='usage_change_journal_version'; DELETE FROM sync_metadata WHERE key='usage_history_token' OR key LIKE 'usage_history_checkpoint:%'`)
				require.NoError(t, err)
			}
			target, directory := testPGURL(t), t.TempDir()
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			if mode == "local" {
				q, f = pgDayQuery(t, "2026-06-14", "Local"), db.AnalyticsFilter{Timezone: "Local"}
			}
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
			want := pgKeptBuild(t, first, f, q, true)
			files, err := filepath.Glob(filepath.Join(cache, "postgres-activity-reports", "*", "*.gob"))
			require.NoError(t, err)
			if mode == "legacy" || mode == "today" || mode == "local" {
				require.Empty(t, files)
				if mode == "today" || mode == "local" {
					require.Len(t, first.runtimeState().activityKept.Snapshot(), 1)
					if mode == "local" {
						requirePGKeptEqual(t, want, pgKeptBuild(t, first, f, q, false))
						restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
						t.Cleanup(func() { require.NoError(t, restarted.Close()) })
						requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, q, true))
					}

				}
				return
			}
			require.Len(t, files, 1)
			switch mode {
			case "restart":
				pgKeptSession(t, local, "b", "project-b", "2026-07-14")
				pgKeptPush(t, syncer, false)
			case "sweep":
				old := time.Now().Add(-31 * 24 * time.Hour)
				require.NoError(t, os.Chtimes(files[0], old, old))

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
			got := pgKeptBuild(t, restarted, f, q, mode == "sweep" || mode == "replaced")
			switch mode {
			case "restart":
				requirePGKeptEqual(t, want, got)
				pgKeptSession(t, local, "a", "project-a", "2026-06-15")
				pgKeptPush(t, syncer, false)
				assert.Empty(t, pgKeptBuild(t, restarted, f, q, true).Sessions)
			case "replaced":
				assert.Equal(t, 600, got.Report.Totals.OutputTokens)
			default:
				requirePGKeptEqual(t, want, got)
			}
		})
	}
}
