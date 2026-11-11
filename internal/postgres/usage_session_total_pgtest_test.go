//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestSessionUsageTotalPushesApartFromUsage(t *testing.T) {
	const schema = "agentsview_session_total_test"
	ctx := t.Context()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	dbtest.SeedSessionUsageTotal(t, local)
	syncer, err := New(
		ctx, pgURL, schema, local, "session-total-machine", true, storage.PusherOptions{},
	)
	require.NoError(t, err, "create PostgreSQL sync")
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push session total")

	remote, err := NewStore(ctx, pgURL, schema, true)
	require.NoError(t, err, "open PostgreSQL store")
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	dbtest.AssertSessionUsageTotal(t, remote)

	// Removing the archive's total must remove it from PostgreSQL on the
	// next push, even though no message changed.
	require.NoError(t, local.ReplaceSessionUsageTotal(ctx, dbtest.SessionTotalSessionID, nil))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push removed session total")
	got, err := remote.GetSessionUsage(ctx, dbtest.SessionTotalSessionID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.SessionTotal, "the push removes a total the archive no longer has")
	assert.Equal(t, int64(2_000), got.Cost.Microdollars)
}
