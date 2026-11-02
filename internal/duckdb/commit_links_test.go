//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBCommitLinkParity(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedCommitLinks(t, local)
	path := filepath.Join(t.TempDir(), "commits.duckdb")
	opts := storage.MirrorPushOptions{Projects: []string{"commit-project"}}
	_, err := Push(t.Context(), path, local, "m", opts, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	dbtest.AssertCommitLinkParity(t, local, store)
	require.NoError(t, store.Close())
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET content='different revision' WHERE session_id='commit-live' AND ordinal=0`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store = openPushedStore(t, path)
	links, err := store.SessionCommitLinks(t.Context(), "commit-live")
	require.NoError(t, err)
	require.NotNil(t, links.Commits[0].Turn)
	assert.Equal(t, "Save the change", links.Commits[0].Turn.Text)
	require.NoError(t, store.Close())
	require.NoError(t, local.DeleteSession(t.Context(), "commit-delete"))
	// Incremental deletion must keep project evidence for the retained row.
	_, err = Push(t.Context(), path, local, "m", opts, false, nil)
	require.NoError(t, err)
	store = openPushedStore(t, path)
	dbtest.AssertCommitLinkParity(t, local, store)
	require.NoError(t, store.Close())
	// A rebuild and scope narrowing must keep and then remove the retained link.
	_, err = Push(t.Context(), path, local, "m", opts, true, nil)
	require.NoError(t, err)
	store = openPushedStore(t, path)
	dbtest.AssertCommitLinkParity(t, local, store)
	require.NoError(t, store.Close())
	opts.Projects = []string{"other"}
	_, err = Push(t.Context(), path, local, "m", opts, false, nil)
	require.NoError(t, err)
	store = openPushedStore(t, path)
	defer store.Close()
	links, err = store.SessionCommitLinks(t.Context(), "commit-delete")
	require.NoError(t, err)
	assert.Empty(t, links.Commits)
}
