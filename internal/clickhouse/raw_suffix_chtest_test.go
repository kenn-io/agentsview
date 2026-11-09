//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseFindSessionIDsByRawSuffix(t *testing.T) {
	t.Parallel()
	local, target := seedFixture(t)
	dbtest.SeedRawSuffix(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	// The trashed session's live version stays in an older part, where the
	// unmerged candidate read finds it; the FINAL read must still drop it.
	// Stopping merges keeps that part from collapsing into the new version.
	conn := chtest.Open(t, target.URL, target.Database)
	_, err = conn.ExecContext(t.Context(), "SYSTEM STOP MERGES sessions")
	require.NoError(t, err)
	dbtest.TrashRawSuffix(t, local)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	var liveVersions int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT toInt64(count()) FROM sessions
		WHERE id = 'host~trashed' AND deleted_at IS NULL SETTINGS final = 0`).Scan(&liveVersions))
	require.Equal(t, 1, liveVersions, "the mirror must keep the pre-trash version unmerged")
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertRawSuffix(t, store)
}

// A mirror created before the id_tail index gains it on the next schema
// setup, and later setups do not rebuild it.
func TestClickHouseSessionIDTailIndexUpgrade(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	for _, table := range mirrorTables {
		table.indexes = nil
		_, err := conn.ExecContext(ctx, table.createSQL())
		require.NoError(t, err)
	}
	_, err := conn.ExecContext(ctx,
		"INSERT INTO sessions (id, push_version) VALUES ('host~codex:upgrade', 1)")
	require.NoError(t, err)

	materializations := func() int {
		t.Helper()
		var n int
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT toInt64(count()) FROM system.mutations
			WHERE database = currentDatabase() AND table = 'sessions'
			  AND command LIKE '%MATERIALIZE INDEX id_tail%'`).Scan(&n))
		return n
	}
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	var indexes int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT toInt64(count()) FROM system.data_skipping_indices
		WHERE database = currentDatabase() AND table = 'sessions' AND name = 'id_tail'`).Scan(&indexes))
	assert.Equal(t, 1, indexes)
	assert.Equal(t, 1, materializations())
}
