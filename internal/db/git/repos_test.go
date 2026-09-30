package git

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
	assert.Equal(t, canonAll(want), canonAll(slices.Concat(got...)), "DiscoverRepos")
}

func TestDiscoverRepos_Dedup(t *testing.T) {
	skipIfNoGit(t)
	repoA := initBareRepo(t)
	sub1 := mkdirIn(t, repoA, "sub1")
	sub2 := mkdirIn(t, repoA, "sub2/deeper")

	got := DiscoverRepos(t.Context(), []string{sub1, sub2, repoA})
	require.Len(t, got, 1, "want exactly one entry (dedup)")
	assert.Equal(t, canonAll([]string{repoA}), canonAll(slices.Concat(got...)),
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
		canonAll(slices.Concat(got...)),
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
// timestamp.
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
	assert.Equal(t, canonAll([]string{primary, mirror}), canonAll(slices.Concat(got...)),
		"both checkouts remain available for commit aggregation")
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
		"https://github.com:443/example-org/example-repo.git",
		"ssh://git@github.com:22/example-org/example-repo.git",
		"git+ssh://git@github.com:22/example-org/example-repo.git",
		"ssh+git://git@github.com:22/example-org/example-repo.git",
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

func TestDiscoverRepos_CustomSchemesStayDistinct(t *testing.T) {
	skipIfNoGit(t)
	var roots []string
	for _, origin := range []string{
		"https://example.com/team/repo.git",
		"exampleproto://example.com/team/repo.git",
		"exampleproto://example.com/team/repo.git",
		"otherproto://example.com/team/repo.git",
	} {
		root := initRepo(t)
		setOrigin(t, root, origin)
		roots = append(roots, root)
	}
	assert.Equal(t, [][]string{canonAll(roots[:1]), canonAll(roots[1:3]), canonAll(roots[3:])}, DiscoverRepos(t.Context(), roots))
}

// Keep both histories even when one checkout has a newer HEAD.
func TestDiscoverRepos_DedupByOriginKeepsAllCheckouts(t *testing.T) {
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
	assert.Equal(t, canonAll([]string{stale, fresh}), canonAll(slices.Concat(got...)),
		"both checkout histories contribute")
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
	require.Len(t, got, 2)
	assert.Equal(t, canonAll([]string{first, second}), canonAll(slices.Concat(got...)),
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
	require.Len(t, got, 2)
	assert.Equal(t, canonAll([]string{first, second}), canonAll(slices.Concat(got...)),
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
			name: "custom scheme URL remains intact",
			raw:  "exampleproto://user@Example.com/team/repo.git?ref=a@b#section",
			want: "exampleproto://user@Example.com/team/repo.git?ref=a@b#section",
		},
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
			name: "ssh scheme with default port",
			raw:  "ssh://git@github.com:22/example-org/example-repo.git",
			want: "github.com/example-org/example-repo",
		},
		{
			name: "https default port",
			raw:  "https://example.com:443/team/repo.git",
			want: "example.com/team/repo",
		},
		{
			name: "http default port",
			raw:  "http://example.com:80/team/repo.git",
			want: "example.com/team/repo",
		},
		{
			name: "git default port",
			raw:  "git://example.com:9418/team/repo.git",
			want: "example.com/team/repo",
		},
		{
			name: "nondefault port is preserved",
			raw:  "https://example.com:8443/team/repo.git",
			want: "example.com:8443/team/repo",
		},
		{
			name: "IPv6 default port",
			raw:  "https://[2001:db8::1]:443/team/repo.git",
			want: "[2001:db8::1]/team/repo",
		},
		{
			name: "path query and fragment are preserved",
			raw:  "https://git@example.com:443/team/repo%2Fname?ref=a@b#section",
			want: "example.com/team/repo%2Fname?ref=a@b#section",
		},
		{
			name: "git suffix in query and fragment is preserved",
			raw:  "https://example.com:443/team/repo.git?ref=release.git#docs.git",
			want: "example.com/team/repo?ref=release.git#docs.git",
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
		{name: "host only", raw: "https://github.com", want: ""},
		{name: "host only with slash", raw: "https://github.com/", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeRemoteURL(tt.raw, t.TempDir()))
		})
	}
}

func TestDiscoverRepos_LocalRemotePaths(t *testing.T) {
	skipIfNoGit(t)
	for _, tc := range []struct {
		name    string
		origins func(string, string) (string, string)
		want    int
	}{
		{"relative paths in different parents", func(a, b string) (string, string) { return "../mirror.git", "../mirror.git" }, 2},
		{"absolute suffixes stay distinct", func(a, b string) (string, string) { return filepath.Join(a, "mirror"), filepath.Join(a, "mirror.git") }, 2},
		{"file URL host and absolute same path", func(a, b string) (string, string) {
			path := filepath.Join(a, "mirror.git")
			u := url.URL{Scheme: "file", Host: "mirror-host", Path: "/" + strings.TrimPrefix(filepath.ToSlash(path), "/")}
			return u.String(), path
		}, 1},
		{"file URL and absolute same path", func(a, b string) (string, string) {
			path := filepath.Join(a, "mirror.git")
			u := url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(path), "/")}
			return u.String(), path
		}, 1},
		{"relative and absolute same path", func(a, b string) (string, string) {
			return "../mirror.git", filepath.Join(filepath.Dir(a), "mirror.git")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mkdirIn(t, t.TempDir(), "checkout")
			b := mkdirIn(t, t.TempDir(), "checkout")
			for _, root := range []string{a, b} {
				gitRun(t, root, nil, "init", "-q", "-b", "main")
			}
			first, second := tc.origins(a, b)
			for _, remote := range []struct{ root, origin string }{{a, first}, {b, second}} {
				path := remote.origin
				if strings.HasPrefix(path, "file://") {
					path = filepath.Join(a, "mirror.git")
				} else if !filepath.IsAbs(path) {
					path = filepath.Join(remote.root, path)
				}
				require.NoError(t, os.MkdirAll(path, 0o755))
				gitRun(t, path, nil, "init", "--bare", "-q")
				setOrigin(t, remote.root, remote.origin)
			}
			assert.Len(t, DiscoverRepos(t.Context(), []string{a, b}), tc.want)
		})
	}
}

func TestDiscoverRepos_UsesGlobalOrigin(t *testing.T) {
	skipIfNoGit(t)
	require.NoError(t, os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte("[remote \"origin\"]\n url = https://example.com/team/repo.git\n"), 0o600))
	a, b := initRepo(t), initRepo(t)
	assert.Len(t, DiscoverRepos(t.Context(), []string{a, b}), 1)
}
