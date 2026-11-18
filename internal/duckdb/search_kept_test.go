//go:build duckdb && !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSearchKeptMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	buildMirrorFixture(t, path, "session-a", "answer")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	loads := 0
	store.searchReads.Size = func(db.SearchPage) int64 { loads++; return 1 }
	filter := db.SearchFilter{Query: "answer"}
	first, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, first.Results, 1)
	require.Equal(t, "session-a", first.Results[0].SessionID)
	repeated, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	require.Equal(t, 1, loads)
	next := filepath.Join(dir, "next.duckdb")
	buildMirrorFixture(t, next, "session-b", "answer")
	require.NoError(t, swapMirrorFile(next, path))
	changed, err := store.Search(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, changed.Results, 1)
	require.Equal(t, "session-b", changed.Results[0].SessionID)
	require.Equal(t, 2, loads)
}
