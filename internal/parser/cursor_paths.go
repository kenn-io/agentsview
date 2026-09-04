package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// CursorResolveMode selects the policy used by the shared Cursor filesystem
// resolver. Passive discovery treats protected and automount refusal as an
// incomplete search; explicit resume may bypass only those policy refusals.
type CursorResolveMode uint8

const (
	CursorResolvePassiveDiscovery CursorResolveMode = iota
	CursorResolveExplicitResume
)

var cursorReadDir = os.ReadDir

// cursorPassiveResolutionTTL bounds reuse of passive filesystem resolutions
// across discovery passes and changed-path classifications. Discovery and
// watch batches re-resolve the same encoded project directories continuously,
// and each resolution walks real directories from the filesystem root, so a
// short reuse window removes almost all of that steady-state work while a
// created, deleted, or renamed workspace is still observed within one TTL.
const (
	cursorPassiveResolutionTTL        = 30 * time.Second
	cursorPassiveResolutionMaxEntries = 4096
)

// cursorPassiveResolutionsEnabled disables the cross-pass cache in test
// binaries: lifecycle tests create and delete real workspaces between sync
// passes and must observe the live filesystem. The cache's own tests opt back
// in explicitly.
var cursorPassiveResolutionsEnabled = !testing.Testing()

type cursorPassiveResolutionEntry struct {
	resolution SourceCwdResolution
	expiresAt  time.Time
}

var cursorPassiveResolutions = struct {
	sync.Mutex
	entries map[string]cursorPassiveResolutionEntry
}{entries: make(map[string]cursorPassiveResolutionEntry)}

func cachedCursorPassiveResolution(dirName string) (SourceCwdResolution, bool) {
	cursorPassiveResolutions.Lock()
	defer cursorPassiveResolutions.Unlock()
	entry, ok := cursorPassiveResolutions.entries[dirName]
	if !ok || time.Now().After(entry.expiresAt) {
		return SourceCwdResolution{}, false
	}
	return entry.resolution, true
}

func storeCursorPassiveResolution(dirName string, resolution SourceCwdResolution) {
	cursorPassiveResolutions.Lock()
	defer cursorPassiveResolutions.Unlock()
	if len(cursorPassiveResolutions.entries) >= cursorPassiveResolutionMaxEntries {
		cursorPassiveResolutions.entries = make(
			map[string]cursorPassiveResolutionEntry,
		)
	}
	cursorPassiveResolutions.entries[dirName] = cursorPassiveResolutionEntry{
		resolution: resolution,
		expiresAt:  time.Now().Add(cursorPassiveResolutionTTL),
	}
}

func resetCursorPassiveResolutions() {
	cursorPassiveResolutions.Lock()
	defer cursorPassiveResolutions.Unlock()
	cursorPassiveResolutions.entries = make(map[string]cursorPassiveResolutionEntry)
}

// ResolveCursorWorkspaceDir resolves a Cursor project directory against the
// filesystem and returns a path only for one unique passive match.
func ResolveCursorWorkspaceDir(dirName string) (string, bool) {
	matches, incomplete := resolveCursorWorkspaceDirMatchesIn("", dirName, "", 2)
	if incomplete {
		return "", false
	}
	return cursorUniqueWorkspaceMatch(matches)
}

// ResolveCursorWorkspaceDirIn is the test-rooted form of ResolveCursorWorkspaceDir.
func ResolveCursorWorkspaceDirIn(root, dirName string) (string, bool) {
	matches, incomplete := resolveCursorWorkspaceDirMatchesIn(root, dirName, "", 2)
	if incomplete {
		return "", false
	}
	return cursorUniqueWorkspaceMatch(matches)
}

// ResolveCursorWorkspaceDirResolution resolves a source directory using the
// named policy and returns the complete source Cwd state.
func ResolveCursorWorkspaceDirResolution(
	root, dirName, hint string, mode CursorResolveMode,
) SourceCwdResolution {
	return resolveCursorWorkspaceDirResolution(root, dirName, hint, mode)
}

// ResolveCursorWorkspaceDirPassive is the parser entry point for discovery.
func ResolveCursorWorkspaceDirPassive(dirName string) SourceCwdResolution {
	return resolveCursorWorkspaceDirResolution(
		"", dirName, "", CursorResolvePassiveDiscovery,
	)
}

// ResolveCursorWorkspaceDirExplicit is the parser entry point for resume.
func ResolveCursorWorkspaceDirExplicit(
	root, dirName, hint string,
) SourceCwdResolution {
	return resolveCursorWorkspaceDirResolution(
		root, dirName, hint, CursorResolveExplicitResume,
	)
}

func cursorUniqueWorkspaceMatch(matches []string) (string, bool) {
	switch len(matches) {
	case 0:
		return "", false
	case 1:
		return matches[0], false
	default:
		return matches[0], true
	}
}

// ResolveCursorWorkspaceDirHint resolves a workspace, requiring the selected
// match to contain hint when a hint is supplied.
func ResolveCursorWorkspaceDirHint(root, dirName, hint string) string {
	resolution := resolveCursorWorkspaceDirResolution(
		root, dirName, hint, CursorResolveExplicitResume,
	)
	if resolution.State != SourceCwdResolved {
		return ""
	}
	return resolution.Path
}

// ResolveCursorWorkspaceDirMatchesIn returns up to limit filesystem matches.
func ResolveCursorWorkspaceDirMatchesIn(root, dirName, hint string, limit int) []string {
	matches, incomplete := resolveCursorWorkspaceDirMatchesIn(
		root, dirName, hint, limit,
	)
	if incomplete {
		return nil
	}
	return matches
}

func resolveCursorWorkspaceDirResolution(
	root, dirName, hint string, mode CursorResolveMode,
) SourceCwdResolution {
	dirName = strings.TrimSpace(dirName)
	if dirName == "" {
		return SourceCwdResolution{State: SourceCwdNone}
	}
	// Only the production passive shape is cached: rooted calls are test- or
	// probe-scoped, and explicit resume must observe the live filesystem.
	cacheable := cursorPassiveResolutionsEnabled && root == "" &&
		hint == "" && mode == CursorResolvePassiveDiscovery
	if cacheable {
		if resolution, ok := cachedCursorPassiveResolution(dirName); ok {
			return resolution
		}
	}
	resolution := resolveCursorWorkspaceDirResolutionUncached(
		root, dirName, hint, mode,
	)
	if cacheable {
		storeCursorPassiveResolution(dirName, resolution)
	}
	return resolution
}

func resolveCursorWorkspaceDirResolutionUncached(
	root, dirName, hint string, mode CursorResolveMode,
) SourceCwdResolution {
	hint = normalizeCursorDir(hint)
	matches, incomplete := resolveCursorWorkspaceDirFromRootMatches(
		root, dirName, hint, 2, mode,
	)
	// Incompleteness dominates every other classification: an unreadable or
	// uncanonicalizable branch may hide the true workspace or double-count
	// one workspace as two matches, and Ambiguous/None would clear a
	// preserved Cwd that Unavailable keeps.
	if incomplete {
		return SourceCwdResolution{State: SourceCwdUnavailable}
	}
	if mode == CursorResolveExplicitResume && hint != "" && len(matches) > 1 {
		var hinted string
		for _, match := range matches {
			if cursorPathContainsHint(match, hint) {
				if hinted != "" {
					return SourceCwdResolution{State: SourceCwdAmbiguous}
				}
				hinted = match
			}
		}
		if hinted != "" {
			return SourceCwdResolution{
				State: SourceCwdResolved,
				Path:  hinted,
			}
		}
		return SourceCwdResolution{State: SourceCwdAmbiguous}
	}
	if len(matches) > 1 {
		return SourceCwdResolution{State: SourceCwdAmbiguous}
	}
	switch len(matches) {
	case 0:
		return SourceCwdResolution{State: SourceCwdNone}
	case 1:
		return SourceCwdResolution{
			State: SourceCwdResolved,
			Path:  matches[0],
		}
	default:
		return SourceCwdResolution{State: SourceCwdAmbiguous}
	}
}

func resolveCursorWorkspaceDirMatchesIn(
	root, dirName, hint string, limit int,
) ([]string, bool) {
	return resolveCursorWorkspaceDirFromRootMatches(
		root, dirName, hint, limit, CursorResolvePassiveDiscovery,
	)
}

func resolveCursorWorkspaceDirFromRootMatches(
	root, dirName, hint string, limit int, mode CursorResolveMode,
) ([]string, bool) {
	tokens := cursorEncodedTokens(dirName)
	if len(tokens) == 0 {
		return nil, false
	}
	current := root
	if root == "" && runtime.GOOS == "windows" && len(tokens[0]) == 1 && isCursorDriveComponent(tokens[0]) {
		current = strings.ToUpper(tokens[0]) + ":" + string(filepath.Separator)
		tokens = tokens[1:]
	}
	if current == "" {
		current = string(filepath.Separator)
	}
	rootInfo, err := osStat(current)
	if err != nil {
		return nil, true
	}
	if !rootInfo.IsDir() {
		return nil, false
	}
	resolvedRoot := canonicalCursorDir(current)
	if resolvedRoot == "" {
		return nil, true
	}
	if len(tokens) == 0 {
		return []string{normalizeCursorDir(current)}, false
	}
	var matches []string
	incomplete := collectCursorPathMatches(current, tokens, cursorWalkQuery{
		resolvedRoot: resolvedRoot,
		hint:         hint,
		limit:        limit,
		mode:         mode,
		// Windows 8.3 aliases always mangle a component with a '~N' tail, so
		// an encoded name without '~' can never need the per-entry short-path
		// syscall; a name already valid as 8.3 matches through its long form.
		shortAliases: strings.ContainsRune(dirName, '~'),
	}, &matches)
	for i := range matches {
		matches[i] = normalizeCursorDir(matches[i])
	}
	canonical := matches[:0]
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if match == "" {
			incomplete = true
			continue
		}
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		canonical = append(canonical, match)
	}
	return canonical, incomplete
}

type cursorPathMatch struct {
	name     string
	path     string
	consumed int
	hinted   bool
}

// cursorWalkQuery carries the per-resolution walk parameters that stay fixed
// while collectCursorPathMatches recurses.
type cursorWalkQuery struct {
	resolvedRoot string
	hint         string
	limit        int
	mode         CursorResolveMode
	shortAliases bool
}

func collectCursorPathMatches(
	dir string, tokens []string, q cursorWalkQuery, matches *[]string,
) bool {
	if q.limit > 0 && len(*matches) >= q.limit {
		return false
	}
	if len(tokens) == 0 {
		info, err := osStat(dir)
		if err != nil {
			return true
		}
		if !info.IsDir() {
			return true
		}
		canonical := canonicalCursorDir(dir)
		if canonical == "" || !isContainedIn(canonical, q.resolvedRoot) {
			return false
		}
		for _, match := range *matches {
			if canonicalCursorDir(match) == canonical {
				return false
			}
		}
		*matches = append(*matches, normalizeCursorDir(dir))
		return false
	}
	components, incomplete := matchCursorPathComponents(dir, tokens, q)
	for _, match := range components {
		if collectCursorPathMatches(
			match.path, tokens[match.consumed:], q, matches,
		) {
			incomplete = true
		}
		if q.limit > 0 && len(*matches) >= q.limit {
			return incomplete
		}
	}
	return incomplete
}

func matchCursorPathComponents(
	dir string, tokens []string, q cursorWalkQuery,
) ([]cursorPathMatch, bool) {
	entries, err := cursorReadDir(dir)
	if err != nil {
		return nil, true
	}
	matches := make([]cursorPathMatch, 0, len(entries))
	incomplete := false
	for _, entry := range entries {
		fullPath := filepath.Join(dir, entry.Name())
		var candidate []string
		matchedPath := fullPath
		for _, alias := range cursorComponentTokenAliases(
			fullPath, q.shortAliases,
		) {
			if len(alias.tokens) > 0 && len(alias.tokens) <= len(tokens) &&
				cursorTokenPrefixMatch(tokens, alias.tokens) {
				candidate = alias.tokens
				matchedPath = alias.path
				break
			}
		}
		if candidate == nil {
			continue
		}
		if !cursorWorkspaceProbeAllowed(fullPath, q.mode) {
			incomplete = true
			continue
		}
		info, statErr := osStat(fullPath)
		if statErr != nil {
			incomplete = true
			continue
		}
		if !info.IsDir() {
			continue
		}
		resolved := canonicalCursorDir(fullPath)
		if resolved == "" {
			incomplete = true
			continue
		}
		if !isContainedIn(resolved, q.resolvedRoot) {
			continue
		}
		matches = append(matches, cursorPathMatch{
			name:     entry.Name(),
			path:     matchedPath,
			consumed: len(candidate),
			hinted:   cursorPathContainsHint(fullPath, q.hint),
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].hinted != matches[j].hinted {
			return matches[i].hinted
		}
		if matches[i].consumed != matches[j].consumed {
			return matches[i].consumed > matches[j].consumed
		}
		return matches[i].name < matches[j].name
	})
	return matches, incomplete
}

func cursorWorkspaceProbeAllowed(path string, mode CursorResolveMode) bool {
	if mode == CursorResolveExplicitResume {
		// Explicit resume has user intent to cross the passive protected-path
		// and automount policy boundary; directory type, canonicalization,
		// containment, and match cardinality remain enforced below.
		return true
	}
	return probeGitRootForCwd(path)
}

func cursorPathContainsHint(path, hint string) bool {
	if path == "" || hint == "" {
		return false
	}
	// Compare canonical identities so Windows short aliases do not block hint selection.
	if canonicalPath := canonicalCursorDir(path); canonicalPath != "" {
		path = canonicalPath
	}
	if canonicalHint := canonicalCursorDir(hint); canonicalHint != "" {
		hint = canonicalHint
	}
	rel, err := filepath.Rel(path, hint)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func cursorTokenPrefixMatch(tokens, candidate []string) bool {
	for i := range candidate {
		if runtime.GOOS == "windows" {
			if !strings.EqualFold(tokens[i], candidate[i]) {
				return false
			}
			continue
		}
		if tokens[i] != candidate[i] {
			return false
		}
	}
	return true
}

func cursorEncodedTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '-' })
}
func cursorComponentTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '.' || r == '_' })
}

func normalizeCursorDir(path string) string {
	if !isDir(path) {
		return ""
	}
	clean := filepath.Clean(path)
	resolved := canonicalCursorDir(clean)
	if resolved == "" {
		return clean
	}
	if runtime.GOOS == "windows" && !cursorPathContainsSymlink(clean) {
		return clean
	}
	return resolved
}

func canonicalCursorDir(path string) string {
	if !isDir(path) {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil || !isDir(resolved) {
		return ""
	}
	resolved = filepath.Clean(resolved)
	if runtime.GOOS == "darwin" && strings.HasPrefix(resolved, "/private/") {
		publicPath := filepath.Clean(strings.TrimPrefix(resolved, "/private"))
		if isDir(publicPath) {
			resolved = publicPath
		}
	}
	return resolved
}

func cursorPathContainsSymlink(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil &&
			info.Mode()&os.ModeSymlink != 0 {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func isDir(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := osStat(path)
	return err == nil && info != nil && info.IsDir()
}

func isCursorDriveComponent(part string) bool {
	return len(part) == 1 && ((part[0] >= 'A' && part[0] <= 'Z') || (part[0] >= 'a' && part[0] <= 'z'))
}

// cursorTranscriptLocation is what a transcript path encodes in its layout:
// the project directory, the session's raw ID, and the parent's raw ID when
// the file sits in a parent session's subagents directory.
type cursorTranscriptLocation struct {
	ProjectDir  string
	RawID       string
	ParentRawID string
}

const cursorSubagentsDirName = "subagents"

// ParseCursorTranscriptRelPath validates a path relative to a
// Cursor projects dir and returns the encoded project directory
// name for recognized transcript layouts.
func ParseCursorTranscriptRelPath(rel string) (string, bool) {
	loc, ok := parseCursorTranscriptRel(rel)
	return loc.ProjectDir, ok
}

// parseCursorTranscriptRel recognizes the layouts Cursor writes under
// <project>/agent-transcripts/:
//
//	flat:     <id>.{txt,jsonl}
//	nested:   <id>/<id>.{txt,jsonl}
//	subagent: <parent>/subagents/<id>.{txt,jsonl}
//
// Cursor CLI nests subagents exactly one level deep.
func parseCursorTranscriptRel(rel string) (cursorTranscriptLocation, bool) {
	rel = filepath.Clean(rel)
	return parseCursorTranscriptRelParts(
		strings.Split(rel, string(filepath.Separator)),
	)
}

func parseCursorTranscriptRelParts(parts []string) (cursorTranscriptLocation, bool) {
	if len(parts) < 3 || len(parts) > 5 || parts[1] != "agent-transcripts" {
		return cursorTranscriptLocation{}, false
	}
	if parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return cursorTranscriptLocation{}, false
	}
	name := parts[len(parts)-1]
	if !IsCursorTranscriptExt(name) {
		return cursorTranscriptLocation{}, false
	}
	loc := cursorTranscriptLocation{
		ProjectDir: parts[0],
		RawID:      strings.TrimSuffix(name, filepath.Ext(name)),
	}
	switch len(parts) {
	case 4:
		if parts[2] != loc.RawID {
			return cursorTranscriptLocation{}, false
		}
	case 5:
		if parts[3] != cursorSubagentsDirName || !IsValidSessionID(parts[2]) ||
			parts[2] == loc.RawID {
			return cursorTranscriptLocation{}, false
		}
		loc.ParentRawID = parts[2]
	}
	return loc, true
}

func cursorTranscriptLocationInRoot(root, path string) (cursorTranscriptLocation, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return cursorTranscriptLocation{}, false
	}
	return parseCursorTranscriptRel(rel)
}

// cursorTranscriptLocationFromPath needs no projects root: parse receives
// paths from discovery, watcher events, explicit finds, and S3
// materializations that keep the object layout under a temporary root. Every
// agent-transcripts component is tried from the end, so a session directory
// that happens to carry that name does not mask the real layout marker.
func cursorTranscriptLocationFromPath(path string) (cursorTranscriptLocation, bool) {
	parts := splitCleanPath(path)
	for i := len(parts) - 1; i >= 1; i-- {
		if parts[i] != "agent-transcripts" {
			continue
		}
		if loc, ok := parseCursorTranscriptRelParts(parts[i-1:]); ok {
			return loc, true
		}
	}
	return cursorTranscriptLocation{}, false
}

// cursorTranscriptCandidates orders a session's own nested and flat layouts,
// .jsonl before .txt, ahead of the subagent directory named by loc. A
// top-level copy therefore wins a stem tie regardless of extension, so the
// changed-path route never has to search other sessions' directories.
func cursorTranscriptCandidates(
	transcriptsDir string, loc cursorTranscriptLocation,
) []string {
	exts := []string{".jsonl", ".txt"}
	candidates := make([]string, 0, 6)
	for _, ext := range exts {
		target := loc.RawID + ext
		candidates = append(candidates,
			filepath.Join(transcriptsDir, loc.RawID, target),
			filepath.Join(transcriptsDir, target),
		)
	}
	if loc.ParentRawID != "" {
		for _, ext := range exts {
			candidates = append(candidates, filepath.Join(
				transcriptsDir, loc.ParentRawID, cursorSubagentsDirName, loc.RawID+ext,
			))
		}
	}
	return candidates
}
