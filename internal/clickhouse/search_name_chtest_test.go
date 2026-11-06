//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseSearchNameMatches(t *testing.T) {
	t.Parallel()
	local, target := seedFixture(t)
	dbtest.SeedNameSearch(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertNameSearch(t, store)
}

// Search name-branch benchmark archive: every nameBenchStride-th session
// has a name containing the query, and no message content does.
const (
	nameBenchSessions        = 20_000
	nameBenchMessagesPerSess = 100
	nameBenchStride          = 2_000
)

// BenchmarkClickHouseSearchNameMatches measures a search whose rows come
// from the name branch over a large mirror, where only a few sessions match
// by name.
func BenchmarkClickHouseSearchNameMatches(b *testing.B) {
	ctx := b.Context()
	dsn, database := chtest.FreshDatabase(b)
	target := Target{URL: dsn, Database: database}
	require.NoError(b, EnsureSchema(ctx, target))
	conn := chtest.Open(b, dsn, database)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent, first_message,
			session_name, started_at, ended_at, created_at,
			message_count, user_message_count, push_version)
		SELECT concat('bench-', toString(n)), concat('project-', toString(n % 50)),
			'bench', 'claude', concat('first prompt ', toString(n)),
			if(n % ? = 0, concat('namedneedle ', toString(n)), NULL),
			ts, ts + toIntervalHour(1), ts, ?, ?, 1
		FROM (SELECT number AS n,
				toDateTime64('2026-01-01 00:00:00', 6, 'UTC') + toIntervalSecond(n * 600) AS ts
			FROM numbers(?))`,
		nameBenchStride, nameBenchMessagesPerSess, nameBenchMessagesPerSess/2,
		nameBenchSessions)
	require.NoError(b, err, "seed sessions")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content,
			timestamp, content_length, is_system, push_version)
		SELECT toInt64(n), concat('bench-', toString(s)), toInt64(o),
			if(o % 2 = 0, 'user', 'assistant'),
			concat(repeat('lorem ipsum dolor sit amet ', 1 + o % 8), toString(o)),
			toDateTime64('2026-01-01 00:00:00', 6, 'UTC')
				+ toIntervalSecond(s * 600 + o * 30),
			0, o % 20 = 0, 1
		FROM (SELECT number AS n, intDiv(number, ?) AS s, number % ? AS o
			FROM numbers(?))`,
		nameBenchMessagesPerSess, nameBenchMessagesPerSess,
		nameBenchSessions*nameBenchMessagesPerSess)
	require.NoError(b, err, "seed messages")
	store, err := NewStore(ctx, target)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	f := db.SearchFilter{Query: "namedneedle", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := store.Search(ctx, f)
		require.NoError(b, err)
		require.Len(b, page.Results, nameBenchSessions/nameBenchStride)
	}
}
