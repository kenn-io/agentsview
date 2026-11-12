//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRecordedCostPricesOnlyUnpricedModels(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	dbtest.SeedRecordedCostSessions(t, local)
	dbtest.AssertRecordedCostUsage(t, local)

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "push to DuckDB")
	dbtest.AssertRecordedCostUsage(t, NewStoreFromDB(syncer.DB()))
}
