//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestSessionUsageTotalMirrorsApartFromUsage(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	dbtest.SeedSessionUsageTotal(t, local)

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "push to DuckDB")
	store := NewStoreFromDB(syncer.DB())
	dbtest.AssertSessionUsageTotal(t, store)

	// Removing the total locally changes the session's fingerprint, so the
	// next push clears the mirror's row.
	require.NoError(t, local.ReplaceSessionUsageTotal(ctx, dbtest.SessionTotalSessionID, nil))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "second push to DuckDB")
	got, err := store.GetSessionUsage(ctx, dbtest.SessionTotalSessionID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.SessionTotal)
}
