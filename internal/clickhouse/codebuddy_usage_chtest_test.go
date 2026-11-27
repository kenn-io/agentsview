//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// TestCodeBuddyForkedUsageMatchesSQLite checks the ClickHouse push and daily
// usage read against the same forked CodeBuddy archive. One shared cb:
// billing identity must count once, while the retry under another request
// identity remains a separate usage row.
func TestCodeBuddyForkedUsageMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local := dbtest.SeedCodeBuddyForkedArchive(t)
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}

	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	pushed, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, pushed.Errors, "no session errors")
	assert.Equal(t, 2, pushed.SessionsPushed, "both fork sessions pushed")

	remote, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	conn := chtest.Open(t, dsn, database)
	for _, tc := range []struct {
		id   string
		want int
	}{{
		id: dbtest.CodeBuddyParentID, want: 4,
	}, {
		id: dbtest.CodeBuddyChildID, want: 6,
	}} {
		assert.Equal(t, tc.want, chtest.Count(t, conn, "messages", "session_id = ?", tc.id),
			"ClickHouse has every message of %s", tc.id)
	}
	assert.Equal(t, 2, chtest.Count(t, conn, "messages", "source_uuid = ?", dbtest.CodeBuddyInheritedID),
		"the inherited response is stored in both sessions")

	want, err := local.GetDailyUsage(ctx, codebuddyForkFilter())
	require.NoError(t, err)
	got, err := remote.GetDailyUsage(ctx, codebuddyForkFilter())
	require.NoError(t, err)
	dbtest.AssertCodeBuddyDailyTotals(t, want, "SQLite")
	dbtest.AssertCodeBuddyDailyTotals(t, got, "ClickHouse")
	assert.Equal(t, want.Totals.InputTokens, got.Totals.InputTokens, "daily input tokens")
	assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens, "daily output tokens")
}

func codebuddyForkFilter() db.UsageFilter {
	return db.UsageFilter{
		From: dbtest.CodeBuddyDay, To: dbtest.CodeBuddyDay,
		Timezone: "UTC", Agent: dbtest.CodeBuddyAgentName,
	}
}
