//go:build chtest

package clickhouse

import (
	"strings"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
)

func TestClickHouseSearchMemory(t *testing.T) {
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	require.NoError(t, EnsureSchema(ctx, target))
	conn := chtest.Open(t, dsn, database)
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, project, machine, agent, first_message, started_at, ended_at, created_at, message_count, user_message_count, push_version)
		SELECT concat('session-', leftPad(toString(number), 5, '0')), 'project', 'machine', 'claude', repeat('preview ', 20),
			toDateTime64('2026-01-01 00:00:00', 6, 'UTC') + toIntervalSecond(number),
			toDateTime64('2026-01-01 00:00:00', 6, 'UTC') + toIntervalSecond(number),
			toDateTime64('2026-01-01 00:00:00', 6, 'UTC'), 40, 40, 1 FROM numbers(24000)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO messages (id, session_id, ordinal, role, content, timestamp, content_length, is_system, push_version)
		SELECT toInt64(number), concat('session-', leftPad(toString(intDiv(number, 40)), 5, '0')), toInt64(number % 40), 'user',
			concat('context ', repeat('sample text ', 42)), toDateTime64('2026-01-01 00:00:00', 6, 'UTC'), 512, false, 1 FROM numbers(960000)`)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ctx = chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{"max_memory_usage": 2147483648}))
	page, err := store.Search(ctx, db.SearchFilter{Query: "context", Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Results, 50)
	require.Equal(t, "session-23999", page.Results[0].SessionID)
	require.Equal(t, "session-23950", page.Results[49].SessionID)
}

func TestClickHouseSearchResultsAndPages(t *testing.T) {
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	require.NoError(t, EnsureSchema(ctx, target))
	conn := chtest.Open(t, dsn, database)
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, project, machine, agent, session_name, started_at, ended_at, created_at, push_version)
		SELECT id, 'project', 'machine', 'claude', name, ts, ts, ts, 1
		FROM (SELECT arrayJoin([('a', 'other'), ('b', 'other'), ('c', 'context name only'), ('d', 'other')]) AS row,
			row.1 AS id, row.2 AS name, toDateTime64('2026-01-01 00:00:00', 6, 'UTC') AS ts)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO messages (id, session_id, ordinal, role, content, timestamp, content_length, is_system, push_version)
		SELECT row.1, row.2, row.3, 'user', row.4, toDateTime64('2026-01-01 00:00:00', 6, 'UTC'), length(row.4), false, 1
		FROM (SELECT arrayJoin([(toInt64(1), 'a', toInt64(0), 'later context extra'),
			(toInt64(2), 'a', toInt64(2), 'context extra café'),
			(toInt64(3), 'a', toInt64(1), concat('context extra 世界', repeat('界', 200))),
			(toInt64(4), 'b', toInt64(0), 'context extra second'),
			(toInt64(5), 'c', toInt64(0), 'ordinary message'),
			(toInt64(6), 'd', toInt64(0), 'context alone')]) AS row)`)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := db.SearchFilter{Query: "context", Limit: 1}
	for _, want := range []struct {
		id, snippet   string
		ordinal, next int
	}{
		{"a", string([]rune("context extra 世界" + strings.Repeat("界", 200))[:200]), 1, 1},
		{"b", "context extra second", 0, 2},
		{"d", "context alone", 0, 3},
	} {
		page, err := store.Search(ctx, f)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Equal(t, want.id, page.Results[0].SessionID)
		require.Equal(t, want.ordinal, page.Results[0].Ordinal)
		require.Equal(t, want.snippet, page.Results[0].Snippet)
		require.Equal(t, want.next, page.NextCursor)
		f.Cursor = page.NextCursor
	}
	page, err := store.Search(ctx, f)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "c", page.Results[0].SessionID)
	require.Zero(t, page.NextCursor)

	page, err = store.Search(ctx, db.SearchFilter{Query: "context extra", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 2)
	require.Equal(t, "a", page.Results[0].SessionID)
	require.Equal(t, "b", page.Results[1].SessionID)
}
