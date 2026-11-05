//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBSearchNameMatches(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedNameSearch(t, local)
	path := filepath.Join(t.TempDir(), "name-search.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
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

// BenchmarkDuckDBSearchNameMatches measures a search whose rows come from
// the name branch over a large in-memory mirror, where only a few sessions
// match by name.
func BenchmarkDuckDBSearchNameMatches(b *testing.B) {
	ctx := b.Context()
	conn, err := openDuckDB("")
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	require.NoError(b, createSchema(ctx, conn))
	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent, first_message,
			session_name, started_at, ended_at, created_at,
			message_count, user_message_count)
		SELECT 'bench-' || i, 'project-' || (i % 50), 'bench', 'claude',
			'first prompt ' || i,
			CASE WHEN i % ? = 0 THEN 'namedneedle ' || i END,
			ts, ts + INTERVAL 1 HOUR, ts, ?, ?
		FROM (SELECT i, TIMESTAMP '2026-01-01' + to_seconds(i * 600) AS ts
			FROM range(?) t(i))`,
		nameBenchStride, nameBenchMessagesPerSess, nameBenchMessagesPerSess/2,
		nameBenchSessions)
	require.NoError(b, err, "seed sessions")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content,
			timestamp, content_length, is_system)
		SELECT row_number() OVER (), s.id, o,
			CASE WHEN o % 2 = 0 THEN 'user' ELSE 'assistant' END,
			repeat('lorem ipsum dolor sit amet ', 1 + o % 8) || o,
			s.started_at + to_seconds(o * 30), 0, o % 20 = 0
		FROM sessions s, range(?) r(o)`, nameBenchMessagesPerSess)
	require.NoError(b, err, "seed messages")
	store := NewStoreFromDB(conn)
	f := db.SearchFilter{Query: "namedneedle", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := store.Search(ctx, f)
		require.NoError(b, err)
		require.Len(b, page.Results, nameBenchSessions/nameBenchStride)
	}
}
