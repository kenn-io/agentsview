// Package git discovers local repositories and aggregates git-derived metrics
// for session analytics.
package git

import (
	"context"
	"os"
	"path/filepath"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"
)

// DiscoverRepos resolves each cwd to its enclosing git repository toplevel and
// returns the deduplicated list. Cwds with no enclosing repo (or whose
// resolution fails) are silently dropped. Order follows first-seen order in
// the input.
//
// Resolution prefers `git rev-parse --show-toplevel`, which handles standard
// `.git` directories, linked worktrees (`.git` is a file pointing at the
// shared gitdir), and submodules. When the cwd no longer exists on disk, the
// helper falls back to walking upward from the nearest existing ancestor and
// invoking `git rev-parse` from there — that mirrors how the parser package
// recovers repo roots for archived sessions whose cwd has been deleted.
func DiscoverRepos(ctx context.Context, cwds []string) []string {
	seen := map[string]struct{}{}
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
		out = append(out, root)
	}
	return out
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
