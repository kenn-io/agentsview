//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// TestCodeBuddyForkedUsageMatchesSQLite checks that the PostgreSQL mirror
// pushes the same SourceUUID-bearing message rows as the archive and that
// its daily usage read deduplicates a forked response once. A genuine
// retry with another cb: request ID must still count.
func TestCodeBuddyForkedUsageMatchesSQLite(t *testing.T) {
	t.Parallel()
	const schema = "agentsview_codebuddy_fork_test"
	ctx := t.Context()
	pgURL := testPGSchemaURL(t, schema)
	local := dbtest.SeedCodeBuddyForkedArchive(t)

	syncer, err := New(ctx, pgURL, schema, local, dbtest.CodeBuddyMachine, true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	pushed, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, pushed.Errors, "no session errors")
	assert.Equal(t, 2, pushed.SessionsPushed, "both fork sessions pushed")

	remote, err := NewStore(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	for _, tc := range []struct {
		id   string
		want int
	}{{
		id: dbtest.CodeBuddyParentID, want: 4,
	}, {
		id: dbtest.CodeBuddyChildID, want: 6,
	}} {
		assert.Equal(t, tc.want, countPGMessages(t, remote, tc.id),
			"PostgreSQL has every message of %s", tc.id)
	}
	assert.Equal(t, 2, countPGSourceUUID(t, remote, dbtest.CodeBuddyInheritedID),
		"the inherited response is stored in both sessions")

	want, err := local.GetDailyUsage(ctx, codebuddyForkFilter())
	require.NoError(t, err)
	got, err := remote.GetDailyUsage(ctx, codebuddyForkFilter())
	require.NoError(t, err)
	dbtest.AssertCodeBuddyDailyTotals(t, want, "SQLite")
	dbtest.AssertCodeBuddyDailyTotals(t, got, "PostgreSQL")
	assert.Equal(t, want.Totals.InputTokens, got.Totals.InputTokens, "daily input tokens")
	assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens, "daily output tokens")
}

func codebuddyForkFilter() db.UsageFilter {
	return db.UsageFilter{
		From: dbtest.CodeBuddyDay, To: dbtest.CodeBuddyDay,
		Timezone: "UTC", Agent: dbtest.CodeBuddyAgentName,
	}
}

func countPGMessages(t *testing.T, store *Store, sessionID string) int {
	t.Helper()
	var count int
	require.NoError(t, store.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM messages WHERE session_id = $1`, sessionID,
	).Scan(&count))
	return count
}

func countPGSourceUUID(t *testing.T, store *Store, sourceUUID string) int {
	t.Helper()
	var count int
	require.NoError(t, store.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM messages WHERE source_uuid = $1`, sourceUUID,
	).Scan(&count))
	return count
}
