package git

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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

func linkRepoDirectory(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		require.NoError(t, err, "%s", out)
	} else {
		require.NoError(t, os.Symlink(target, link))
	}
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

func TestFindRepoRoot_BareWorktreeFallback(t *testing.T) {
	skipIfNoGit(t)
	bare, worktree := t.TempDir(), t.TempDir()
	gitRun(t, bare, nil, "init", "--bare", "-q")
	gitRun(t, bare, nil, "config", "core.bare", "false")
	gitRun(t, bare, nil, "config", "core.worktree", worktree)
	assert.Equal(t, canonAll([]string{worktree})[0], findRepoRoot(t.Context(), bare))
}

func TestFindRepoRoot_DirectoryAlias(t *testing.T) {
	skipIfNoGit(t)
	repo := initBareRepo(t)
	sub := mkdirIn(t, repo, "sub/nested")
	alias := filepath.Join(t.TempDir(), "alias")
	linkRepoDirectory(t, alias, filepath.Dir(sub))
	want := canonAll([]string{repo})[0]
	for _, cwd := range []string{alias, filepath.Join(alias, "nested")} {
		assert.Equal(t, want, findRepoRoot(t.Context(), cwd))
	}
	t.Setenv("PATH", t.TempDir())
	for _, cwd := range []string{alias, filepath.Join(alias, "nested")} {
		assert.Equal(t, want, findRepoRoot(t.Context(), cwd))
	}
}

func TestFindRepoRoot_RepositoryChanges(t *testing.T) {
	skipIfNoGit(t)
	repo := initBareRepo(t)
	path := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	assert.Empty(t, findRepoRoot(t.Context(), repo))
	t.Setenv("PATH", path)
	assert.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), repo))
	outside := t.TempDir()
	ctx := &pausedRepoFill{Context: t.Context(), started: make(chan struct{}), resume: make(chan struct{})}
	close(ctx.resume)
	assert.Empty(t, findRepoRoot(ctx, outside))
	assert.Empty(t, findRepoRoot(ctx, outside))
	assert.Zero(t, ctx.attempts.Load(), "ordinary non-repositories need no Git lookup")
	gitRun(t, outside, nil, "init", "-q")
	assert.Equal(t, canonAll([]string{outside})[0], findRepoRoot(t.Context(), outside))
	sub := mkdirIn(t, repo, "sub")
	assert.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), sub))
	gitRun(t, repo, nil, "commit", "--allow-empty", "-q", "-m", "seed")
	gitRun(t, repo, nil, "status", "--porcelain")
	t.Setenv("PATH", t.TempDir())
	assert.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), sub))
	assert.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), mkdirIn(t, repo, "unseen")))
	t.Setenv("PATH", path)
	t.Run("configured root groups one history", func(t *testing.T) {
		configured := mkdirIn(t, repo, "configured")
		gitRun(t, repo, nil, "config", "core.worktree", configured)
		groups := DiscoverRepos(t.Context(), []string{sub, configured})
		commits := 0
		for _, roots := range groups {
			result, err := AggregateLog(t.Context(), roots[0], "test@example.com", "1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z")
			require.NoError(t, err)
			commits += result.Commits
		}
		gitRun(t, repo, nil, "config", "--unset", "core.worktree")
		assert.Len(t, groups, 1)
		assert.Equal(t, 1, commits)
	})
	t.Run("equal metadata config edit", func(t *testing.T) {
		configured := mkdirIn(t, repo, ".git/aa")
		gitRun(t, repo, nil, "config", "core.worktree", "..")
		require.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), sub))
		config := filepath.Join(repo, ".git", "config")
		info, err := os.Stat(config)
		require.NoError(t, err)
		contents, err := os.ReadFile(config)
		require.NoError(t, err)
		edited := strings.Replace(string(contents), "worktree = ..", "worktree = aa", 1)
		require.NotEqual(t, string(contents), edited)
		require.NoError(t, os.WriteFile(config, []byte(edited), 0o600))
		require.NoError(t, os.Chtimes(config, info.ModTime(), info.ModTime()))
		got := findRepoRoot(t.Context(), sub)
		gitRun(t, repo, nil, "config", "--unset", "core.worktree")
		assert.Equal(t, canonAll([]string{configured})[0], got)
	})
	gitRun(t, sub, nil, "init", "-q")
	assert.Equal(t, canonAll([]string{sub})[0], findRepoRoot(t.Context(), sub))
	require.NoError(t, os.RemoveAll(filepath.Join(sub, ".git")))
	assert.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), sub))
	require.NoError(t, os.Rename(filepath.Join(repo, ".git"), filepath.Join(t.TempDir(), "old-git")))
	gitRun(t, repo, nil, "init", "-q")
	t.Setenv("PATH", t.TempDir())
	assert.Empty(t, findRepoRoot(t.Context(), sub), "replaced marker must resolve again")
	t.Setenv("PATH", path)
	configureTestRepoIdentity(t, repo)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-q", "-m", "seed")
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repo, nil, "worktree", "add", "-b", "feature", worktree)
	assert.Equal(t, canonAll([]string{worktree})[0], findRepoRoot(t.Context(), worktree))
	configured := mkdirIn(t, repo, "configured")
	gitRun(t, repo, nil, "config", "extensions.worktreeConfig", "true")
	gitRun(t, repo, nil, "config", "core.worktree", configured)
	assert.Equal(t, canonAll([]string{configured})[0], findRepoRoot(t.Context(), worktree))
	local := mkdirIn(t, worktree, "configured")
	gitRun(t, worktree, nil, "config", "--worktree", "core.worktree", local)
	assert.Equal(t, canonAll([]string{local})[0], findRepoRoot(t.Context(), worktree))
	gitRun(t, worktree, nil, "config", "--worktree", "--unset", "core.worktree")
	gitRun(t, repo, nil, "config", "--unset", "core.worktree")
	assert.Equal(t, canonAll([]string{worktree})[0], findRepoRoot(t.Context(), worktree))
	t.Setenv("PATH", t.TempDir())
	assert.Equal(t, canonAll([]string{worktree})[0], findRepoRoot(t.Context(), worktree))
	t.Setenv("PATH", path)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: missing\n"), 0o600))
	assert.Empty(t, findRepoRoot(t.Context(), worktree))
}

func TestFindRepoRoot_PendingFill(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		waiters                     int
		cancelCreator, cancelWaiter bool
	}{
		{"shared success", 8, false, false},
		{"failed creator", 1, true, false},
		{"cancelled waiter", 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skipIfNoGit(t)
			repo := initBareRepo(t)
			sub := mkdirIn(t, repo, "sibling")
			synctest.Test(t, func(t *testing.T) {
				base, cancelCreator := context.WithCancel(t.Context())
				waiterCtx, cancelWaiter := context.WithCancel(t.Context())
				defer cancelCreator()
				defer cancelWaiter()
				ctx := &pausedRepoFill{Context: base, started: make(chan struct{}), resume: make(chan struct{})}
				creator, waiter := make(chan string, 1), make(chan string, tc.waiters)
				go func() { creator <- findRepoRoot(ctx, repo) }()
				<-ctx.started
				for range tc.waiters {
					go func() { waiter <- findRepoRoot(waiterCtx, sub) }()
				}
				synctest.Wait()
				if tc.waiters > 1 {
					assert.Empty(t, waiter, "waiters must share the pending lookup")
				}
				if tc.cancelWaiter {
					cancelWaiter()
					assert.Empty(t, <-waiter)
				}
				if tc.cancelCreator {
					cancelCreator()
				}
				close(ctx.resume)
				if tc.cancelCreator {
					assert.Empty(t, <-creator)
				} else {
					assert.Equal(t, canonAll([]string{repo})[0], <-creator)
				}
				if !tc.cancelWaiter {
					for range tc.waiters {
						assert.Equal(t, canonAll([]string{repo})[0], <-waiter)
					}
				}
			})
		})
	}
}

func TestFindRepoRoot_ConfiguredLinkRetarget(t *testing.T) {
	for _, owner := range []string{"common", "worktree"} {
		for _, same := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/equal%v", owner, same), func(t *testing.T) {
				skipIfNoGit(t)
				repo := initBareRepo(t)
				config := filepath.Join(repo, ".git", "config")
				if owner == "worktree" {
					gitRun(t, repo, nil, "config", "extensions.worktreeConfig", "true")
					config = filepath.Join(repo, ".git", "config.worktree")
				}
				initial := repo
				if !same {
					initial = t.TempDir()
				}
				alias := filepath.Join(t.TempDir(), "worktree")
				linkRepoDirectory(t, alias, initial)
				gitRun(t, repo, nil, "config", "--file", config, "core.worktree", alias)
				if owner == "worktree" {
					text, err := os.ReadFile(config)
					require.NoError(t, err)
					text = []byte(strings.Replace(string(text), "[core]\n\tworktree", "[CoRe] WoRkTrEe", 1))
					require.NoError(t, os.WriteFile(config, text, 0o600))
				}
				require.Equal(t, canonAll([]string{initial})[0], findRepoRoot(t.Context(), repo))
				require.NoError(t, os.Remove(alias))
				replacement := t.TempDir()
				linkRepoDirectory(t, alias, replacement)
				assert.Equal(t, canonAll([]string{replacement})[0], findRepoRoot(t.Context(), repo))
			})
		}
	}
}

func TestFindRepoRoot_IncompleteConfigScan(t *testing.T) {
	skipIfNoGit(t)
	repo := initBareRepo(t)
	config := filepath.Join(repo, ".git", "config")
	file, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString("\n#" + strings.Repeat("x", 1<<16) + "\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), repo))
	t.Setenv("PATH", t.TempDir())
	assert.Empty(t, findRepoRoot(t.Context(), repo), "an incomplete scan must stay uncached")
}

func TestFindRepoRoot_AmbiguousMetadata(t *testing.T) {
	skipIfNoGit(t)
	repo := initBareRepo(t)
	sub := mkdirIn(t, repo, "nested")
	require.NoError(t, os.WriteFile(filepath.Join(sub, "HEAD"), nil, 0o600))
	assert.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), sub))
	gitRun(t, sub, nil, "init", "--bare", "-q")
	worktree := t.TempDir()
	gitRun(t, sub, nil, "config", "core.bare", "false")
	gitRun(t, sub, nil, "config", "core.worktree", worktree)
	assert.Equal(t, canonAll([]string{worktree})[0], findRepoRoot(t.Context(), sub))
	ordinary := initBareRepo(t)
	require.Equal(t, canonAll([]string{ordinary})[0], findRepoRoot(t.Context(), ordinary))
	assert.Empty(t, findRepoRoot(t.Context(), filepath.Join(ordinary, ".git")))
}

func TestFindRepoRoot_PointerParentAfterLink(t *testing.T) {
	for _, name := range []string{"gitfile", "commondir"} {
		t.Run(name, func(t *testing.T) {
			skipIfNoGit(t)
			repo := initBareRepo(t)
			holder, target := t.TempDir(), t.TempDir()
			child := mkdirIn(t, target, "child")
			link := filepath.Join(holder, "link")
			linkRepoDirectory(t, link, child)
			actual := filepath.Join(target, "actual")
			decoy := mkdirIn(t, holder, "actual")
			gitRun(t, decoy, nil, "init", "--bare", "-q")
			gitdir := filepath.Join(repo, ".git")
			base := repo
			if name == "commondir" {
				base = gitdir
			}
			pointer, err := filepath.Rel(base, link)
			require.NoError(t, err)
			pointer += string(filepath.Separator) + ".." + string(filepath.Separator) + "actual"
			if name == "gitfile" {
				require.NoError(t, os.Rename(gitdir, actual))
				require.NoError(t, os.WriteFile(gitdir, []byte("gitdir: "+pointer+"\n"), 0o600))
			} else {
				require.NoError(t, os.MkdirAll(actual, 0o755))
				gitRun(t, actual, nil, "init", "--bare", "-q")
				require.NoError(t, os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(pointer+"\n"), 0o600))
			}
			config := filepath.Join(actual, "config")
			gitRun(t, repo, nil, "config", "--file", config, "core.bare", "false")
			gitRun(t, repo, nil, "config", "--file", config, "core.worktree", repo)
			gitRun(t, repo, nil, "config", "--file", config, "extensions.worktreeConfig", "true")
			if runtime.GOOS == "windows" {
				gitRun(t, repo, nil, "config", "--file", filepath.Join(decoy, "config"), "core.bare", "false")
				gitRun(t, repo, nil, "config", "--file", filepath.Join(decoy, "config"), "core.worktree", repo)
				gitRun(t, repo, nil, "config", "--file", filepath.Join(decoy, "config"), "extensions.worktreeConfig", "true")
			}
			require.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), repo))
			t.Run("ambiguous pointer stays uncached", func(t *testing.T) {
				t.Setenv("PATH", t.TempDir())
				assert.Empty(t, findRepoRoot(t.Context(), repo))
			})
			worktree := t.TempDir()
			gitRun(t, repo, nil, "config", "--file", config, "core.worktree", worktree)
			if runtime.GOOS == "windows" {
				gitRun(t, repo, nil, "config", "--file", filepath.Join(decoy, "config"), "core.worktree", worktree)
			}
			assert.Equal(t, canonAll([]string{worktree})[0], findRepoRoot(t.Context(), repo))
		})
	}
}

func TestNearestGitMarker_DeviceBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git for Windows has no device boundary")
	}
	device, err := repoRootDevice("/dev/shm")
	if err != nil {
		t.Skip("shared memory filesystem unavailable")
	}
	parent, err := repoRootDevice("/dev")
	require.NoError(t, err)
	if parent == device {
		t.Skip("shared memory has the same device")
	}
	t.Setenv("TMPDIR", "/dev/shm")
	marker, absent := nearestGitMarker(t.TempDir())
	assert.Empty(t, marker.path)
	assert.False(t, absent, "a device boundary requires Git's own discovery")
}

// Pause at the fill's timeout creation, after it owns the cache entry.
type pausedRepoFill struct {
	context.Context
	started, resume chan struct{}
	startedOnce     sync.Once
	attempts        atomic.Int32
}

func (ctx *pausedRepoFill) Deadline() (time.Time, bool) {
	ctx.attempts.Add(1)
	ctx.startedOnce.Do(func() { close(ctx.started) })
	<-ctx.resume
	return ctx.Context.Deadline()
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
	unresolved := initBareRepo(t)
	t.Setenv("PATH", t.TempDir())
	ctx := &pausedRepoFill{Context: t.Context(), started: make(chan struct{}), resume: make(chan struct{})}
	close(ctx.resume)
	assert.Empty(t, DiscoverRepos(ctx, []string{unresolved, unresolved, unresolved}))
	assert.Equal(t, int32(1), ctx.attempts.Load(), "one failed lookup per distinct working directory in a request")
}

func TestDiscoverRepos_EmptyInputReturnsEmptySlice(t *testing.T) {
	got := DiscoverRepos(t.Context(), nil)
	require.NotNil(t, got, "DiscoverRepos(nil)")
	assert.Empty(t, got, "DiscoverRepos(nil) should be empty slice")
	got = DiscoverRepos(t.Context(), []string{})
	require.NotNil(t, got, "DiscoverRepos([])")
	assert.Empty(t, got, "DiscoverRepos([]) should be empty slice")
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
	mirror := initBareRepo(t)
	require.Len(t, DiscoverRepos(t.Context(), []string{primary, mirror}), 2)
	setOrigin(t, primary, origin)
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
