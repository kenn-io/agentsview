//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSearchKeptQuackReadsFresh(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session", "needle first")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-search"
	server := openQuackMirrorServer(t, ctx, path, uri, token)
	store, err := NewQuackStore(ctx, uri, token, false, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	filter := db.SearchFilter{Query: "needle"}
	for range 2 {
		page, err := store.Search(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Contains(t, page.Results[0].Snippet, "first")
	}
	_, err = server.ExecContext(ctx, `UPDATE messages SET content='needle second' WHERE session_id='session'`)
	require.NoError(t, err)
	page, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "second")
}
