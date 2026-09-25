// Package git discovers local repositories and aggregates git-derived metrics
// for session analytics.
package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"
)

// DiscoverRepos resolves each cwd to its enclosing git repository toplevel and
// returns one entry per repository. Cwds with no enclosing repo (or whose
// resolution fails) are silently dropped. Order follows first-seen order in
// the input.
//
// Resolution prefers `git rev-parse --show-toplevel`, which handles standard
// `.git` directories, linked worktrees (`.git` is a file pointing at the
// shared gitdir), and submodules. When the cwd no longer exists on disk, the
// helper falls back to walking upward from the nearest existing ancestor and
// invoking `git rev-parse` from there — that mirrors how the parser package
// recovers repo roots for archived sessions whose cwd has been deleted.
//
// A repository is identified by its `origin` remote, not by its local
// directory. Callers aggregate per returned entry, and commit and
// pull-request counts come from the remote: a mirror, a second clone, or a
// linked worktree resolving to its own toplevel would each add the remote's
// numbers again. When two checkouts share a remote, the one whose HEAD commit
// is newest represents it, because a stale checkout is missing the newest
// commits and would undercount. A repository with no resolvable origin falls
// back to its local path, so local-only work stays its own repository.
func DiscoverRepos(ctx context.Context, cwds []string) []string {
	seen := map[string]struct{}{}
	// position records, per identity key, the index in out that the
	// identity's representative occupies, so replacing a stale checkout with
	// a fresher one keeps first-seen order.
	position := map[string]int{}
	out := []string{}
	for _, cwd := range cwds {
		root := findRepoRoot(ctx, cwd)
		if root == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		key := repoIdentity(ctx, root)
		at, ok := position[key]
		if !ok {
			position[key] = len(out)
			out = append(out, root)
			continue
		}
		if headCommitTime(ctx, root).After(headCommitTime(ctx, out[at])) {
			out[at] = root
		}
	}
	return out
}

// repoIdentity returns the key that identifies the repository root belongs to:
// its normalised `origin` URL, or the root itself when no origin resolves. The
// path fallback is prefixed so a directory can never collide with a remote URL.
func repoIdentity(ctx context.Context, root string) string {
	if origin := normalizeRemoteURL(originURL(ctx, root)); origin != "" {
		return "origin:" + origin
	}
	return "path:" + root
}

// originURL returns the configured `origin` remote URL for root, or "" when
// there is none or git fails. A 5s timeout guards against hung invocations on
// broken repos, matching gitToplevel.
func originURL(ctx context.Context, root string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// normalizeRemoteURL reduces the spellings of one remote to a single key:
// scheme, credentials, a trailing `.git` and a trailing slash are dropped, the
// host is lowercased, and the SSH shorthand `git@host:owner/repo` is rewritten
// to `host/owner/repo`. Host case is normalised because hosts are
// case-insensitive; the path is left as written because repository paths are
// not case-insensitive everywhere. A remote that is an absolute filesystem
// path, written directly or as `file://`, keeps that path as its key.
// Returns "" for an empty or unparseable URL, which makes the caller fall back
// to the local path rather than merge repositories it cannot tell apart.
func normalizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// scp-like shorthand: [user@]host:path, which has no "//" after a scheme.
	if !strings.Contains(raw, "://") {
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			raw = raw[at+1:]
		}
		if colon := strings.Index(raw, ":"); colon >= 0 {
			raw = raw[:colon] + "/" + raw[colon+1:]
		}
	} else {
		raw = raw[strings.Index(raw, "://")+len("://"):]
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			raw = raw[at+1:]
		}
	}
	raw = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git"), "/")
	if strings.HasPrefix(raw, "/") {
		// An absolute filesystem remote — a local bare mirror — has no host to
		// normalise, and the path alone already identifies it.
		return raw
	}
	host, path, found := strings.Cut(raw, "/")
	if !found || host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + path
}

// headCommitTime returns the committer time of root's HEAD commit, or the zero
// time when the repository has no commits or git fails. It decides which of
// several checkouts of one remote is the freshest.
func headCommitTime(ctx context.Context, root string) time.Time {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%ct", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// findRepoRoot returns the absolute repo toplevel for start, or "" when no
// enclosing repo can be resolved.
func findRepoRoot(ctx context.Context, start string) string {
	if start == "" {
		return ""
	}
	dir := existingAncestor(start)
	if dir == "" {
		return ""
	}
	return gitToplevel(ctx, dir)
}

// existingAncestor returns the closest ancestor of path that exists on disk
// and is a directory. If path itself is an existing directory, it is
// returned. Returns "" when no ancestor exists (only possible on torn
// filesystems or invalid roots).
func existingAncestor(path string) string {
	dir := path
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if info.IsDir() {
				return dir
			}
			dir = filepath.Dir(dir)
			continue
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// gitToplevel runs `git rev-parse --show-toplevel` from dir and returns the
// trimmed result, or "" if git fails or prints nothing. A 5s timeout guards
// against hung git invocations on broken repos.
func gitToplevel(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root, err := gitrepo.Root(ctx, dir)
	if err != nil {
		return ""
	}
	return root
}
