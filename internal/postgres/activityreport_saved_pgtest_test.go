//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
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

func TestPGActivityReportRestoreDuringBuild(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	ctx := t.Context()
	var revision, sessionRevision int64
	var checkpoint, updatedAt string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT r.value::bigint, c.value, s.updated_at::text, u.revision FROM sync_metadata r JOIN sync_metadata c ON c.key='usage_history_checkpoint:' || r.value CROSS JOIN sessions s JOIN usage_session_changes u ON u.session_id=s.id WHERE r.key='usage_change_revision' AND s.id='a'`).Scan(&revision, &checkpoint, &updatedAt, &sessionRevision))

	directory := t.TempDir()
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	pushed, restored := false, false
	_, err := store.BuildActivityReportArtifacts(ctx, f, q, func(p activity.Progress) {
		switch {
		case p.Phase == activity.ProgressLoadingUsage && !pushed:
			pushed = true
			require.NoError(t, local.ReplaceSessionMessages(ctx, "a", []db.Message{
				{SessionID: "a", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-14T10:00:00Z"},
				{SessionID: "a", Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":700}`)},
			}))
			pgKeptPush(t, syncer, false)
			var pushedRevision int64
			var tokens string
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT r.value::bigint, m.token_usage FROM sync_metadata r CROSS JOIN messages m WHERE r.key='usage_change_revision' AND m.session_id='a' AND m.role='assistant'`).Scan(&pushedRevision, &tokens))
			require.Greater(t, pushedRevision, revision)
			require.JSONEq(t, `{"output_tokens":700}`, tokens)
		case p.Phase == activity.ProgressFinalizing && pushed && !restored:
			restored = true
			// Restore the original rows and journal in one transaction, as a data restore would.
			tx, err := syncer.pg.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE messages SET token_usage='{"output_tokens":500}' WHERE session_id='a' AND role='assistant'`)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at=$1::timestamptz WHERE id='a'`, updatedAt)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE usage_session_changes SET revision=$1 WHERE session_id='a'`, sessionRevision)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `DELETE FROM usage_session_changes WHERE revision > $1`, revision)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE sync_metadata SET value=$1::bigint::text WHERE key='usage_change_revision'`, revision)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE sync_metadata SET value=$2 WHERE key IN ('usage_history_token', 'usage_history_checkpoint:' || $1::bigint::text)`, revision, checkpoint)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key LIKE 'usage_history_checkpoint:%' AND substring(key from length('usage_history_checkpoint:') + 1)::bigint > $1`, revision)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
		}
	})
	require.NoError(t, err)
	require.True(t, pushed)
	require.True(t, restored)
	again, err := store.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.NoError(t, err)
	assert.Equal(t, 500, again.Report.Totals.OutputTokens)
	restarted := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	afterRestart, err := restarted.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.NoError(t, err)
	assert.Equal(t, 500, afterRestart.Report.Totals.OutputTokens)
}

func TestPGSavedActivityReportMemoryVisits(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	want := pgKeptBuild(t, store, f, q, true)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	old := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(files[0], old, old))
	require.True(t, store.activityReports.Put("expired", "v", []byte("report")))
	allFiles, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, allFiles, 2)
	stale := allFiles[0]
	if stale == files[0] {
		stale = allFiles[1]
	}
	expired := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(stale, expired, expired))
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	_, err = os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist)
	info, err := os.Stat(files[0])
	require.NoError(t, err)
	assert.True(t, info.ModTime().After(old))
	pgKeptSession(t, local, "b", "project-b", "2026-07-14")
	pgKeptPush(t, syncer, false)
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	entries := store.runtimeState().activityKept.Snapshot()
	require.Len(t, entries, 1)
	entry := entries[0]
	data, ok := store.activityReports.Get(entry.Slot+"|"+entry.Rows[0].identity+"|"+entry.Version, entry.Rows[0].identity+"|"+entry.Version)
	require.True(t, ok)
	var saved pgActivitySavedReport
	require.NoError(t, json.Unmarshal(data, &saved))
	var revision int64
	var checkpoint string
	require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT r.value::bigint, c.value FROM sync_metadata r JOIN sync_metadata c ON c.key='usage_history_checkpoint:' || r.value WHERE r.key='usage_change_revision'`).Scan(&revision, &checkpoint))
	require.Equal(t, revision, saved.Revision)
	require.Equal(t, checkpoint, saved.Checkpoint)
	require.NoError(t, os.Remove(files[0]))
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPGSavedActivityReportRestoreBetweenValidationReads(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptSession(t, local, "b", "project-b", "2026-07-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Equal(t, 500, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
	var revision int64
	require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'`).Scan(&revision))
	_, err := syncer.pg.ExecContext(t.Context(), `UPDATE sessions SET message_count=message_count+1 WHERE id='b'`)
	require.NoError(t, err)
	var temporaryToken string
	require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT value FROM sync_metadata WHERE key='usage_history_token'`).Scan(&temporaryToken))
	tx, err := syncer.pg.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `LOCK TABLE sessions IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		report activity.CandidateArtifacts
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := store.BuildActivityReportArtifacts(ctx, f, q, nil)
		done <- result{report, err}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := syncer.pg.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%FROM sessions s%' AND query LIKE '%s.id = ANY%')`).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 50*time.Millisecond)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM usage_session_changes WHERE revision > $1`, revision)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sync_metadata SET value=$1::bigint::text WHERE key='usage_change_revision'`, revision)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE messages SET token_usage='{"output_tokens":700}' WHERE session_id='a' AND role='assistant'`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sessions SET updated_at=now() WHERE id='a'`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `SET CONSTRAINTS ALL IMMEDIATE`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sync_metadata SET value=$1 WHERE key='usage_history_token'`, temporaryToken)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, 500, got.report.Report.Totals.OutputTokens)
	require.Equal(t, 700, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
}

func TestPGSavedActivityReportRecoversIdentity(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	probe := &activityIdentityError{storeExecutor: store.pg}
	store.pg = probe
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	want := pgKeptBuild(t, store, f, q, true)
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	require.GreaterOrEqual(t, probe.queries, 2)
	restarted := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, q, false))
}

func TestPGSavedActivityReportTruncated(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	first := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Len(t, pgKeptBuild(t, first, f, q, true).Sessions, 1)
	require.NoError(t, first.Close())
	restarted := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	require.Len(t, pgKeptBuild(t, restarted, f, q, false).Sessions, 1)
	require.NoError(t, restarted.Close())
	pgKeptSession(t, local, "b", "project-b", "2026-07-14")
	pgKeptPush(t, syncer, false)
	_, err := syncer.pg.ExecContext(t.Context(), `TRUNCATE sessions CASCADE`)
	require.NoError(t, err)
	after := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	require.Empty(t, pgKeptBuild(t, after, f, q, true).Sessions)
	require.NoError(t, after.Close())
	again := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, again.Close()) })
	require.Empty(t, pgKeptBuild(t, again, f, q, false).Sessions)
}
