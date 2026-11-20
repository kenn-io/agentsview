//go:build pgtest

package postgres

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestPGSavedActivityReportNamedZoneRuleChange(t *testing.T) {
	if os.Getenv("ACTIVITY_ZONE_CHILD") == "1" {
		t.Setenv("CACHE_DIRECTORY", os.Getenv("ACTIVITY_ZONE_CACHE"))
		target, schema := os.Getenv("ACTIVITY_ZONE_TARGET"), os.Getenv("ACTIVITY_ZONE_SCHEMA")
		store := openPGActivityInputStore(t, target, schema, os.Getenv("ACTIVITY_ZONE_DIRECTORY"))
		defer store.Close()
		q, err := activity.ResolveQuery(activity.QueryInput{Preset: "custom", From: "2026-06-14T00:00:00Z", To: "2026-06-16T00:00:00Z", Timezone: "Test/Zone", BucketOverride: "1d"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		loaded := false
		got, err := store.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{Timezone: "Test/Zone"}, q, func(p activity.Progress) { loaded = loaded || p.Phase == activity.ProgressLoadingUsage })
		require.NoError(t, err)
		data, err := json.Marshal(struct {
			Loaded  bool
			Buckets []activity.Bucket
		}{loaded, got.Report.Buckets})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(os.Getenv("ACTIVITY_ZONE_OUTPUT"), data, 0600))
		return
	}
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	zones, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, zones.Close()) })
	for i, source := range []string{"UTC", "Etc/GMT+12"} {
		zoneDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(zoneDir, "Test"), 0700))
		zoneFile, err := zones.Open(source)
		require.NoError(t, err)
		zone, err := io.ReadAll(zoneFile)
		require.NoError(t, err)
		require.NoError(t, zoneFile.Close())
		require.NoError(t, os.WriteFile(filepath.Join(zoneDir, "Test", "Zone"), zone, 0600))
		out := filepath.Join(t.TempDir(), "report.json")
		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestPGSavedActivityReportNamedZoneRuleChange$")
		cmd.Env = append(os.Environ(), "ACTIVITY_ZONE_CHILD=1", "ACTIVITY_ZONE_TARGET="+target, "ACTIVITY_ZONE_SCHEMA="+syncer.schema, "ACTIVITY_ZONE_DIRECTORY="+directory, "ACTIVITY_ZONE_OUTPUT="+out, "ACTIVITY_ZONE_CACHE="+os.Getenv("CACHE_DIRECTORY"), "ZONEINFO="+zoneDir)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		data, err := os.ReadFile(out)
		require.NoError(t, err)
		var got struct {
			Loaded  bool
			Buckets []activity.Bucket
		}
		require.NoError(t, json.Unmarshal(data, &got))
		files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "postgres-activity-reports", "*", "*.gob"))
		require.NoError(t, err)
		require.NotEmpty(t, files)
		require.Len(t, got.Buckets, 2+i)
		require.True(t, got.Loaded, "run %d must rebuild", i)
	}
}

type activityHistorySwitch struct {
	storeExecutor
	switchHistory func(context.Context) error
	err           error
}

func (s *activityHistorySwitch) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := s.storeExecutor.QueryContext(ctx, query, args...)
	if err == nil && s.switchHistory != nil && strings.Contains(query, "LEFT JOIN usage_session_changes c") {
		s.err = s.switchHistory(ctx)
		s.switchHistory = nil
	}
	return rows, err
}

func TestPGSavedActivityReportHistoryChangesBetweenReads(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptSession(t, local, "b", "project-b", "2026-07-14")
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	store := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Equal(t, 500, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
	var revision int64
	require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'`).Scan(&revision))
	_, err := syncer.pg.ExecContext(t.Context(), `UPDATE sessions SET message_count=message_count+1 WHERE id='b'`)
	require.NoError(t, err)
	switcher := &activityHistorySwitch{storeExecutor: store.pg}
	switcher.switchHistory = func(ctx context.Context) error {
		tx, err := syncer.pg.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `DELETE FROM usage_session_changes WHERE revision > $1`, revision); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sync_metadata SET value=$1::bigint::text WHERE key='usage_change_revision'`, revision); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET token_usage='{"output_tokens":700}' WHERE session_id='a' AND role='assistant'`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at=now() WHERE id='a'`); err != nil {
			return err
		}
		return tx.Commit()
	}
	store.pg = switcher
	first := pgKeptBuild(t, store, f, q, false)
	require.NoError(t, switcher.err)
	require.Nil(t, switcher.switchHistory)
	require.Equal(t, 500, first.Report.Totals.OutputTokens)
	var through int64
	require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT value::bigint FROM sync_metadata WHERE key='usage_change_revision'`).Scan(&through))
	require.Equal(t, revision+1, through)
	got := pgKeptBuild(t, store, f, q, true)
	require.Equal(t, 700, got.Report.Totals.OutputTokens)
}
