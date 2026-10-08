// Package git discovers local repositories and aggregates git-derived metrics
// for session analytics.
package git

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/pathutil"

	gitenv "go.kenn.io/kit/git/env"
	gitrepo "go.kenn.io/kit/git/repo"
	"go.kenn.io/kit/pathresolve"
)

// DiscoverRepos resolves each cwd to its enclosing git repository toplevel and
// returns one group of checkout paths per repository. Cwds with no enclosing
// repo (or whose resolution fails) are silently dropped. Order follows
// first-seen order in the input.
//
// Resolution prefers `git rev-parse --show-toplevel`, which handles standard
// `.git` directories, linked worktrees (`.git` is a file pointing at the
// shared gitdir), and submodules. When the cwd no longer exists on disk, the
// helper falls back to walking upward from the nearest existing ancestor and
// invoking `git rev-parse` from there — that mirrors how the parser package
// recovers repo roots for archived sessions whose cwd has been deleted.
// Ordinary owned roots are reused; fresh checks skip directories without repository metadata.
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

// Ordinary roots are shared while their Git marker and root configuration remain unchanged.
var repoRoots = struct {
	sync.Mutex
	entries map[string]*repoRootEntry
	pending map[string]*repoRootCheck
}{entries: make(map[string]*repoRootEntry), pending: make(map[string]*repoRootCheck)}

type repoRootCheck struct {
	done, ended <-chan struct{}
}

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
	worktreeInactive bool
}

type repoRootEligibility struct {
	marker        gitMarker
	config        repoRootConfig
	dir           string
	absent, known bool
}

func boundedRepoRootEligibility(ctx context.Context, key string, check func(context.Context) repoRootEligibility) repoRootEligibility {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		repoRoots.Lock()
		if ctx.Err() != nil {
			repoRoots.Unlock()
			break
		}
		pending := repoRoots.pending[key]
		if pending == nil {
			done := make(chan struct{})
			pending = &repoRootCheck{done: done, ended: ctx.Done()}
			repoRoots.pending[key] = pending
			repoRoots.Unlock()
			var result repoRootEligibility
			go func() {
				result = check(ctx)
				repoRoots.Lock()
				delete(repoRoots.pending, key)
				close(done)
				repoRoots.Unlock()
			}()
			select {
			case <-done:
				if ctx.Err() == nil {
					return result
				}
			case <-ctx.Done():
			}
			return repoRootEligibility{}
		}
		repoRoots.Unlock()
		select {
		case <-pending.done:
			continue
		case <-pending.ended:
			select {
			case <-pending.done:
				continue
			default:
				return repoRootEligibility{}
			}
		case <-ctx.Done():
			return repoRootEligibility{}
		}
	}
	return repoRootEligibility{}
}

func readRepoRootFile(ctx context.Context, path string, read func(io.Reader) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
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
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	err = read(ctxio.Reader{Reader: file, Context: ctx})
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return true, err
}

var repoRootWorktreeKey = regexp.MustCompile(`(?i)(^|])[[:space:]]*worktree([[:space:]=#;]|$)`)

func fingerprintRepoRootFile(ctx context.Context, path string) (repoRootFile, bool, error) {
	hash := sha256.New()
	worktree := false
	exists, err := readRepoRootFile(ctx, path, func(file io.Reader) error {
		scanner := bufio.NewScanner(io.TeeReader(file, hash))
		for scanner.Scan() {
			if strings.HasSuffix(scanner.Text(), "\\") {
				return os.ErrInvalid
			}
			worktree = worktree || repoRootWorktreeKey.Match(scanner.Bytes())
		}
		return scanner.Err()
	})
	return repoRootFile{sum: [sha256.Size]byte(hash.Sum(nil)), exists: exists}, worktree, err
}

func readRepoRootPointer(ctx context.Context, path string) (string, bool, error) {
	var text []byte
	exists, err := readRepoRootFile(ctx, path, func(file io.Reader) error {
		var err error
		text, err = io.ReadAll(io.LimitReader(file, 1<<20+1))
		if len(text) > 1<<20 {
			return os.ErrInvalid
		}
		return err
	})
	if err != nil {
		return "", exists, err
	}
	return string(text), exists, nil
}

// Git root setup reads these two config files directly, without expanding includes.
func snapshotRepoRootConfig(ctx context.Context, marker gitMarker, cwd string) (repoRootConfig, bool) {
	if marker.info == nil || ctx.Err() != nil || !repoRootOwned(filepath.Dir(marker.path)) || ctx.Err() != nil || !repoRootOwned(marker.path) {
		return repoRootConfig{}, false
	}
	gitdir := marker.path
	if marker.info.Mode().IsRegular() {
		text, _, err := readRepoRootPointer(ctx, marker.path)
		pointer, ok := strings.CutPrefix(text, "gitdir: ")
		gitdir = strings.TrimRight(pointer, "\r\n")
		if err != nil || !ok || gitdir == "" {
			return repoRootConfig{}, false
		}
		if !repoRootPointerEligible(gitdir) {
			return repoRootConfig{}, false
		}
		gitdir = pathutil.GitPointerPath(gitdir, filepath.Dir(marker.path), cwd)
		if gitdir == "" {
			return repoRootConfig{}, false
		}
	}
	if ctx.Err() != nil {
		return repoRootConfig{}, false
	}
	gitdir, err := pathresolve.EvalSymlinks(gitdir)
	if err != nil || ctx.Err() != nil || (gitdir != marker.path && !repoRootOwned(gitdir)) || !repoRootHeadValid(ctx, filepath.Join(gitdir, "HEAD")) {
		return repoRootConfig{}, false
	}
	text, exists, err := readRepoRootPointer(ctx, filepath.Join(gitdir, "commondir"))
	if err != nil {
		return repoRootConfig{}, false
	}
	common := gitdir
	if exists {
		common = strings.TrimRight(text, "\r\n")
		if common == "" || !repoRootPointerEligible(common) {
			return repoRootConfig{}, false
		}
		common = pathutil.GitPointerPath(common, gitdir, cwd)
		if common == "" {
			return repoRootConfig{}, false
		}
		if ctx.Err() != nil {
			return repoRootConfig{}, false
		}
		common, err = pathresolve.EvalSymlinks(common)
		if err != nil {
			return repoRootConfig{}, false
		}
	}
	if ctx.Err() != nil || !repoRootAccessible(filepath.Join(common, "objects")) || ctx.Err() != nil || !repoRootAccessible(filepath.Join(common, "refs")) {
		return repoRootConfig{}, false
	}
	config, key, err := fingerprintRepoRootFile(ctx, filepath.Join(common, "config"))
	if err != nil || key {
		return repoRootConfig{}, false
	}
	result := repoRootConfig{gitdir: gitdir, common: common, config: config}
	repoRoots.Lock()
	entry := repoRoots.entries[marker.path]
	if entry != nil {
		select {
		case <-entry.ready:
			result.worktreeInactive = entry.marker.matches(marker) && entry.config.gitdir == gitdir &&
				entry.config.common == common && entry.config.config == config && entry.config.worktreeInactive
		default:
		}
	}
	repoRoots.Unlock()
	if result.worktreeInactive {
		return result, true
	}
	result.worktree, key, err = fingerprintRepoRootFile(ctx, filepath.Join(gitdir, "config.worktree"))
	if err != nil {
		return repoRootConfig{}, false
	}
	if key {
		if !repoRootWorktreeInactive(ctx, gitdir, filepath.Join(common, "config")) {
			return repoRootConfig{}, false
		}
		result.worktree, result.worktreeInactive = repoRootFile{}, true
	}
	return result, true
}

var repoRootHead = regexp.MustCompile(`^(ref:[ \t\n\r]*refs/|[0-9a-fA-F]{40})`)

func repoRootHeadValid(ctx context.Context, path string) bool {
	if ctx.Err() != nil {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || ctx.Err() != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return err == nil && ctx.Err() == nil && strings.HasPrefix(target, "refs/")
	}
	if !info.Mode().IsRegular() {
		return false
	}
	var data []byte
	_, err = readRepoRootFile(ctx, path, func(file io.Reader) error {
		data, err = io.ReadAll(io.LimitReader(file, 255))
		return err
	})
	return err == nil && repoRootHead.Match(data)
}

func repoRootWorktreeInactive(ctx context.Context, gitdir, config string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--no-includes", "--file", config,
		"--type=bool", "--get", "extensions.worktreeConfig")
	cmd.Dir = gitdir
	cmd.Env = gitenv.StripAll(os.Environ())
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)) == "false"
	}
	exit, ok := errors.AsType[*exec.ExitError](err)
	return ok && exit.ExitCode() == 1 && ctx.Err() == nil
}

// Cleaning a parent component after a name can change which linked directory Git reads.
func repoRootPointerEligible(path string) bool {
	leading := !filepath.IsAbs(path)
	for part := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if part == ".." && !leading {
			return false
		}
		if part != "" && part != "." && part != ".." {
			leading = false
		}
	}
	return true
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
func nearestGitMarker(ctx context.Context, dir string) (gitMarker, bool) {
	if ctx.Err() != nil {
		return gitMarker{}, false
	}
	dir, err := pathresolve.EvalSymlinks(dir)
	if err != nil || ctx.Err() != nil {
		return gitMarker{}, false
	}
	device, err := repoRootDevice(dir)
	if err != nil {
		return gitMarker{}, false
	}
	for ctx.Err() == nil {
		current, err := repoRootDevice(dir)
		if err != nil || ctx.Err() != nil || current != device {
			return gitMarker{}, false
		}
		path := filepath.Join(dir, ".git")
		info, err := os.Lstat(path)
		if ctx.Err() != nil {
			return gitMarker{}, false
		}
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
			return gitMarker{}, false
		} else if !os.IsNotExist(err) {
			return gitMarker{}, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitMarker{}, true
		}
		dir = parent
	}
	return gitMarker{}, false
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
		eligibility := boundedRepoRootEligibility(ctx, start, func(batch context.Context) repoRootEligibility {
			dir := existingAncestor(batch, start)
			marker, absent := nearestGitMarker(batch, dir)
			config, known := snapshotRepoRootConfig(batch, marker, dir)
			return repoRootEligibility{marker: marker, config: config, dir: dir, absent: absent, known: known}
		})
		if ctx.Err() != nil || eligibility.absent {
			return ""
		}
		marker, config := eligibility.marker, eligibility.config
		if !eligibility.known {
			dir := eligibility.dir
			if dir == "" {
				dir = start
			}
			return gitToplevel(ctx, dir)
		}
		key := marker.path
		repoRoots.Lock()
		if entry := repoRoots.entries[key]; entry != nil {
			repoRoots.Unlock()
			select {
			case <-entry.ready:
			default:
				select {
				case <-entry.ready:
					continue
				case <-ctx.Done():
					return ""
				}
			}
			if ctx.Err() != nil {
				return ""
			}
			if entry.root == "" || !entry.marker.matches(marker) || entry.config != config {
				repoRoots.Lock()
				if repoRoots.entries[key] == entry {
					delete(repoRoots.entries, key)
				}
				repoRoots.Unlock()
				continue
			}
			return entry.root
		}
		if ctx.Err() != nil {
			repoRoots.Unlock()
			return ""
		}
		entry := &repoRootEntry{ready: make(chan struct{})}
		repoRoots.entries[key] = entry
		repoRoots.Unlock()
		root := gitToplevel(ctx, eligibility.dir)
		repoRoots.Lock()
		if ctx.Err() != nil {
			root = ""
		}
		entry.root = root
		if root != "" && filepath.Clean(root) == filepath.Dir(marker.path) {
			entry.marker = marker
			entry.config = config
		} else {
			delete(repoRoots.entries, key)
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
func existingAncestor(ctx context.Context, path string) string {
	dir := path
	for ctx.Err() == nil {
		info, err := os.Stat(dir)
		if ctx.Err() != nil {
			return ""
		}
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
	return ""
}

// gitToplevel runs `git rev-parse --show-toplevel` from dir and returns the
// trimmed result, or "" if git fails or prints nothing. A 5s timeout guards
// against hung git invocations on broken repos.
func gitToplevel(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root, err := gitrepo.Root(ctx, dir)
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return root
}
