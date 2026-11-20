//go:build pgtest

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGSavedActivityReportRespectsChangedRowPolicy(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	ctx := t.Context()
	role := fmt.Sprintf("activity_reader_%x", rand.Uint64())
	_, err := syncer.pg.ExecContext(ctx, `CREATE ROLE "`+role+`" LOGIN NOSUPERUSER NOBYPASSRLS`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := syncer.pg.ExecContext(context.WithoutCancel(ctx), `DROP OWNED BY "`+role+`"; DROP ROLE "`+role+`"`)
		require.NoError(t, err)
	})
	_, err = syncer.pg.ExecContext(ctx, `GRANT USAGE ON SCHEMA "`+syncer.schema+`" TO "`+role+`";
 GRANT SELECT ON ALL TABLES IN SCHEMA "`+syncer.schema+`" TO "`+role+`";
 GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system(), pg_catalog.pg_current_wal_lsn(), pg_catalog.pg_walfile_name(pg_lsn) TO "`+role+`";
 CREATE POLICY activity_reader ON sessions FOR SELECT TO "`+role+`" USING (true)`)
	require.NoError(t, err)
	var columns string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT string_agg(quote_ident(column_name), ',') FROM information_schema.columns WHERE table_schema=$1 AND table_name='sessions'`, syncer.schema).Scan(&columns))
	_, err = syncer.pg.ExecContext(ctx, `REVOKE SELECT ON sessions FROM "`+role+`"; GRANT SELECT (`+columns+`) ON sessions TO "`+role+`"`)
	require.NoError(t, err)
	password := setHostedFixturePassword(t, syncer.pg, role)
	target, err := appendConnParams(testPGURL(t), map[string]string{"user": role, "password": password})
	require.NoError(t, err)
	directory := t.TempDir()
	first := openPGActivityInputStore(t, target, syncer.schema, directory)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Len(t, pgKeptBuild(t, first, f, q, true).Sessions, 1)
	require.NoError(t, first.Close())
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	_, err = syncer.pg.ExecContext(ctx, `CREATE TABLE unrelated_private (value text)`)
	require.NoError(t, err)
	withUnrelated := openPGActivityInputStore(t, target, syncer.schema, directory)
	require.Len(t, pgKeptBuild(t, withUnrelated, f, q, false).Sessions, 1)
	require.NoError(t, withUnrelated.Close())
	var allowed string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT string_agg(quote_ident(column_name), ',') FROM information_schema.columns WHERE table_schema=$1 AND table_name='sessions' AND column_name <> 'agent'`, syncer.schema).Scan(&allowed))
	_, err = syncer.pg.ExecContext(ctx, `REVOKE SELECT (`+columns+`) ON sessions FROM "`+role+`"; GRANT SELECT (`+allowed+`) ON sessions TO "`+role+`"`)
	require.NoError(t, err)
	restricted := openPGActivityInputStore(t, target, syncer.schema, directory)
	_, err = restricted.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.Error(t, err)
	require.NoError(t, restricted.Close())
	_, err = syncer.pg.ExecContext(ctx, `GRANT SELECT (agent) ON sessions TO "`+role+`"`)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `ALTER TABLE sessions ENABLE ROW LEVEL SECURITY; ALTER POLICY activity_reader ON sessions USING (false)`)
	require.NoError(t, err)
	restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	var visible int
	require.NoError(t, restarted.pg.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&visible))
	require.Zero(t, visible)
	got := pgKeptBuild(t, restarted, f, q, true)
	require.Empty(t, got.Sessions)
}

func TestPGSavedActivityReportWriteDuringBuild(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	pushed := false
	_, err := store.BuildActivityReportArtifacts(t.Context(), f, q, func(p activity.Progress) {
		if p.Phase == activity.ProgressLoadingUsage && !pushed {
			pushed = true
			pgKeptSession(t, local, "a", "project-a", "2026-06-15")
			pgKeptPush(t, syncer, false)
		}
	})
	require.NoError(t, err)
	require.True(t, pushed)
	require.Len(t, store.runtimeState().activityKept.Snapshot(), 0)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files)
	require.Empty(t, pgKeptBuild(t, store, f, q, true).Sessions)
}

type activityFilenodeProbe struct {
	storeExecutor
	after  func()
	denied bool
}

func (p *activityFilenodeProbe) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "pg_relation_filenode") {
		if p.denied {
			return p.storeExecutor.QueryRowContext(ctx, "SELECT activity_identity_denied()")
		}
		row := p.storeExecutor.QueryRowContext(ctx, query, args...)
		if p.after != nil {
			after := p.after
			p.after = nil
			after()
		}
		return row
	}
	return p.storeExecutor.QueryRowContext(ctx, query, args...)
}

func TestPGActivityReportPurgeBetweenIdentityAndValidation(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Len(t, pgKeptBuild(t, store, f, q, true).Sessions, 1)
	purged := false
	store.pg = &activityFilenodeProbe{storeExecutor: store.pg, after: func() {
		_, err := syncer.pg.ExecContext(t.Context(), `TRUNCATE sessions CASCADE`)
		require.NoError(t, err)
		purged = true
	}}
	got, err := store.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	require.True(t, purged)
	require.Empty(t, got.Sessions)
}

func TestPGActivityReportFilenodeDenied(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err := store.pg.ExecContext(t.Context(), `CREATE FUNCTION activity_identity_denied() RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'permission denied' USING ERRCODE = '42501'; END $$`)
	require.NoError(t, err)
	store.pg = &activityFilenodeProbe{storeExecutor: store.pg, denied: true}
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	got, err := store.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	require.Len(t, got.Sessions, 1)
}

func TestPGSavedActivityReportInvalidDoesNotRefresh(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, _ := newUsageJournalFixture(t)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.True(t, store.activityReports.Put("slot|identity|version", "identity|version", []byte("invalid")))
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(files[0], old, old))
	_, ok := store.loadActivityReport("slot", "version", "identity")
	require.False(t, ok)
	info, err := os.Stat(files[0])
	require.NoError(t, err)
	require.WithinDuration(t, old, info.ModTime(), time.Second)
}

func TestPGSavedActivityReportMissingHistoryMarker(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	want := pgKeptBuild(t, store, f, q, true)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	before, err := os.ReadFile(files[0])
	require.NoError(t, err)
	pgKeptSession(t, local, "b", "project-b", "2026-07-14")
	pgKeptPush(t, syncer, false)
	_, err = syncer.pg.ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key='usage_history_checkpoint:' || (SELECT value FROM sync_metadata WHERE key='usage_change_revision')`)
	require.NoError(t, err)
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	after, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after), "saved report changed without a checkpoint")
	entries := store.runtimeState().activityKept.Snapshot()
	require.Len(t, entries, 1)
	entry := entries[0].Rows[0]
	require.Empty(t, entry.checkpoint)
	data, err := entry.encode()
	require.NoError(t, err)
	require.True(t, store.activityReports.Put(entries[0].Slot+"|"+entry.identity+"|"+entries[0].Version, entry.identity+"|"+entries[0].Version, data))
	restarted := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, q, true))
}

func TestPGActivityReportWithoutHistoryCheckpoint(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	_, err := syncer.pg.ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key='usage_history_checkpoint:' || (SELECT value FROM sync_metadata WHERE key='usage_change_revision')`)
	require.NoError(t, err)
	directory := t.TempDir()
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	want := pgKeptBuild(t, store, f, q, true)
	require.Len(t, want.Sessions, 1)
	requirePGKeptEqual(t, want, pgKeptBuild(t, store, f, q, false))
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files)
	require.NoError(t, store.Close())
	restarted := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, q, true))
}

func TestPGSavedActivityReportNewYorkRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	first := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	q, f := pgDayQuery(t, "2026-06-14", "America/New_York"), db.AnalyticsFilter{Timezone: "America/New_York"}
	want := pgKeptBuild(t, first, f, q, true)
	require.Len(t, want.Sessions, 1)
	require.NoError(t, first.Close())
	restarted := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, q, false))
}

func TestPGActivityReportViewReadsFreshRows(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	_, err := syncer.pg.ExecContext(t.Context(), `ALTER TABLE messages RENAME TO messages_data; CREATE VIEW messages AS SELECT * FROM messages_data`)
	require.NoError(t, err)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Equal(t, 500, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
	_, err = syncer.pg.ExecContext(t.Context(), `UPDATE messages_data SET token_usage='{"output_tokens":700}' WHERE session_id='a' AND role='assistant'`)
	require.NoError(t, err)
	require.Equal(t, 700, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
}

func TestPGActivityReportToolResultChanges(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptSession(t, local, "b", "project-b", "2026-06-14")
	pgKeptPush(t, syncer, false)
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, t.TempDir())
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	pgKeptBuild(t, store, f, q, true)
	for _, change := range []struct {
		statement string
		ids       []string
	}{
		{`INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, source, status, content, timestamp) VALUES ('a', 1, 'tool_execution', 'completed', '', '2026-06-14 10:10:00+00')`, []string{"a"}},
		{`UPDATE tool_result_events SET session_id='b', timestamp='2026-06-14 10:20:00+00' WHERE session_id='a'`, []string{"a", "b"}},
		{`DELETE FROM tool_result_events WHERE session_id='b'`, []string{"b"}},
	} {
		before, err := store.UsageChangeRevision(t.Context())
		require.NoError(t, err)
		_, err = syncer.pg.ExecContext(t.Context(), change.statement)
		require.NoError(t, err)
		after, ids, err := store.LoadUsageChanges(t.Context(), before)
		require.NoError(t, err)
		require.Greater(t, after, before)
		require.Equal(t, change.ids, ids)
		pgKeptBuild(t, store, f, q, true)
	}
}

func TestPGSavedActivityReportTerminalResultChanges(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-13")
	pgKeptSession(t, local, "b", "project-b", "2026-07-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	require.Empty(t, pgKeptBuild(t, store, f, q, true).Sessions)
	require.NoError(t, store.Close())
	for _, change := range []struct {
		statement string
		wantID    string
		wantRead  bool
	}{
		{`INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, source, status, content, timestamp) VALUES ('a', 1, 'tool_execution', 'completed', '', '2026-06-14 10:10:00+00')`, "a", true},
		{`UPDATE tool_result_events SET status='running' WHERE session_id='a'`, "", true},
		{`UPDATE tool_result_events SET status='completed' WHERE session_id='a'`, "a", true},
		{`DELETE FROM tool_result_events WHERE session_id='a'`, "", true},
		{`INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, source, status, content, timestamp) VALUES ('b', 1, 'tool_execution', 'completed', '', '2026-07-14 10:10:00+00')`, "", false},
	} {
		_, err := syncer.pg.ExecContext(t.Context(), change.statement)
		require.NoError(t, err)
		store = openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
		got := pgKeptBuild(t, store, f, q, change.wantRead)
		if change.wantID == "" {
			require.Empty(t, got.Sessions)
		} else {
			require.Len(t, got.Sessions, 1)
			require.Equal(t, change.wantID, got.Sessions[0].SessionID)
		}
		require.NoError(t, store.Close())
	}
}

func TestPGActivityReportToolResultJournalUpgrade(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	first := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	pgKeptBuild(t, first, f, q, true)
	require.NoError(t, first.Close())
	_, err := syncer.pg.ExecContext(t.Context(), `DROP FUNCTION mark_usage_tool_result_events() CASCADE;
 UPDATE sync_metadata SET value='3' WHERE key='usage_change_journal_version';
 INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, source, status, content, timestamp) VALUES ('a', 1, 'tool_execution', 'completed', '', '2026-06-14 10:10:00+00')`)
	require.NoError(t, err)
	untracked := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	pgKeptBuild(t, untracked, f, q, true)
	pgKeptBuild(t, untracked, f, q, true)
	require.NoError(t, untracked.Close())
	require.True(t, ensureUsageChangeJournalPG(t.Context(), syncer.pg))
	upgraded := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	pgKeptBuild(t, upgraded, f, q, true)
}
