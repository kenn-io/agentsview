//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckNativeSearchSources(t *testing.T) {
	archive := newLocalDB(t)
	dbtest.SeedNativeSearch(t, archive)
	path := filepath.Join(t.TempDir(), "mirror.db")
	_, err := Push(t.Context(), path, archive, "test", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.CheckNativeSearch(t, store, false)
}
