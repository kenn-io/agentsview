package remotesync

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

func safeRemotePathArchiveName(remotePath string) (string, error) {
	p := strings.ReplaceAll(remotePath, `\`, "/")
	if hasDotDotPathComponent(p) {
		return "", fmt.Errorf("unsafe remote path %q: contains '..'", remotePath)
	}
	if p == "." || p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "//") {
		rest := pathpkg.Clean(strings.TrimLeft(p, "/"))
		if rest == "." || rest == "" {
			return "", fmt.Errorf("unsafe remote path %q: empty UNC path", remotePath)
		}
		return pathpkg.Join("__unc", rest), nil
	}
	if len(p) >= 2 && p[1] == ':' {
		drive := "__drive_" + p[:1]
		rest := strings.TrimLeft(p[2:], "/")
		if rest == "" {
			return drive, nil
		}
		return pathpkg.Join(drive, pathpkg.Clean(rest)), nil
	}
	p = pathpkg.Clean(p)
	if pathpkg.IsAbs(p) {
		p = strings.TrimLeft(p, "/")
	}
	p = strings.TrimLeft(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	return p, nil
}

func hasDotDotPathComponent(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}

func remappedRemotePath(tempDir, remotePath string) string {
	local, err := safeRemappedRemotePath(tempDir, remotePath)
	if err != nil {
		return tempDir
	}
	return local
}

func safeRemappedRemotePath(tempDir, remotePath string) (string, error) {
	name, err := safeRemotePathArchiveName(remotePath)
	if err != nil {
		return "", err
	}
	return safeLocalArchivePath(tempDir, name)
}

func safeLocalArchivePath(tempDir, archiveName string) (string, error) {
	raw := strings.ReplaceAll(archiveName, `\`, "/")
	if pathpkg.IsAbs(raw) || hasDotDotPathComponent(raw) {
		return "", fmt.Errorf("unsafe archive path %q", archiveName)
	}
	clean := pathpkg.Clean(raw)
	if clean == "." || clean == "" {
		return tempDir, nil
	}
	parts := strings.Split(clean, "/")
	elems := make([]string, 0, len(parts)+1)
	elems = append(elems, tempDir)
	elems = append(elems, parts...)
	local := filepath.Join(elems...)
	if !within(tempDir, local) {
		return "", fmt.Errorf("unsafe archive path %q escapes extraction dir", archiveName)
	}
	return local, nil
}

func remoteArchiveRel(remoteDir, remotePath string) (string, bool) {
	base, err := safeRemotePathArchiveName(remoteDir)
	if err != nil {
		return "", false
	}
	name, err := safeRemotePathArchiveName(remotePath)
	if err != nil {
		return "", false
	}
	if base == "" {
		return name, true
	}
	if name == base {
		return "", true
	}
	prefix := base + "/"
	if after, ok := strings.CutPrefix(name, prefix); ok {
		return after, true
	}
	return "", false
}

func validateTargetSetPaths(targets TargetSet) error {
	for agent, dirs := range targets.Dirs {
		for _, dir := range dirs {
			if _, err := safeRemotePathArchiveName(dir); err != nil {
				return fmt.Errorf("target dir %s %q: %w", agent, dir, err)
			}
		}
	}
	for agent, files := range targets.Files {
		for _, file := range files {
			if _, err := safeRemotePathArchiveName(file); err != nil {
				return fmt.Errorf("target file %s %q: %w", agent, file, err)
			}
		}
	}
	for _, file := range targets.AllExtraFiles() {
		if _, err := safeRemotePathArchiveName(file); err != nil {
			return fmt.Errorf("target file %q: %w", file, err)
		}
	}
	for _, root := range targets.ForbiddenRoots {
		if _, err := safeRemotePathArchiveName(root); err != nil {
			return fmt.Errorf("forbidden root %q: %w", root, err)
		}
	}
	return nil
}

// pathWithinForbiddenRoots reports whether path is a forbidden root or lies
// beneath one, treating path and roots as local OS filepaths. Both sides are
// first canonicalized into the same local comparison domain (see
// localComparablePath), so a forbidden root configured with a relative
// spelling still guards its children when compared against an absolute
// target, and spelling-variant aliases of one directory on a
// case-insensitive filesystem compare equal. It wraps
// PathWithinForbiddenRoots for this package's many same-package callers,
// all of which operate on local filesystem paths.
func pathWithinForbiddenRoots(roots []string, path string) bool {
	return newForbiddenRootMatcher(roots).within(path)
}

// forbiddenRootMatcher holds a forbidden-root set with spellings already
// canonicalized into the local comparison domain. Archive and manifest
// walks construct one per operation so each entry canonicalizes only the
// candidate path instead of re-resolving every root for every entry; the
// zero matcher matches nothing and does no work at all.
type forbiddenRootMatcher struct {
	comparable []string
}

func newForbiddenRootMatcher(roots []string) forbiddenRootMatcher {
	var comparable []string
	for _, root := range roots {
		// filepath.Abs("") would resolve to the working directory,
		// silently turning a blank root into a cwd-wide exclusion.
		if root == "" {
			continue
		}
		comparable = append(comparable, localComparablePath(root))
	}
	return forbiddenRootMatcher{comparable: comparable}
}

// within reports whether path is a forbidden root or lies beneath one.
// Canonicalizing the candidate touches the filesystem (Abs plus symlink
// resolution), so callers must pass only trusted paths or client paths
// already validated against an allowed root — never a raw request string,
// which on Windows could name a UNC share and force an outbound
// connection just by being stat'ed.
func (m forbiddenRootMatcher) within(path string) bool {
	if len(m.comparable) == 0 {
		return false
	}
	return PathWithinForbiddenRoots(
		m.comparable, localComparablePath(path), filepath.Separator,
	)
}

// localComparablePath canonicalizes a local OS path for forbidden-root
// comparison: resolved to absolute against the process working directory
// (so "." and other relative override spellings anchor to the same place
// as their absolute aliases), symlink-resolved (so a root reached through
// a symlinked ancestor compares by its physical location — the walkers
// skip symlink entries, but they do follow symlinked ancestors of the
// roots they are handed), and case-folded on Windows and macOS, whose
// default filesystems are case-insensitive. Folding on a case-sensitive
// APFS/NTFS variant can only over-prune — it never widens what leaves the
// machine.
func localComparablePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	abs = resolveSymlinksBestEffort(abs)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	return abs
}

// resolveSymlinksBestEffort resolves symlinks in p, falling back to
// resolving the longest existing ancestor and keeping the missing tail
// literal (forbidden roots may name directories that do not exist yet;
// their eventual contents still deserve the boundary). A path that cannot
// be resolved at all is returned unchanged — comparisons then degrade to
// the lexical behavior rather than failing open by returning nothing.
// evalSymlinksFn is swapped by tests that assert which paths reach
// filesystem-touching canonicalization.
var evalSymlinksFn = filepath.EvalSymlinks

func resolveSymlinksBestEffort(p string) string {
	if resolved, err := evalSymlinksFn(p); err == nil {
		return resolved
	}
	dir, tail := filepath.Dir(p), filepath.Base(p)
	for {
		if resolved, err := evalSymlinksFn(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		tail = filepath.Join(filepath.Base(dir), tail)
		dir = parent
	}
}

// PathWithinForbiddenRoots reports whether path is a forbidden root or lies
// beneath one. sep is the path separator of the caller's domain: '/' for
// remote POSIX paths (see internal/ssh, which builds paths for the resolve
// script and tar filter), or filepath.Separator for local OS paths. Roots
// and path are normalized (dot segments resolved, redundant separators
// collapsed) before comparison, and matching requires a full path-component
// boundary so sibling names such as .forbidden-provider-backup do not get
// conflated with a protected directory (root "/a/b" must not match path
// "/a/bc").
//
// Unlike filepath.Clean/filepath.Rel on Windows, normalization here is not
// volume-aware: it does not preserve a leading UNC "\\host\share" marker or
// refuse to compare paths on different drives. Correctness for volume-scoped
// paths therefore depends on root and path having passed through the same
// cleanPathWithSeparator call with the same sep, which cancels out any
// shared lossy transform (both current call sites satisfy this — see
// cleanPathWithSeparator). This function does not independently reject a
// root and path that name different volumes; do not call it with root and
// path drawn from different normalization domains.
func PathWithinForbiddenRoots(roots []string, path string, sep byte) bool {
	path = cleanPathWithSeparator(path, sep)
	sepStr := string(sep)
	for _, root := range roots {
		root = cleanPathWithSeparator(root, sep)
		if path == root {
			return true
		}
		if root == sepStr {
			if strings.HasPrefix(path, sepStr) {
				return true
			}
			continue
		}
		if strings.HasPrefix(path, root+sepStr) {
			return true
		}
	}
	return false
}

// cleanPathWithSeparator normalizes p (resolving "." and ".." components and
// collapsing redundant separators) using sep as the path separator. path.Clean
// implements this for '/'; for any other separator, p is translated to '/',
// cleaned, and translated back so the same normalization rules apply
// regardless of which OS is running the check.
//
// This is lossy for Windows volume markers: path.Clean collapses a leading
// "//" (translated from a UNC "\\host\share" prefix) down to a single
// separator, whereas real filepath.Clean on Windows preserves it, and this
// function has no equivalent of filepath.Rel's refusal to relate paths on
// different drives (e.g. "C:\x" vs "D:\x"). PathWithinForbiddenRoots relies
// on root and path always passing through this same lossy transform so the
// collapse cancels out in the comparison; it does not restore true
// volume-name awareness.
func cleanPathWithSeparator(p string, sep byte) string {
	if sep == '/' {
		return pathpkg.Clean(p)
	}
	sepStr := string(sep)
	slashed := strings.ReplaceAll(p, sepStr, "/")
	cleaned := pathpkg.Clean(slashed)
	return strings.ReplaceAll(cleaned, "/", sepStr)
}

func tempPathToRemotePath(
	tempPath string,
	remoteDirs []string,
	tempDirs []string,
) (string, bool) {
	for i, tempDir := range tempDirs {
		rel, ok := localPathRel(tempDir, tempPath)
		if !ok {
			continue
		}
		return joinRemoteRelative(remoteDirs[i], rel), true
	}
	return "", false
}

func localPathRel(base, name string) (string, bool) {
	rel, err := filepath.Rel(base, name)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	if filepath.IsAbs(rel) ||
		rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func joinRemoteRelative(remoteDir, localRel string) string {
	if localRel == "" {
		return remoteDir
	}
	rel := filepath.ToSlash(localRel)
	if strings.Contains(remoteDir, `\`) && !strings.Contains(remoteDir, "/") {
		return strings.TrimRight(remoteDir, `/\`) + `\` +
			strings.ReplaceAll(rel, "/", `\`)
	}
	return strings.TrimRight(remoteDir, `/\`) + "/" + rel
}
