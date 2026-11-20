//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
 REVOKE SELECT ON pricing_meta, cursor_usage_events FROM "`+role+`";
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

func TestPGActivityReportPurge(t *testing.T) {
	t.Run("during validation", func(t *testing.T) {
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
	})
	t.Run("before restart", func(t *testing.T) {
		t.Setenv("CACHE_DIRECTORY", t.TempDir())
		local, syncer, _ := newUsageJournalFixture(t)
		pgKeptSession(t, local, "a", "project-a", "2026-06-14")
		pgKeptPush(t, syncer, false)
		directory := t.TempDir()
		first := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
		q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
		require.Len(t, pgKeptBuild(t, first, f, q, true).Sessions, 1)
		require.NoError(t, first.Close())
		_, err := syncer.pg.ExecContext(t.Context(), `TRUNCATE sessions CASCADE`)
		require.NoError(t, err)
		after := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
		require.Empty(t, pgKeptBuild(t, after, f, q, true).Sessions)
		require.NoError(t, after.Close())
		again := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
		t.Cleanup(func() { require.NoError(t, again.Close()) })
		require.Empty(t, pgKeptBuild(t, again, f, q, false).Sessions)
	})
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
	first := pgKeptBuild(t, store, f, q, true)
	require.Len(t, first.Sessions, 1)
	requirePGKeptEqual(t, first, pgKeptBuild(t, store, f, q, true))
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestPGActivityTableIdentityPartitionedInput(t *testing.T) {
	_, _, store := newUsageJournalFixture(t)
	tx, err := store.beginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	reader := store.readView(tx)
	identity, err := reader.activityTableIdentity(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, identity)
	_, err = tx.ExecContext(t.Context(), `CREATE TEMP TABLE messages (id int) PARTITION BY LIST (id); CREATE TEMP TABLE messages_part PARTITION OF messages FOR VALUES IN (1)`)
	require.NoError(t, err)
	identity, err = reader.activityTableIdentity(t.Context())
	require.NoError(t, err)
	require.Empty(t, identity)
}
