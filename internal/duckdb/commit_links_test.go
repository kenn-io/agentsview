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

func TestDuckDBIncrementalRetainedCommitLinkScope(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedCommitLinks(t, local)
	path := filepath.Join(t.TempDir(), "commits.duckdb")
	opts := storage.MirrorPushOptions{Projects: []string{"commit-project"}}
	_, err := Push(t.Context(), path, local, "m", opts, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	defer func() { require.NoError(t, store.Close()) }()
	dbtest.AssertCommitLinkParity(t, local, store)

	push := func(project string) {
		require.NoError(t, store.Close())
		_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{Projects: []string{project}}, false, nil)
		require.NoError(t, err)
		store = openPushedStore(t, path)
	}
	require.NoError(t, local.DeleteSession(t.Context(), "commit-delete"))
	push("commit-project")
	dbtest.AssertCommitLinkParity(t, local, store)
	for _, project := range []string{"other", "commit-project"} {
		push(project)
		dbtest.AssertRetainedCommitLink(t, store, project == "commit-project")
	}
}

func TestDuckDBRetainedCommitLinkScope(t *testing.T) {
	local := newLocalDB(t)
	path := filepath.Join(t.TempDir(), "commits.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	dbtest.SeedCommitLinks(t, local)
	store := openPushedStore(t, path)
	defer func() { require.NoError(t, store.Close()) }()
	push := func(projects, exclude []string) {
		require.NoError(t, store.Close())
		_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{Projects: projects, ExcludeProjects: exclude}, true, nil)
		require.NoError(t, err)
		store = openPushedStore(t, path)
	}
	dbtest.AssertRetainedCommitLinkScope(t, local, func() db.CommitLinkReader { return store }, push)
}
