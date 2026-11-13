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

func TestDuckTrendsTermsDateWindowEdges(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedTrendsWindow(t, local)
	path := filepath.Join(t.TempDir(), "trends.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	require.NoError(t, store.OpenActivityReportDisk(t.Context()))
	dbtest.AssertTrendsWindow(t, store)
	require.NoError(t, store.Close())
	dbtest.AssertTrendsPartsReuse(t, func() db.Store {
		opened := openPushedStore(t, path)
		require.NoError(t, opened.OpenActivityReportDisk(t.Context()))
		return opened
	}, func() {
		dbtest.RewriteTrendsWindow(t, local)
		_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
	})
}
