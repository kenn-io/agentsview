//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/storage"
)

// compactions lists the tables the database's OPTIMIZE statements finished on.
func compactions(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	ctx := t.Context()
	_, err := conn.ExecContext(ctx, "SYSTEM FLUSH LOGS")
	require.NoError(t, err)
	rows, err := conn.QueryContext(ctx, `SELECT extract(query, '^OPTIMIZE TABLE (\w+) FINAL') FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish' AND startsWith(query, 'OPTIMIZE TABLE ')
		ORDER BY event_time_microseconds`)
	require.NoError(t, err)
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	require.NoError(t, rows.Err())
	return tables
}

func TestPushCompactsAfterLargeRewrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	conn := chtest.Open(t, target.URL, target.Database)

	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	for i := range 12 {
		appendMessage(t, local, fixtureAlphaID, fmt.Sprintf("alpha reply %d", i),
			fmt.Sprintf("2026-01-10T00:%02d:00.000Z", 10+i))
	}
	before := len(compactions(t, conn))
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	// The push rewrote alpha's 14 messages out of 16 live ones.
	compacted := compactions(t, conn)[before:]
	assert.Contains(t, compacted, "messages")
	assert.Contains(t, compacted, "sessions")
	var stored uint64
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count() FROM messages SETTINGS final = 0").Scan(&stored))
	assert.Equal(t, uint64(16), stored, "compaction leaves one stored row per live message")

	// Rewriting the child session's 2 messages out of 17 stays below the share.
	appendMessage(t, local, fixtureChildID, "child reply", "2026-01-10T00:30:00.000Z")
	before = len(compactions(t, conn))
	result, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, result.MessagesPushed)
	assert.Empty(t, compactions(t, conn)[before:])
}

func TestPushSucceedsWithoutOptimizeGrant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	admin := chtest.Open(t, target.URL, target.Database)
	user := target.Database + "_pusher"
	_, err := admin.ExecContext(ctx, "CREATE USER "+user+" IDENTIFIED WITH no_password")
	require.NoError(t, err)
	t.Cleanup(func() {
		// The prepared usage view names the pusher as its definer, so the
		// user can be dropped only after the database.
		ctx := context.WithoutCancel(t.Context())
		_, err := admin.ExecContext(ctx, "DROP DATABASE "+target.Database+" SYNC")
		require.NoError(t, err)
		_, err = admin.ExecContext(ctx, "DROP USER "+user)
		require.NoError(t, err)
	})
	for _, stmt := range []string{
		"GRANT ALL ON " + target.Database + ".* TO " + user,
		"GRANT SELECT ON system.* TO " + user,
		"REVOKE OPTIMIZE ON " + target.Database + ".* FROM " + user,
	} {
		_, err = admin.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	parsed, err := url.Parse(target.URL)
	require.NoError(t, err)
	parsed.User = url.User(user)
	pusher := target
	pusher.URL = parsed.String()
	s := newTestSync(t, local, pusher, storage.PusherOptions{})

	result, err := s.Push(ctx, false, nil)
	require.NoError(t, err, "a push whose compaction is denied still succeeds")
	assert.Equal(t, 4, result.MessagesPushed)
	assert.Equal(t, 4, chtest.Count(t, admin, "messages", ""))
	assert.Empty(t, compactions(t, admin))

	status, err := ReadStatus(ctx, pusher, fixtureMachine, s.archiveID, nil, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, status.LastPushAt, "the push advanced its cursor")
}
