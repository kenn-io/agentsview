//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBRawSuffixMatchesArchive(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedRawSuffix(t, local)
	dbtest.TrashRawSuffix(t, local)
	path := filepath.Join(t.TempDir(), "raw-suffix.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertRawSuffix(t, store)
}
