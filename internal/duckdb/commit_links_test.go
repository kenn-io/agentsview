//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBTrailerDuplicateOwnerIdentities(t *testing.T) {
	local := newLocalDB(t)
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "codex:abc", Agent: "codex", Project: "project-a", Machine: "m"}))
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, local.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
		SHA: sha, Sessions: []string{"claude:codex:abc", "codex:abc"}, CommittedAt: "2026-01-01T00:00:00Z",
	}}))
	path := filepath.Join(t.TempDir(), "commits.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	defer func() { require.NoError(t, store.Close()) }()
	links, err := store.SessionCommitLinks(t.Context(), "codex:abc")
	require.NoError(t, err)
	require.Len(t, links.Commits, 1)
	assert.Equal(t, sha, links.Commits[0].SHA)
	assert.Equal(t, "codex:abc", links.Commits[0].SessionID)
	assert.Equal(t, "trailer", links.Commits[0].Source)
}

func TestDuckDBCommitLinkParity(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedCommitLinks(t, local)
	path := filepath.Join(t.TempDir(), "commits.duckdb")
	opts := storage.MirrorPushOptions{Projects: []string{"commit-project"}}
	_, err := Push(t.Context(), path, local, "m", opts, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	dbtest.AssertCommitLinkParity(t, local, store)
	dbtest.AssertTrailerIncrementalPush(t, local, func() db.CommitLinkReader { return store }, func() {
		require.NoError(t, store.Close())
		_, err := Push(t.Context(), path, local, "m", opts, false, nil)
		require.NoError(t, err)
		store = openPushedStore(t, path)
	})
	require.NoError(t, store.Close())
	require.NoError(t, local.DeleteSession(t.Context(), "commit-delete"))
	// Incremental deletion must keep project evidence for the retained row.
	_, err = Push(t.Context(), path, local, "m", opts, false, nil)
	require.NoError(t, err)
	store = openPushedStore(t, path)
	dbtest.AssertCommitLinkParity(t, local, store)
	defer func() { require.NoError(t, store.Close()) }()
	// The shared scope helper forces publication; keep incremental scope changes here.
	for _, project := range []string{"other", "commit-project"} {
		require.NoError(t, store.Close())
		opts.Projects = []string{project}
		_, err = Push(t.Context(), path, local, "m", opts, false, nil)
		require.NoError(t, err)
		store = openPushedStore(t, path)
		links, err := store.SessionCommitLinks(t.Context(), "commit-delete")
		require.NoError(t, err)
		if project == "other" {
			assert.Empty(t, links.Commits)
		} else {
			require.Len(t, links.Commits, 1)
			assert.Equal(t, "abc1234567890123456789012345678901234567", links.Commits[0].SHA)
			assert.True(t, links.Commits[0].Missing)
			assert.Nil(t, links.Commits[0].Turn)
		}
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
	dbtest.AssertRetainedCommitLinkScope(t, local, func() db.CommitLinkReader { return store }, func(projects, exclude []string) {
		require.NoError(t, store.Close())
		_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{Projects: projects, ExcludeProjects: exclude}, true, nil)
		require.NoError(t, err)
		store = openPushedStore(t, path)
	})
}
