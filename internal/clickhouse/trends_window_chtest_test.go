//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseTrendsTermsDateWindowEdges(t *testing.T) {
	t.Parallel()
	local, target := seedFixture(t)
	dbtest.SeedTrendsWindow(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.openActivityReportDisk(target))
	dbtest.AssertTrendsWindow(t, store)
	dbtest.AssertTrendsPartsReuse(t, func() db.Store {
		opened, err := NewStore(t.Context(), target)
		require.NoError(t, err)
		require.NoError(t, opened.openActivityReportDisk(target))
		return opened
	}, func() {
		dbtest.RewriteTrendsWindow(t, local)
		_, err := syncer.Push(t.Context(), false, nil)
		require.NoError(t, err)
	})
}
