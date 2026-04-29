package parser

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"
)

// osStat is indirected through a var so tests can intercept stat
// calls from the git-root walker. Production code always uses
// os.Stat via this binding.
var osStat = os.Stat

var projectMarkers = []string{
	"code", "projects", "repos", "src", "work", "dev",
}

var ignoredSystemDirs = map[string]bool{
	"users": true, "home": true, "var": true,
	"tmp": true, "private": true,
}

// NormalizeName converts dashes to underscores for consistent
// project name formatting.
func NormalizeName(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// GetProjectName converts an encoded Claude project directory name
// to a clean project name. Claude encodes paths like
// /Users/alice/code/my-app as -Users-alice-code-my-app.
func GetProjectName(dirName string) string {
	if dirName == "" {
		return ""
	}

	if !strings.HasPrefix(dirName, "-") {
		return NormalizeName(dirName)
	}

	parts := strings.Split(dirName, "-")

	// Strategy 1: find a known project parent directory marker
	for _, marker := range projectMarkers {
		for i, part := range parts {
			if strings.EqualFold(part, marker) && i+1 < len(parts) {
				result := strings.Join(parts[i+1:], "-")
				if result != "" {
					return NormalizeName(result)
				}
			}
		}
	}

	// Strategy 2: use last non-system-directory component
	for i := len(parts) - 1; i >= 0; i-- {
		if p := parts[i]; p != "" && !ignoredSystemDirs[strings.ToLower(p)] {
			return NormalizeName(p)
		}
	}

	return NormalizeName(dirName)
}

// ExtractProjectFromCwd extracts a project name from a working
// directory path. If cwd is inside a git repository (including
// linked worktrees), this returns the repository root directory
// name. Otherwise it falls back to the last path component.
func ExtractProjectFromCwd(cwd string) string {
	return ExtractProjectFromCwdWithBranch(cwd, "")
}

// ExtractProjectFromCwdWithBranch extracts a canonical project
// name from cwd and optionally git branch metadata. Branch is
// used as a fallback heuristic when the original worktree path no
// longer exists on disk.
func ExtractProjectFromCwdWithBranch(
	cwd, gitBranch string,
) string {
	if cwd == "" {
		return ""
	}
	winPath := looksLikeWindowsPath(cwd)
	norm := cwd
	if winPath {
		norm = strings.ReplaceAll(cwd, "\\", "/")
	}
	cleaned := filepath.Clean(norm)

	// Recognize worktree manager layouts before walking git roots.
	// These layouts encode the owning project in the path even when
	// the git root basename is a branch or generated worktree id.
	if p := projectFromWorktreeLayout(cleaned); p != "" {
		return NormalizeName(p)
	}

	// Skip the git-root walk when the cwd cannot resolve to a
	// real local filesystem location. On macOS a bulk walk under
	// an unbacked autofs prefix cascades through automountd into
	// opendirectoryd (/usr/libexec/od_user_homes), so we probe
	// the prefix once before walking.
	if !isForeignOSPath(cwd, cleaned, winPath) {
		if root := findGitRepoRoot(cleaned); root != "" {
			name := filepath.Base(root)
			if isInvalidPathBase(name) {
				return ""
			}
			return NormalizeName(name)
		}
	}

	name := filepath.Base(cleaned)
	if isInvalidPathBase(name) {
		return ""
	}
	name = trimBranchSuffix(name, gitBranch)
	if isInvalidPathBase(name) {
		return ""
	}
	return NormalizeName(name)
}

// worktreeLayout describes path fragments that identify worktree
// manager directory conventions. projectPart is the zero-based
// component after marker that contains the owning project name.
type worktreeLayout struct {
	marker      string
	projectPart int
	minParts    int
}

var worktreeLayouts []worktreeLayout

func init() {
	sep := string(filepath.Separator)
	worktreeLayouts = []worktreeLayout{
		// .superset/worktrees/$PROJECT/$BRANCH[/...]
		{marker: sep + ".superset" + sep + "worktrees" + sep, projectPart: 0, minParts: 2},
		// conductor/workspaces/$PROJECT/$BRANCH[/...]
		{marker: sep + "conductor" + sep + "workspaces" + sep, projectPart: 0, minParts: 2},
		// ~/.config/middleman/worktrees/github.com/$OWNER/$REPO/$WORKTREE[/...]
		{marker: sep + ".config" + sep + "middleman" + sep + "worktrees" + sep + "github.com" + sep, projectPart: 1, minParts: 3},
		// ~/.codex/worktrees/$WORKTREE_ID/$REPO[/...]
		{marker: sep + ".codex" + sep + "worktrees" + sep, projectPart: 1, minParts: 2},
	}
}

// projectFromWorktreeLayout detects known worktree manager
// directory layouts and extracts the project name component.
// Returns "" if the path does not match any known layout.
func projectFromWorktreeLayout(path string) string {
	for _, layout := range worktreeLayouts {
		_, rest, found := strings.Cut(path, layout.marker)
		if !found {
			continue
		}
		parts := strings.Split(rest, string(filepath.Separator))
		if len(parts) < layout.minParts {
			continue
		}
		project := parts[layout.projectPart]
		if isInvalidPathBase(project) {
			continue
		}
		return project
	}
	return ""
}

// autofsMountSource is indirected so tests can supply fixture
// output in lieu of running mount(8).
var autofsMountSource = runMountCommand

func runMountCommand() ([]byte, error) {
	return exec.Command("/sbin/mount").Output()
}

// autofsPrefixes holds path prefixes that autofs is actively
// managing on this host, each with a trailing separator so
// strings.HasPrefix gives component-boundary matches. Populated
// at package init on darwin from the live mount table; other
// platforms leave it empty.
//
// Why we care: os.Stat into an autofs-managed prefix triggers
// automountd. For the default /home entry macOS resolves the map
// via /usr/libexec/od_user_homes, which asks opendirectoryd to
// enumerate every user record. Bulk remote-sync runs whose
// session cwds all share a /home/<user>/... prefix therefore peg
// opendirectoryd and automountd at hundreds of percent CPU.
//
// Sourcing the prefix set from mount(8) (rather than parsing
// /etc/auto_master directly) captures prefixes pulled in via
// +auto_master directory-service includes, which never appear
// in the local config file.
var autofsPrefixes = detectAutofsPrefixes()

// detectAutofsPrefixes returns the autofs-managed path prefixes
// reported by the running mount table. Non-darwin hosts and
// exec failures both return nil.
func detectAutofsPrefixes() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	data, err := autofsMountSource()
	if err != nil {
		return nil
	}
	return parseMountOutputForAutofs(data)
}

// parseMountOutputForAutofs extracts the mount points of autofs
// filesystems from mount(8) output. Typical macOS lines look
// like:
//
//	map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
//	server.example:/export on /corp/home (autofs, nobrowse)
//
// macOS presents the Data volume at / via an APFS firmlink, so
// /System/Volumes/Data is stripped to match the path form that
// client code observes.
func parseMountOutputForAutofs(data []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.Contains(line, "(autofs") {
			continue
		}
		_, after, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		rest := after
		parenIdx := strings.LastIndex(rest, " (")
		if parenIdx < 0 {
			continue
		}
		mount := strings.TrimSpace(rest[:parenIdx])
		mount = strings.TrimPrefix(mount, "/System/Volumes/Data")
		if mount == "" || mount == "/" {
			continue
		}
		out = append(out, strings.TrimRight(mount, "/")+"/")
	}
	return out
}

// isForeignOSPath reports whether cwd should bypass the local
// git-root walk.
//
//   - Windows-convention paths on POSIX hosts (drive letters, UNC
//     prefixes) cannot exist as real filesystem locations.
//   - Paths under a local autofs prefix whose first component does
//     not resolve: walking them on macOS triggers automountd per
//     ancestor, and auto_home's resolver asks opendirectoryd to
//     enumerate every user record — a hundred-percent-CPU storm
//     under bulk remote sync.
//
// An autofs prefix that does resolve (e.g. an enterprise NFS-backed
// /home) falls through to the walk so repository roots are still
// found.
func isForeignOSPath(cwd, cleaned string, winPath bool) bool {
	if winPath {
		return runtime.GOOS != "windows"
	}
	for _, prefix := range autofsPrefixes {
		if !strings.HasPrefix(cleaned, prefix) {
			continue
		}
		return !autofsFirstLevelResolves(prefix, cleaned)
	}
	return false
}

// autofsProbeTTL bounds how long a probe result (positive or
// negative) is trusted. Long enough that a bulk sync pays for
// one probe per unique prefix user; short enough that a long-
// running server rediscovers a mount that came up after startup.
const autofsProbeTTL = 60 * time.Second

// nowFn is indirected so TTL-expiry tests can advance the clock.
var nowFn = time.Now

type autofsProbeEntry struct {
	resolves bool
	expires  time.Time
}

// autofsProbes memoises whether the first path component under an
// autofs prefix resolves locally. Keyed by the probed path
// (e.g. "/home/wes"), so a bulk sync whose cwds all share a
// single <prefix>/<user> pays one probe rather than one per
// session.
//
// Writes go through autofsProbeSF to collapse concurrent misses
// for the same key into a single osStat call — critical because
// sync workers probe in parallel.
var (
	autofsProbesMu sync.Mutex
	autofsProbes   = map[string]autofsProbeEntry{}
	autofsProbeSF  singleflight.Group
)

// resetAutofsProbes clears the cache and in-flight probes.
// Intended for tests that need deterministic probe counts.
func resetAutofsProbes() {
	autofsProbesMu.Lock()
	autofsProbes = map[string]autofsProbeEntry{}
	autofsProbesMu.Unlock()
	autofsProbeSF = singleflight.Group{}
}

// autofsFirstLevelResolves probes whether <prefix>/<first-comp>
// exists as a real filesystem entry. Results are cached for
// autofsProbeTTL and concurrent misses share a single probe.
func autofsFirstLevelResolves(prefix, cleaned string) bool {
	rest := cleaned[len(prefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	key := prefix + rest

	if resolves, ok := lookupAutofsProbe(key); ok {
		return resolves
	}

	v, _, _ := autofsProbeSF.Do(key, func() (any, error) {
		// Re-check inside the singleflight slot: a prior
		// caller for the same key may have just populated
		// the cache while we were waiting for the slot.
		if resolves, ok := lookupAutofsProbe(key); ok {
			return resolves, nil
		}
		_, err := osStat(key)
		resolves := err == nil
		storeAutofsProbe(key, resolves)
		return resolves, nil
	})
	return v.(bool)
}

func lookupAutofsProbe(key string) (bool, bool) {
	autofsProbesMu.Lock()
	defer autofsProbesMu.Unlock()
	entry, ok := autofsProbes[key]
	if !ok || !nowFn().Before(entry.expires) {
		return false, false
	}
	return entry.resolves, true
}

func storeAutofsProbe(key string, resolves bool) {
	autofsProbesMu.Lock()
	autofsProbes[key] = autofsProbeEntry{
		resolves: resolves,
		expires:  nowFn().Add(autofsProbeTTL),
	}
	autofsProbesMu.Unlock()
}

// looksLikeWindowsPath returns true when cwd appears to use
// Windows path conventions: a drive letter (e.g. "C:\...") or a
// UNC prefix ("\\server\..."). On POSIX, backslash is a legal
// filename character so we must not blindly rewrite it.
func looksLikeWindowsPath(cwd string) bool {
	if len(cwd) >= 3 && cwd[1] == ':' && cwd[2] == '\\' {
		c := cwd[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return true
		}
	}
	if strings.HasPrefix(cwd, "\\\\") {
		return true
	}
	return false
}

func isInvalidPathBase(name string) bool {
	if name == "." || name == ".." || name == "/" || name == string(filepath.Separator) {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	return false
}

// findGitRepoRoot walks upward from cwd to find the enclosing git
// repository root. Supports both standard repos (.git directory)
// and linked worktrees/submodules (.git file). When cwd no longer
// exists on disk, sibling directories are checked for worktree
// .git files that can reveal the true repo root.
func findGitRepoRoot(cwd string) string {
	if cwd == "" {
		return ""
	}

	dir := cwd
	cwdMissing := false
	if info, err := osStat(dir); err == nil {
		if !info.IsDir() {
			dir = filepath.Dir(dir)
		}
	} else {
		// Avoid treating non-path strings as cwd.
		if !strings.ContainsRune(dir, filepath.Separator) {
			return ""
		}
		cwdMissing = true
		dir = filepath.Dir(dir)
	}

	// When the original path is gone, walk up to the first
	// existing ancestor and check its children for worktree
	// .git files. This handles nested worktrees (e.g.
	// worktrees/project/branch/cmd/server) where the whole
	// subtree may be deleted.
	if cwdMissing {
		sibDir := dir
		for {
			if _, err := osStat(sibDir); err == nil {
				break
			}
			parent := filepath.Dir(sibDir)
			if parent == sibDir {
				break
			}
			sibDir = parent
		}
		if root := repoRootFromSiblings(sibDir, cwd); root != "" {
			return root
		}
	}

	for {
		gitPath := filepath.Join(dir, ".git")
		info, err := osStat(gitPath)
		if err == nil {
			if info.IsDir() {
				return dir
			}
			if info.Mode().IsRegular() {
				if root := repoRootFromGitFile(dir, gitPath); root != "" {
					return root
				}
				// Keep conservative fallback for gitfile repos
				// when metadata cannot be parsed.
				return dir
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// repoRootFromSiblings checks child directories of dir for
// linked-worktree .git files and uses them to discover the
// true repo root. Submodule .git files are skipped, and all
// candidates must agree on the same root to avoid
// misattributing unrelated paths.
func repoRootFromSiblings(dir, cwd string) string {
	// If dir is itself a repo or worktree, let the normal
	// upward walk handle it.
	if _, err := osStat(filepath.Join(dir, ".git")); err == nil {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	worktreeMarker := string(filepath.Separator) + ".git" +
		string(filepath.Separator) + "worktrees" +
		string(filepath.Separator)
	// Two-pass scan: first collect linked-worktree roots,
	// then optionally include .git directory siblings only
	// when worktree evidence exists.
	type siblingInfo struct {
		root  string // resolved repo root
		isDir bool   // true = .git directory, false = .git file
	}
	var siblings []siblingInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		gitPath := filepath.Join(dir, entry.Name(), ".git")
		info, err := osStat(gitPath)
		if err != nil {
			continue
		}
		if info.IsDir() {
			siblings = append(siblings, siblingInfo{
				root:  filepath.Join(dir, entry.Name()),
				isDir: true,
			})
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		gitDir := readGitDirFromFile(gitPath)
		if gitDir == "" {
			continue
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, entry.Name(), gitDir)
		}
		gitDir = filepath.Clean(gitDir)
		if !strings.Contains(gitDir, worktreeMarker) {
			continue
		}
		root := repoRootFromGitFile(
			filepath.Join(dir, entry.Name()), gitPath,
		)
		if root == "" {
			continue
		}
		siblings = append(siblings, siblingInfo{
			root:  root,
			isDir: false,
		})
	}

	// Count worktree and directory siblings.
	var worktreeCount, dirCount int
	var singleDirRoot string
	for _, s := range siblings {
		if s.isDir {
			dirCount++
			singleDirRoot = s.root
		} else {
			worktreeCount++
		}
	}

	// With linked-worktree siblings, all candidates must
	// agree on the same root. Without worktree siblings,
	// accept a single main checkout only if its
	// .git/worktrees/ exists, proving it has (or had)
	// linked worktrees.
	if worktreeCount == 0 {
		if dirCount != 1 {
			return ""
		}
		// Verify the deleted child matches a known worktree
		// entry under .git/worktrees/.
		if !deletedChildIsWorktree(dir, cwd, singleDirRoot) {
			return ""
		}
		return singleDirRoot
	}

	var found string
	for _, s := range siblings {
		if found == "" {
			found = s.root
		} else if found != s.root {
			return ""
		}
	}
	return found
}

// deletedChildIsWorktree checks whether the first missing
// path component (the deleted child under dir) matches an
// entry in the repo's .git/worktrees/ directory.
func deletedChildIsWorktree(
	dir, cwd, repoRoot string,
) bool {
	rel, err := filepath.Rel(dir, cwd)
	if err != nil || rel == "." {
		return false
	}
	child := strings.SplitN(
		filepath.ToSlash(rel), "/", 2,
	)[0]
	if child == "" {
		return false
	}
	wtDir := filepath.Join(repoRoot, ".git", "worktrees")
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == child {
			return true
		}
	}
	return false
}

func repoRootFromGitFile(repoDir, gitFilePath string) string {
	gitDir := readGitDirFromFile(gitFilePath)
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(gitFilePath), gitDir)
	}
	gitDir = filepath.Clean(gitDir)

	commonDir := readCommonDir(gitDir)
	if commonDir != "" {
		if filepath.Base(commonDir) == ".git" {
			return filepath.Dir(commonDir)
		}
	}

	// Fallback for linked worktrees if commondir is missing.
	marker := string(filepath.Separator) + ".git" +
		string(filepath.Separator) + "worktrees" +
		string(filepath.Separator)
	if root, _, found := strings.Cut(gitDir, marker); found {
		if root != "" {
			return filepath.Clean(root)
		}
	}

	return repoDir
}

func readGitDirFromFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const prefix = "gitdir:"
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func readCommonDir(gitDir string) string {
	b, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(b))
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(gitDir, value))
}

func trimBranchSuffix(name, gitBranch string) string {
	branch := strings.TrimSpace(gitBranch)
	if name == "" || branch == "" {
		return name
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")
	branchToken := normalizeBranchToken(branch)
	if branchToken == "" {
		return name
	}
	if isDefaultBranchToken(branchToken) {
		return name
	}

	for _, sep := range []string{"-", "_"} {
		suffix := sep + branchToken
		if strings.HasSuffix(
			strings.ToLower(name),
			strings.ToLower(suffix),
		) {
			base := strings.TrimRight(
				name[:len(name)-len(suffix)], "-_",
			)
			if base != "" {
				return base
			}
		}
	}
	return name
}

func normalizeBranchToken(branch string) string {
	var b strings.Builder
	b.Grow(len(branch))

	lastDash := false
	for _, r := range branch {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		case r == '/', r == '-', r == '_', r == '.', unicode.IsSpace(r):
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	out := strings.Trim(b.String(), "-")
	return out
}

func isDefaultBranchToken(branch string) bool {
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master", "trunk", "develop", "dev":
		return true
	default:
		return false
	}
}

// NeedsProjectReparse checks if a stored project name looks like
// an un-decoded encoded path that should be re-extracted.
func NeedsProjectReparse(project string) bool {
	bad := []string{
		"_Users", "_home", "_private", "_tmp", "_var",
	}
	for _, prefix := range bad {
		if strings.HasPrefix(project, prefix) {
			return true
		}
	}
	return strings.Contains(project, "_var_folders_") ||
		strings.Contains(project, "_var_tmp_")
}
