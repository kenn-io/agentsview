//go:build chtest

package clickhouse

import (
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

// Reusing a probe must avoid data scans without hiding replacements or making
// an unchanged report token stale after a background merge.
func TestActivitySourceProbeCachePreservesFreshness(t *testing.T) {
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	ctx := t.Context()
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	store := NewStoreFromDB(conn)
	for _, query := range []string{
		`SYSTEM STOP MERGES messages`,
		`INSERT INTO sessions (id,push_version,data_version,local_modified_at) VALUES ('probe',1,1,'2026-01-10 00:00:00')`,
		`INSERT INTO messages (id,session_id,ordinal,push_version) SELECT number+1,'probe',number,1 FROM numbers(2000)`,
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	want := activity.SourceProbe{
		SessionCount: 1, MaxSessionModified: "2026-01-10 00:00:00.000000",
		MaxDataVersion: 1, MaxMessageID: 2000,
	}
	for _, id := range []string{"probe-cold", "probe-warm"} {
		got, err := store.ActivityReportSourceProbe(chdriver.Context(ctx, chdriver.WithQueryID(id)))
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := conn.ExecContext(ctx, `SYSTEM FLUSH LOGS`)
	require.NoError(t, err)
	var cold, warm uint64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT sumIf(read_rows,query_id='probe-cold'),sumIf(read_rows,query_id='probe-warm') FROM system.query_log WHERE current_database=currentDatabase() AND type='QueryFinish' AND is_initial_query=1`).Scan(&cold, &warm))
	require.Positive(t, cold)
	require.Less(t, warm, cold/4, "unchanged data must reuse the probe without scanning messages")

	for _, step := range []struct {
		sql  string
		want activity.SourceProbe
	}{
		{`INSERT INTO messages (id,session_id,ordinal,push_version) VALUES (17,'probe',1999,2)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1999}},
		{`SYSTEM START MERGES messages`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1999}},
		{`OPTIMIZE TABLE messages FINAL`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1999}},
		{`DELETE FROM messages WHERE id=1999 SETTINGS lightweight_deletes_sync=2`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxMessageID: 1998}},
		{`TRUNCATE TABLE messages`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1}},
		{`INSERT INTO usage_events (id,session_id,push_version) VALUES (42,'probe',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 1, MaxUsageID: 42}},
		{`INSERT INTO sessions (id,push_version,data_version,local_modified_at) VALUES ('probe',2,2,'2026-01-10 00:00:00')`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42}},
		{`INSERT INTO model_pricing (model_pattern,updated_at,push_version) VALUES ('probe','2026-01-11',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42, MaxPricingUpdated: "2026-01-11"}},
		{`INSERT INTO genai_pricing (singleton,updated_at,push_version) VALUES (1,'2026-01-12',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42, MaxPricingUpdated: "2026-01-12"}},
		{`INSERT INTO sync_metadata (key,value,push_version) VALUES ('agentsview_identity_revision:probe','7',1)`, activity.SourceProbe{SessionCount: 1, MaxSessionModified: want.MaxSessionModified, MaxDataVersion: 2, MaxUsageID: 42, MaxPricingUpdated: "2026-01-12", ProjectIdentityGeneration: 7}},
	} {
		_, err = conn.ExecContext(ctx, step.sql)
		require.NoError(t, err)
		for range 2 {
			got, probeErr := store.ActivityReportSourceProbe(ctx)
			require.NoError(t, probeErr)
			require.Equal(t, step.want, got, step.sql)
		}
	}
}
