package git

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initBareRepo runs `git init -b main` at root and configures a
// deterministic identity so commit creation never prompts. Returns the
// repo path.
func initBareRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "-b", "main")
	gitRun(t, repo, nil, "config", "user.email", "test@example.com")
	gitRun(t, repo, nil, "config", "user.name", "Test User")
	gitRun(t, repo, nil, "config", "commit.gpgsign", "false")
	return repo
}

// mkdirIn creates rel under root and returns the absolute path.
func mkdirIn(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(p, 0o755), "mkdir %s", p)
	return p
}

// canonAll resolves each path through filepath.EvalSymlinks (falling back
// to the original on error) and returns a sorted copy. Needed because
// `git rev-parse --show-toplevel` returns canonical paths, which on macOS
// expand /var to /private/var.
func canonAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			out[i] = r
		} else {
			out[i] = p
		}
	}
	sort.Strings(out)
	return out
}

func TestDiscoverRepos_FindsRootAndFiltersMissing(t *testing.T) {
	skipIfNoGit(t)
	repoA := initBareRepo(t)
	sub := mkdirIn(t, repoA, "subdir")
	outside := t.TempDir()

	got := DiscoverRepos(context.Background(), []string{sub, outside})
	want := []string{repoA}
	assert.Equal(t, canonAll(want), canonAll(got), "DiscoverRepos")
}

func TestDiscoverRepos_Dedup(t *testing.T) {
	skipIfNoGit(t)
	repoA := initBareRepo(t)
	sub1 := mkdirIn(t, repoA, "sub1")
	sub2 := mkdirIn(t, repoA, "sub2/deeper")

	got := DiscoverRepos(context.Background(), []string{sub1, sub2, repoA})
	require.Len(t, got, 1, "want exactly one entry (dedup)")
	assert.Equal(t, canonAll([]string{repoA}), canonAll(got),
		"DiscoverRepos")
}

func TestDiscoverRepos_EmptyInputReturnsEmptySlice(t *testing.T) {
	got := DiscoverRepos(context.Background(), nil)
	require.NotNil(t, got, "DiscoverRepos(nil)")
	assert.Empty(t, got, "DiscoverRepos(nil) should be empty slice")
	got = DiscoverRepos(context.Background(), []string{})
	require.NotNil(t, got, "DiscoverRepos([])")
	assert.Empty(t, got, "DiscoverRepos([]) should be empty slice")
}

// TestDiscoverRepos_LinkedWorktreeResolves covers the regression flagged
// by code review: linked worktrees use a `.git` FILE (not directory)
// that points at the parent gitdir. `git rev-parse --show-toplevel`
// resolves these, so worktree cwds must contribute a repo root rather
// than being silently dropped.
func TestDiscoverRepos_LinkedWorktreeResolves(t *testing.T) {
	skipIfNoGit(t)
	repo := initBareRepo(t)
	// `git worktree add` requires at least one commit in the source
	// repo, so seed one before linking.
	writeFile(t, repo, "seed.txt", []byte("seed\n"))
	commitAs(t, repo, "test@example.com", "Test User", "seed")

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repo, nil,
		"worktree", "add", "-b", "feature", worktreeRoot,
	)

	got := DiscoverRepos(context.Background(), []string{worktreeRoot})
	require.Len(t, got, 1, "want one worktree root")
	assert.Equal(t,
		canonAll([]string{worktreeRoot}),
		canonAll(got),
		"DiscoverRepos (worktree path)")
}

// TestDiscoverRepos_MissingCwdSkipped confirms that a cwd whose path is
// completely outside any git repo (and which does not exist on disk)
// produces no false-positive root.
func TestDiscoverRepos_MissingCwdSkipped(t *testing.T) {
	skipIfNoGit(t)
	missing := filepath.Join(t.TempDir(), "no", "such", "path")

	got := DiscoverRepos(context.Background(), []string{missing})
	assert.Empty(t, got, "DiscoverRepos missing path")
}
