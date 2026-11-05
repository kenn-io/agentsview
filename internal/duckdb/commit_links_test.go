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

func TestDuckDBCommitLinkParity(t *testing.T) {
	{
		local := newLocalDB(t)
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "codex:abc", Agent: "codex", Project: "project-a", Machine: "m"}))
		const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		_, err := local.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{
			SHA: sha, Sessions: []string{"claude:codex:abc", "codex:abc"}, CommittedAt: "2026-01-01T00:00:00Z",
		}})
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "commits.duckdb")
		_, err = Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
		store := openPushedStore(t, path)
		links, err := store.SessionCommitLinks(t.Context(), "codex:abc")
		require.NoError(t, err)
		require.Len(t, links.Commits, 1)
		assert.Equal(t, sha, links.Commits[0].SHA)
		assert.Equal(t, "codex:abc", links.Commits[0].SessionID)
		assert.Equal(t, "trailer", links.Commits[0].Source)
		require.NoError(t, store.Close())
	}

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
}

func TestDuckDBTrailerStoredAfterPurgeScope(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts storage.MirrorPushOptions
	}{
		{name: "unfiltered"},
		{name: "include project", opts: storage.MirrorPushOptions{Projects: []string{"project-a"}}},
		{name: "exclude project", opts: storage.MirrorPushOptions{ExcludeProjects: []string{"project-a"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local := newLocalDB(t)
			require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "editor", Agent: "claude", Project: "project-a", Machine: "m"}))
			require.NoError(t, local.DeleteSession(t.Context(), "editor"))
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			_, err := local.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Sessions: []string{"claude:editor"}, CommittedAt: "2026-01-01T00:00:00Z"}})
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "commits.duckdb")
			_, err = Push(t.Context(), path, local, "m", tt.opts, false, nil)
			require.NoError(t, err)
			store := openPushedStore(t, path)
			defer func() { require.NoError(t, store.Close()) }()
			links, err := store.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, links.Commits, 1)
			assert.Empty(t, links.Commits[0].Links)
		})
	}
}

func TestDuckDBRetainedCommitLinkScope(t *testing.T) {
	for _, force := range []bool{true, false} {
		name := "full push with deletion before publication"
		if !force {
			name = "incremental push with deletion after publication"
		}
		t.Run(name, func(t *testing.T) {
			local := newLocalDB(t)
			path := filepath.Join(t.TempDir(), "commits.duckdb")
			opts := storage.MirrorPushOptions{}
			if !force {
				dbtest.SeedCommitLinks(t, local)
				opts.Projects = []string{"commit-project"}
			}
			_, err := Push(t.Context(), path, local, "m", opts, false, nil)
			require.NoError(t, err)
			if force {
				dbtest.SeedCommitLinks(t, local)
			}
			store := openPushedStore(t, path)
			defer func() { require.NoError(t, store.Close()) }()
			push := func(projects, exclude []string) {
				require.NoError(t, store.Close())
				_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{Projects: projects, ExcludeProjects: exclude}, force, nil)
				require.NoError(t, err)
				store = openPushedStore(t, path)
			}
			if !force {
				require.NoError(t, local.DeleteSession(t.Context(), "commit-delete"))
				push([]string{"commit-project"}, nil)
				dbtest.AssertCommitLinkParity(t, local, store)
				for _, project := range []string{"other", "commit-project"} {
					push([]string{project}, nil)
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
				return
			}
			dbtest.AssertRetainedCommitLinkScope(t, local, func() db.CommitLinkReader { return store }, push)
		})
	}
}
