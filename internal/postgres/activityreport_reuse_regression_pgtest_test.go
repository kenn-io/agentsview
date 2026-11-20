//go:build pgtest

package postgres

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
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
	_, err = syncer.pg.ExecContext(ctx, `ALTER TABLE sessions ENABLE ROW LEVEL SECURITY; ALTER POLICY activity_reader ON sessions USING (false)`)
	require.NoError(t, err)
	hidden, err := first.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.NoError(t, err)
	assert.Empty(t, hidden.Sessions)
	_, err = syncer.pg.ExecContext(ctx, `ALTER TABLE sessions DISABLE ROW LEVEL SECURITY; ALTER POLICY activity_reader ON sessions USING (true)`)
	require.NoError(t, err)
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
	require.NoError(t, restarted.Close())
	_, err = syncer.pg.ExecContext(ctx, `ALTER TABLE sessions DISABLE ROW LEVEL SECURITY; ALTER TABLE sessions RENAME TO sessions_base; CREATE VIEW sessions AS SELECT * FROM sessions_base WHERE false; GRANT SELECT ON sessions TO "`+role+`"`)
	require.NoError(t, err)
	view := openPGActivityInputStore(t, target, syncer.schema, directory)
	t.Cleanup(func() { require.NoError(t, view.Close()) })
	hidden, err = view.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.NoError(t, err)
	require.Empty(t, hidden.Sessions)
}

func TestPGSavedActivityReportNamedZoneRuleChange(t *testing.T) {
	if os.Getenv("ACTIVITY_ZONE_CHILD") == "1" {
		t.Setenv("CACHE_DIRECTORY", os.Getenv("ACTIVITY_ZONE_CACHE"))
		target, schema := os.Getenv("ACTIVITY_ZONE_TARGET"), os.Getenv("ACTIVITY_ZONE_SCHEMA")
		store := openPGActivityInputStore(t, target, schema, os.Getenv("ACTIVITY_ZONE_DIRECTORY"))
		defer store.Close()
		q, err := activity.ResolveQuery(activity.QueryInput{Preset: "custom", From: "1969-06-14T00:00:00Z", To: "1969-06-16T00:00:00Z", Timezone: "Test/Zone", BucketOverride: "1d"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
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
	pgKeptSession(t, local, "a", "project-a", "1969-06-14")
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	for i, offset := range []int32{0, -12 * 60 * 60} {
		zoneDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(zoneDir, "Test"), 0700))
		// Both zones use UTC from 1970 onward, but differ before it.
		zone := make([]byte, 44+4+1+12+4)
		copy(zone, "TZif")
		binary.BigEndian.PutUint32(zone[32:36], 1)
		binary.BigEndian.PutUint32(zone[36:40], 2)
		binary.BigEndian.PutUint32(zone[40:44], 4)
		zone[48] = 1
		binary.BigEndian.PutUint32(zone[49:53], uint32(offset))
		copy(zone[61:], "TST\x00")
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

func TestPGSavedActivityReportConfiguredAlias(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	t.Cleanup(func() { pricing.SetConfiguredAliases(nil) })
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	directory := t.TempDir()
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	for i, target := range []string{"alias-one", "alias-two"} {
		pricing.SetConfiguredAliases(map[string]string{"kept-model": target})
		store := openPGActivityInputStore(t, testPGURL(t), syncer.schema, directory)
		store.SetCustomPricing(map[string]config.CustomModelRate{
			"alias-one": {OutputMicrodollarsPerMTok: 20_000_000},
			"alias-two": {OutputMicrodollarsPerMTok: 40_000_000},
		})
		got := pgKeptBuild(t, store, f, q, true)
		want := []string{"0.01", "0.02"}[i]
		require.Equal(t, money.MustParseDollars(want), got.Report.Totals.Cost)
		require.NoError(t, store.Close())
	}
}

func TestPGActivityTimezoneBoundaryBeforeRange(t *testing.T) {
	start := time.Date(1969, 6, 14, 12, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 9)
	transition := time.Date(1969, 6, 14, 6, 0, 0, 0, time.UTC)
	var keys []string
	var windows [][]activity.BucketWindow
	for _, tc := range []struct {
		offset     int32
		transition time.Time
	}{{0, transition}, {-12 * 60 * 60, transition}, {-12 * 60 * 60, transition.AddDate(0, 0, -14)}} {
		zone := make([]byte, 44+4+1+12+4)
		copy(zone, "TZif")
		binary.BigEndian.PutUint32(zone[32:36], 1)
		binary.BigEndian.PutUint32(zone[36:40], 2)
		binary.BigEndian.PutUint32(zone[40:44], 4)
		binary.BigEndian.PutUint32(zone[44:48], uint32(int32(tc.transition.Unix())))
		zone[48] = 1
		binary.BigEndian.PutUint32(zone[49:53], uint32(tc.offset))
		copy(zone[61:], "TST\x00")
		loc, err := time.LoadLocationFromTZData("Test/Zone", zone)
		require.NoError(t, err)
		buckets, err := activity.BuildBuckets(start, end, activity.BucketSpec{Unit: activity.BucketWeek, NominalSeconds: 604800}, loc)
		require.NoError(t, err)
		windows = append(windows, buckets)
		keys = append(keys, activityTimezoneKey(loc, start, end))
	}
	require.NotEqual(t, windows[0], windows[1])
	require.NotEqual(t, keys[0], keys[1])
	require.Equal(t, windows[0], windows[2])
	require.Equal(t, keys[0], keys[2])
}
