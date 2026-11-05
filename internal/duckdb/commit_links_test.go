//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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
