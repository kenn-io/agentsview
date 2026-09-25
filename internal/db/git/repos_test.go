package git

import (
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
	configureTestRepoIdentity(t, repo)
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

	got := DiscoverRepos(t.Context(), []string{sub, outside})
	want := []string{repoA}
	assert.Equal(t, canonAll(want), canonAll(got), "DiscoverRepos")
}

func TestDiscoverRepos_Dedup(t *testing.T) {
	skipIfNoGit(t)
	repoA := initBareRepo(t)
	sub1 := mkdirIn(t, repoA, "sub1")
	sub2 := mkdirIn(t, repoA, "sub2/deeper")

	got := DiscoverRepos(t.Context(), []string{sub1, sub2, repoA})
	require.Len(t, got, 1, "want exactly one entry (dedup)")
	assert.Equal(t, canonAll([]string{repoA}), canonAll(got),
		"DiscoverRepos")
}

func TestDiscoverRepos_EmptyInputReturnsEmptySlice(t *testing.T) {
	got := DiscoverRepos(t.Context(), nil)
	require.NotNil(t, got, "DiscoverRepos(nil)")
	assert.Empty(t, got, "DiscoverRepos(nil) should be empty slice")
	got = DiscoverRepos(t.Context(), []string{})
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
	gitRun(t, repo, nil, "commit", "--allow-empty", "-q", "-m", "seed")

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repo, nil,
		"worktree", "add", "-b", "feature", worktreeRoot,
	)

	got := DiscoverRepos(t.Context(), []string{worktreeRoot})
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

	got := DiscoverRepos(t.Context(), []string{missing})
	assert.Empty(t, got, "DiscoverRepos missing path")
}

// setOrigin points repo's `origin` remote at url.
func setOrigin(t *testing.T, repo, url string) {
	t.Helper()
	gitRun(t, repo, nil, "remote", "add", "origin", url)
}

// commitAt creates one empty commit in repo with a fixed author and commit
// timestamp, so "which checkout is freshest" is deterministic.
func commitAt(t *testing.T, repo, when, message string) {
	t.Helper()
	gitRun(t, repo, []string{
		"GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_DATE=" + when,
	}, "commit", "--allow-empty", "-q", "-m", message)
}

// TestDiscoverRepos_DedupByOrigin pins that one remote contributes one
// repository however many times it is checked out locally. Before this, the
// dedup key was the local toplevel path, so a mirror, a second clone, or a
// linked worktree each counted as its own repository and every commit and
// pull request the remote reports was added once per directory.
func TestDiscoverRepos_DedupByOrigin(t *testing.T) {
	skipIfNoGit(t)
	const origin = "https://github.com/example-org/example-repo.git"

	primary := initBareRepo(t)
	setOrigin(t, primary, origin)
	mirror := initBareRepo(t)
	setOrigin(t, mirror, origin)

	got := DiscoverRepos(t.Context(), []string{primary, mirror})
	require.Len(t, got, 1,
		"two checkouts of one remote must contribute one repository")
	assert.Equal(t, canonAll([]string{primary}), canonAll(got),
		"the first-seen checkout represents the remote")
}

// TestDiscoverRepos_DedupByOriginAcrossURLForms pins that the SSH and HTTPS
// spellings of one remote, with and without the `.git` suffix and a trailing
// slash, are one repository.
func TestDiscoverRepos_DedupByOriginAcrossURLForms(t *testing.T) {
	skipIfNoGit(t)
	forms := []string{
		"https://github.com/example-org/example-repo.git",
		"git@github.com:example-org/example-repo.git",
		"ssh://git@github.com/example-org/example-repo",
		"https://GitHub.com/example-org/example-repo/",
	}
	cwds := make([]string, 0, len(forms))
	for _, form := range forms {
		repo := initBareRepo(t)
		setOrigin(t, repo, form)
		cwds = append(cwds, repo)
	}

	got := DiscoverRepos(t.Context(), cwds)
	assert.Len(t, got, 1,
		"every spelling of one remote must collapse to one repository")
}

// TestDiscoverRepos_DedupByOriginKeepsFreshestCheckout pins which duplicate
// represents the remote. A stale second clone is missing the newest commits,
// so keeping it would undercount; the checkout whose HEAD commit is newest is
// the one that can answer for the remote.
func TestDiscoverRepos_DedupByOriginKeepsFreshestCheckout(t *testing.T) {
	skipIfNoGit(t)
	const origin = "https://github.com/example-org/example-repo.git"

	stale := initBareRepo(t)
	setOrigin(t, stale, origin)
	commitAt(t, stale, "2026-01-01T00:00:00+0000", "stale")

	fresh := initBareRepo(t)
	setOrigin(t, fresh, origin)
	commitAt(t, fresh, "2026-06-01T00:00:00+0000", "fresh")

	got := DiscoverRepos(t.Context(), []string{stale, fresh})
	require.Len(t, got, 1, "one remote, one repository")
	assert.Equal(t, canonAll([]string{fresh}), canonAll(got),
		"the checkout with the newest commit represents the remote")
}

// TestDiscoverRepos_DistinctOriginsBothKept pins that deduplication is by
// remote and not something coarser: two different remotes stay two
// repositories.
func TestDiscoverRepos_DistinctOriginsBothKept(t *testing.T) {
	skipIfNoGit(t)
	first := initBareRepo(t)
	setOrigin(t, first, "https://github.com/example-org/first.git")
	second := initBareRepo(t)
	setOrigin(t, second, "https://github.com/example-org/second.git")

	got := DiscoverRepos(t.Context(), []string{first, second})
	assert.Equal(t, canonAll([]string{first, second}), canonAll(got),
		"two remotes must stay two repositories")
}

// TestDiscoverRepos_NoRemoteFallsBackToPath pins that a repository with no
// remote is still its own repository. Collapsing every remote-less checkout
// into one entry would erase local-only work from the totals.
func TestDiscoverRepos_NoRemoteFallsBackToPath(t *testing.T) {
	skipIfNoGit(t)
	first := initBareRepo(t)
	second := initBareRepo(t)

	got := DiscoverRepos(t.Context(), []string{first, second})
	assert.Equal(t, canonAll([]string{first, second}), canonAll(got),
		"remote-less repositories must not collapse into each other")
}

// TestDiscoverRepos_LinkedWorktreeSharesItsRepositoryOrigin pins the
// worktree case named in the report: a linked worktree and its main checkout
// share one remote, so they count once.
func TestDiscoverRepos_LinkedWorktreeSharesItsRepositoryOrigin(t *testing.T) {
	skipIfNoGit(t)
	repo := initBareRepo(t)
	setOrigin(t, repo, "https://github.com/example-org/example-repo.git")
	commitAt(t, repo, "2026-01-01T00:00:00+0000", "seed")

	worktreeRoot := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repo, nil, "worktree", "add", "-b", "feature", worktreeRoot)

	got := DiscoverRepos(t.Context(), []string{repo, worktreeRoot})
	assert.Len(t, got, 1,
		"a linked worktree and its main checkout share one remote")
}

// TestNormalizeRemoteURL pins the identity key the dedup relies on: the
// spellings of one remote reduce to one string, and anything that is not a
// host-plus-path remote reduces to "" so the caller falls back to the local
// path instead of merging unrelated repositories.
func TestNormalizeRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "blank", raw: "   ", want: ""},
		{
			name: "https with .git",
			raw:  "https://github.com/example-org/example-repo.git",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "https without .git",
			raw:  "https://github.com/example-org/example-repo",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "https with trailing slash",
			raw:  "https://github.com/example-org/example-repo/",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "host case is normalised",
			raw:  "https://GitHub.COM/example-org/example-repo",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "https with credentials",
			raw:  "https://token@github.com/example-org/example-repo.git",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "scp shorthand",
			raw:  "git@github.com:example-org/example-repo.git",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "ssh scheme",
			raw:  "ssh://git@github.com/example-org/example-repo.git",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "ssh scheme with port",
			raw:  "ssh://git@github.com:22/example-org/example-repo.git",
			want: "github.com:22/example-org/example-repo",
		},
		{
			name: "nested path is preserved",
			raw:  "https://gitlab.example.test/group/subgroup/example-repo.git",
			want: "gitlab.example.test/group/subgroup/example-repo",
		},
		{
			name: "path case is preserved",
			raw:  "https://github.com/Example-Org/Example-Repo.git",
			want: "github.com/Example-Org/Example-Repo",
		},
		{
			name: "local filesystem remote keeps its path",
			raw:  "/srv/mirrors/example-repo.git",
			want: "/srv/mirrors/example-repo",
		},
		{
			name: "file scheme reduces to the same path",
			raw:  "file:///srv/mirrors/example-repo.git",
			want: "/srv/mirrors/example-repo",
		},
		{name: "host only", raw: "https://github.com", want: ""},
		{name: "host only with slash", raw: "https://github.com/", want: ""},
		{name: "no separator", raw: "example-repo", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeRemoteURL(tt.raw))
		})
	}
}
