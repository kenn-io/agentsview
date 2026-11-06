//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestDuckDBFrustrationMemoFollowsMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	buildMirrorFixture(t, path, "session", "this is broken")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()

	resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	count, ok := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "session", "")
	require.True(t, ok)
	assert.Equal(t, 1, count)
	store.frustrationMemo.Store(store.frustrationMemo.Generation(), "session", "", 7)
	resp, err = store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 7, resp.QualityHealth.Totals.FrustrationMarkerCount)

	nextPath := filepath.Join(dir, "next.duckdb")
	buildMirrorFixture(t, nextPath, "session", "Please explain this function.")
	require.NoError(t, os.Rename(nextPath, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	resp, err = store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Zero(t, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestDuckDBFrustrationMemoSkipsUnwatchedStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session", "this is broken")
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	store := NewStoreFromDB(conn)
	defer store.Close()
	resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	_, ok := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "session", "")
	assert.False(t, ok)
}
