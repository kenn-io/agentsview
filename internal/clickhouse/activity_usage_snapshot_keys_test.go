//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
)

// Key discovery may return obsolete snapshot keys, and the exact time filter
// must not let an older version of a message back in under FINAL. The store
// connection turns exact skip-index mode off so the usage query's own setting
// is what keeps the superseded row out.
func TestActivityUsageIgnoresObsoleteSnapshotKeys(t *testing.T) {
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	opt, err := chdriver.ParseDSN(dsn)
	require.NoError(t, err)
	opt.Auth.Database = database
	opt.Settings = chdriver.Settings{"final": 1, "use_skip_indexes_if_final_exact_mode": 0}
	conn := chdriver.OpenDB(opt)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	store := NewStoreFromDB(conn)
	// Each insert lands in its own usage_messages part. The candidate session
	// stays published at version 1 while its version 2 rows arrive, as a push
	// that has written messages but not yet the session row would leave it.
	for _, query := range []string{
		`SYSTEM STOP MERGES usage_messages`,
		`INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,claude_message_id,claude_request_id,push_version) VALUES
		 ('candidate',0,'2026-01-10 12:00:00','gpt-4o','{"output_tokens":17}','removed','removed',1),
		 ('candidate',1,'2026-01-10 12:00:00','gpt-4o','{"output_tokens":5}','kept','kept',1),
		 ('candidate',2,'2026-01-10 12:00:00','gpt-4o','{"output_tokens":31}','moved','moved',1),
		 ('candidate',3,NULL,'gpt-4o','{"output_tokens":3}','null','null',1),
		 ('peer',0,'2026-01-10 12:00:00','gpt-4o','{"output_tokens":77}','removed','removed',2),
		 ('peer',1,'2026-01-10 12:01:00','gpt-4o','{"output_tokens":11}','kept','kept',2),
		 ('peer',2,'2026-01-10 12:01:00','gpt-4o','{"output_tokens":53}','moved','moved',2)`,
		`INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,claude_message_id,claude_request_id,push_version) VALUES
		 ('candidate',0,'2026-01-10 12:00:00','gpt-4o','','removed','removed',2)`,
		// Only out-of-range timestamps, so the time index can prune this
		// whole part and leave the older in-range version behind.
		`INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,claude_message_id,claude_request_id,push_version) VALUES
		 ('candidate',2,'2026-02-10 12:00:00','gpt-4o','{"output_tokens":31}','moved','moved',2)`,
		`INSERT INTO sessions (id,project,agent,started_at,ended_at,message_count,push_version) VALUES
		 ('candidate','selected','claude','2026-01-10 12:00:00','2026-02-10 12:00:00',4,1),
		 ('peer','elsewhere','claude','2026-01-10 12:00:00','2026-01-10 12:01:00',3,2)`,
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	f := db.AnalyticsFilter{Project: "selected", Timezone: "UTC"}
	got, err := store.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.NoError(t, err)
	require.Equal(t, 14, got.Report.Totals.OutputTokens)
}
