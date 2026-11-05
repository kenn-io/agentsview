//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBTrailerStoredAfterPurgeLookupParity(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedSession(t, local, "editor", "project-a")
	require.NoError(t, local.DeleteSession(t.Context(), "editor"))
	sha := strings.Repeat("a", 40)
	_, err := local.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"claude:editor"}, CommittedAt: "2026-01-01T00:01:00Z"}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "commits.duckdb")
	_, err = Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	defer func() { require.NoError(t, store.Close()) }()
	for _, reader := range []db.CommitLinkReader{local, store} {
		links, err := reader.SessionCommitLinks(t.Context(), "editor")
		require.NoError(t, err)
		assert.Empty(t, links.Commits)
		matches, err := reader.CommitSessionLinks(t.Context(), []string{sha})
		require.NoError(t, err)
		require.Len(t, matches.Commits, 1)
		assert.Empty(t, matches.Commits[0].Links)
	}
}

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
	dbtest.AssertTrailerIncrementalPush(t, local, func() db.CommitLinkReader { return store }, func() { push("commit-project") })
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
