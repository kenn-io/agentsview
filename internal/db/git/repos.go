// Package git discovers local repositories and aggregates git-derived metrics
// for session analytics.
package git

import (
	"context"
	"crypto/sha256"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"
	"go.kenn.io/kit/pathresolve"
)

// DiscoverRepos resolves each cwd to its enclosing git repository toplevel and
// returns one group of checkout paths per repository. Cwds with no enclosing
// repo (or whose resolution fails) are silently dropped. Order follows
// first-seen order in the input.
// Successful roots are reused; fresh filesystem checks skip directories with no repository metadata.
//
// Resolution prefers `git rev-parse --show-toplevel`, which handles standard
// `.git` directories, linked worktrees (`.git` is a file pointing at the
// shared gitdir), and submodules. When the cwd no longer exists on disk, the
// helper falls back to walking upward from the nearest existing ancestor and
// invoking `git rev-parse` from there — that mirrors how the parser package
// recovers repo roots for archived sessions whose cwd has been deleted.
//
// Checkouts with the same origin form one group. Keep every checkout so
// callers can count the union of their commits, including diverged branches.
// Repositories without an origin stay separate by local path.
func DiscoverRepos(ctx context.Context, cwds []string) [][]string {
	seenCwds := map[string]struct{}{}
	seen := map[string]struct{}{}
	position := map[string]int{}
	out := [][]string{}
	for _, cwd := range cwds {
		if _, ok := seenCwds[cwd]; ok {
			continue
		}
		seenCwds[cwd] = struct{}{}
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
			out = append(out, []string{root})
			continue
		}
		out[at] = append(out[at], root)
	}
	return out
}

// repoIdentity returns the key that identifies the repository root belongs to:
// its normalised `origin` URL, or the root itself when no origin resolves. The
// path fallback is prefixed so a directory can never collide with a remote URL.
func repoIdentity(ctx context.Context, root string) string {
	if origin := normalizeRemoteURL(originURL(ctx, root), root); origin != "" {
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
// scheme, credentials, default ports, a trailing `.git` and a trailing slash
// are dropped, the host is lowercased, and the SSH shorthand
// `git@host:owner/repo` is rewritten to `host/owner/repo`. Hosts are
// case-insensitive; the path is left as written because repository paths are
// not case-insensitive everywhere. Query and fragment data are preserved.
// Filesystem remotes resolve against root and retain their full directory
// names, including a `.git` suffix. Custom transports retain the full URL
// because their helpers can assign different meanings to its components.
// Returns "" for an empty or unparseable URL, which makes the caller fall back
// to the local path rather than merge repositories it cannot tell apart.
func normalizeRemoteURL(raw, root string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	local := false
	if strings.HasPrefix(raw, "file://") {
		u, err := url.Parse(raw)
		if err != nil || u.Path == "" {
			return ""
		}
		raw = filepath.FromSlash(u.Path)
		// file:///C:/... names an absolute Windows drive path.
		if strings.HasPrefix(raw, string(filepath.Separator)) && filepath.VolumeName(raw[1:]) != "" {
			raw = raw[1:]
		}
		local = true
	} else if !strings.Contains(raw, "://") {
		colon, slash := strings.IndexByte(raw, ':'), strings.IndexAny(raw, `/\`)
		local = filepath.VolumeName(raw) != "" || colon < 0 || (slash >= 0 && slash < colon)
	}
	if local {
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(root, raw)
		}
		if resolved, err := filepath.EvalSymlinks(raw); err == nil {
			raw = resolved
		}
		return "file:" + filepath.ToSlash(filepath.Clean(raw))
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
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		switch u.Scheme {
		case "http", "https", "ssh", "git":
		case "git+ssh", "ssh+git":
			u.Scheme = "ssh"
		default:
			return raw
		}
		switch u.Scheme + ":" + u.Port() {
		case "http:80", "https:443", "ssh:22", "git:9418":
			u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
		}
		u.RawPath = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(u.EscapedPath(), "/"), ".git"), "/")
		if u.RawPath == "" {
			return ""
		}
		u.Path, err = url.PathUnescape(u.RawPath)
		if err != nil {
			return ""
		}
		u.User = nil
		u.Host = strings.ToLower(u.Host)
		return strings.TrimPrefix(u.String(), u.Scheme+"://")
	}
	raw = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git"), "/")
	host, path, found := strings.Cut(raw, "/")
	if !found || host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + path
}

// Successful lookups survive while the nearest Git marker remains unchanged.
var repoRoots = struct {
	sync.Mutex
	entries map[string]*repoRootEntry
}{entries: make(map[string]*repoRootEntry)}

type repoRootEntry struct {
	ready  chan struct{}
	root   string
	marker gitMarker
	config repoRootConfig
}

type repoRootFile struct {
	sum    [sha256.Size]byte
	exists bool
}

type repoRootConfig struct {
	gitdir, common   string
	config, worktree repoRootFile
}

func readRepoRootFile(path string, out io.Writer) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, os.ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	_, err = io.Copy(out, file)
	return true, err
}

func fingerprintRepoRootFile(path string) (repoRootFile, error) {
	hash := sha256.New()
	exists, err := readRepoRootFile(path, hash)
	return repoRootFile{sum: [sha256.Size]byte(hash.Sum(nil)), exists: exists}, err
}

// Git root setup reads these two config files directly, without expanding includes.
func snapshotRepoRootConfig(marker gitMarker) (repoRootConfig, bool) {
	if marker.info == nil {
		return repoRootConfig{}, false
	}
	gitdir := marker.path
	if marker.info.Mode().IsRegular() {
		var file strings.Builder
		_, err := readRepoRootFile(marker.path, &file)
		pointer, ok := strings.CutPrefix(file.String(), "gitdir: ")
		gitdir = strings.TrimRight(pointer, "\r\n")
		if err != nil || !ok || gitdir == "" {
			return repoRootConfig{}, false
		}
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(filepath.Dir(marker.path), gitdir)
		}
	}
	gitdir, err := pathresolve.EvalSymlinks(gitdir)
	if err != nil {
		return repoRootConfig{}, false
	}
	var file strings.Builder
	exists, err := readRepoRootFile(filepath.Join(gitdir, "commondir"), &file)
	if err != nil {
		return repoRootConfig{}, false
	}
	common := gitdir
	if exists {
		common = strings.TrimRight(file.String(), "\r\n")
		if common == "" {
			return repoRootConfig{}, false
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		common, err = pathresolve.EvalSymlinks(common)
		if err != nil {
			return repoRootConfig{}, false
		}
	}
	config, err := fingerprintRepoRootFile(filepath.Join(common, "config"))
	if err != nil {
		return repoRootConfig{}, false
	}
	worktree, err := fingerprintRepoRootFile(filepath.Join(gitdir, "config.worktree"))
	return repoRootConfig{gitdir: gitdir, common: common, config: config, worktree: worktree}, err == nil
}

type gitMarker struct {
	path string
	info os.FileInfo
}

func (m gitMarker) matches(other gitMarker) bool {
	return m.info != nil && other.info != nil && m.path == other.path && os.SameFile(m.info, other.info) &&
		m.info.Mode().Type() == other.info.Mode().Type() &&
		(m.info.IsDir() || m.info.Size() == other.info.Size() && m.info.ModTime().Equal(other.info.ModTime()))
}

// nearestGitMarker validates cached roots; Git still resolves unusual layouts.
func nearestGitMarker(dir string) (gitMarker, bool) {
	dir, err := pathresolve.EvalSymlinks(dir)
	if err != nil {
		return gitMarker{}, false
	}
	sawHead := false
	for {
		path := filepath.Join(dir, ".git")
		info, err := os.Lstat(path)
		if err == nil {
			// Windows FileInfo loads its identity lazily; capture it before the path can be replaced.
			if (info.IsDir() || info.Mode().IsRegular()) && os.SameFile(info, info) {
				return gitMarker{path: path, info: info}, false
			}
			return gitMarker{}, false
		}
		if !os.IsNotExist(err) {
			return gitMarker{}, false
		}
		if _, err := os.Lstat(filepath.Join(dir, "HEAD")); err == nil {
			sawHead = true
		} else if !os.IsNotExist(err) {
			return gitMarker{}, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitMarker{}, !sawHead
		}
		dir = parent
	}
}

// findRepoRoot returns the absolute repo toplevel for start, or "" when no enclosing repo resolves.
func findRepoRoot(ctx context.Context, start string) string {
	if start == "" {
		return ""
	}
	start, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for ctx.Err() == nil {
		repoRoots.Lock()
		if entry := repoRoots.entries[start]; entry != nil {
			repoRoots.Unlock()
			select {
			case <-entry.ready:
			case <-ctx.Done():
				return ""
			}
			marker, _ := nearestGitMarker(existingAncestor(start))
			config, known := snapshotRepoRootConfig(marker)
			if !entry.marker.matches(marker) || !known || entry.config != config {
				repoRoots.Lock()
				if repoRoots.entries[start] == entry {
					delete(repoRoots.entries, start)
				}
				repoRoots.Unlock()
				continue
			}
			return entry.root
		}
		entry := &repoRootEntry{ready: make(chan struct{})}
		repoRoots.entries[start] = entry
		repoRoots.Unlock()
		dir := existingAncestor(start)
		marker, absent := nearestGitMarker(dir)
		config, known := snapshotRepoRootConfig(marker)
		root := ""
		if dir != "" && !absent {
			root = gitToplevel(ctx, dir)
		}
		cacheable := root != "" && known && filepath.Clean(root) == filepath.Dir(marker.path)
		repoRoots.Lock()
		entry.root = root
		if cacheable {
			entry.marker = marker
			entry.config = config
		} else {
			delete(repoRoots.entries, start)
		}
		close(entry.ready)
		repoRoots.Unlock()
		return root
	}
	return ""
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
