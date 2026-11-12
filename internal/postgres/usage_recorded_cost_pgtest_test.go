//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRecordedCostPricesOnlyUnpricedModels(t *testing.T) {
	const schema = "agentsview_recorded_cost_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	dbtest.SeedRecordedCostSessions(t, local)
	syncer, err := New(
		t.Context(), pgURL, schema, local, "recorded-cost-machine", true, storage.PusherOptions{},
	)
	require.NoError(t, err, "create PostgreSQL sync")
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err, "push recorded-cost fixture")

	remote, err := NewStore(t.Context(), pgURL, schema, true)
	require.NoError(t, err, "open PostgreSQL store")
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	dbtest.AssertRecordedCostUsage(t, remote)
}
