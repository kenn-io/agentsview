package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// JSONLSource is the in-memory payload JSONLSourceSet stores in SourceRef.
type JSONLSource struct {
	Root    string
	Path    string
	RelPath string
}

// JSONLSourceSetOptions configures the reusable JSONL source helper.
type JSONLSourceSetOptions struct {
	// Recursive enables traversal and changed-path classification below each
	// configured root. When false, only direct child files are sources.
	Recursive bool
	// Extensions defaults to .jsonl. Matching is case-sensitive to mirror
	// legacy parser discovery.
	Extensions []string
	// Hash includes a full content hash in SourceFingerprint. Providers should
	// leave this false unless size/mtime freshness is insufficient.
	Hash bool
	// FollowSymlinkDirs treats symlinks to directories as directories while
	// discovering recursive roots. Providers should enable it only when legacy
	// discovery followed symlinked session directories; targets may be outside
	// the configured root, so provider IncludePath filters should constrain the
	// accepted source shape when that matters.
	FollowSymlinkDirs bool
	// FollowSymlinkFiles treats symlinks to regular files as sources. Providers
	// should enable it when legacy discovery accepted matching symlinked files
	// and the parser reads through the symlink target.
	FollowSymlinkFiles bool
	// DescendPath is a directory predicate for recursive discovery. It is also
	// applied to source ancestors during direct source classification so
	// changed-path events cannot accept paths discovery would have pruned.
	DescendPath func(root, path string) bool
	// IncludePath is a path-only source predicate. It runs before Include and is
	// also used for deleted/renamed changed paths where os.FileInfo is
	// unavailable.
	IncludePath func(root, path string) bool
	// Include is a source predicate for existing files. It is not called for
	// deleted/renamed changed paths.
	Include func(path string, info os.FileInfo) bool
	// Key must be stable across process restarts and unique within a provider
	// when every physical source should be parsed. If duplicates exist,
	// discovery keeps the first configured root/traversal result.
	Key func(root, path string) string
	// DisplayPath is human-readable. When FingerprintKey is not set, it also
	// becomes the persisted freshness key.
	DisplayPath func(root, path string) string
	// FingerprintKey is the persisted lookup and freshness identity. Override it
	// when DisplayPath is not the stable value that should survive a provider
	// migration.
	FingerprintKey func(root, path string) string
	// ProjectHint is display metadata only.
	ProjectHint func(root, path string) string
	// SessionIDFromPath returns the raw session ID used by FindSource fallback
	// lookups. It should not include the provider ID prefix.
	SessionIDFromPath func(root, path string) string
	// LookupIDValid reports whether a raw session ID is shaped like an ID this
	// provider could resolve, gating the FindSource discovery fallback. It
	// defaults to IsValidSessionID. Providers whose SessionIDFromPath emits
	// composite IDs (for example subagent IDs containing separators that
	// IsValidSessionID rejects) supply their own validator so those lookups are
	// not dropped before the comparison loop.
	LookupIDValid func(rawID string) bool
	// RawSessionIDForLookup normalizes a raw session ID before the FindSource
	// discovery comparison. Providers whose stored IDs carry a suffix the
	// discovered filename stem lacks (for example iFlow subagent IDs) reduce it
	// to the base ID here so the comparison still matches. It runs after
	// providerFindRequestWithRawSessionID and before the LookupIDValid gate.
	RawSessionIDForLookup func(rawID string) string
	// RawSessionIDSourceFiles reconstructs candidate file paths from a raw
	// session ID for providers whose IDs encode the on-disk layout rather than
	// being a discoverable filename stem. FindSource resolves each candidate
	// through the same path->SourceRef machinery as a stored path and returns
	// the first that exists, before falling through to the discovery scan. The
	// closure iterates the provided roots itself and applies its own ID
	// validation.
	RawSessionIDSourceFiles func(roots []string, rawID string) []string
	// StoredPathFallbackRoot resolves the configured root for a stored source
	// path that is not under any current root, returning false to decline. It
	// lets a provider honor a DB-recorded file_path whose root was removed or
	// was a custom location by reconstructing the implicit root so the path
	// still resolves to a SourceRef. FindSource consults it after the in-root
	// path lookup misses.
	StoredPathFallbackRoot func(storedPath string) (string, bool)
	// ParseFile parses one discovered source file into zero or more sessions
	// plus the IDs of any sessions to exclude. Empty results with no exclusions
	// is a clean no-session. It is what makes JSONLSourceSet a full SourceSet
	// (its Parse method); leave it nil for discovery-only embedders that supply
	// their own Parse. ctx and req.Machine are supplied by sourceSetProvider.
	ParseFile jsonlParseFileFunc
	// ForceReplace marks every non-empty parse outcome from ParseFile as a full
	// replacement of the source's existing sessions, for providers whose
	// transcripts are rewritten wholesale rather than appended.
	ForceReplace bool
}

// JSONLSourceSet discovers, watches, locates, and fingerprints JSONL-like
// transcript files. With a ParseFile option it is also a full SourceSet;
// without one it is a discovery helper that providers compose as a named field
// and forward the methods they support. Missing or unreadable roots and
// subdirectories are treated as empty, matching legacy discovery's lenient
// local-filesystem behavior.
type JSONLSourceSet struct {
	provider   AgentType
	roots      []string
	options    JSONLSourceSetOptions
	extensions []string
}

// newJSONLSourceSet builds a JSONL source set for a provider's roots from
// functional options. Every option has a zero-value default, so callers state
// only what differs.
func newJSONLSourceSet(
	provider AgentType,
	roots []string,
	opts ...jsonlOption,
) JSONLSourceSet {
	var options JSONLSourceSetOptions
	for _, opt := range opts {
		opt(&options)
	}
	return jsonlSourceSetFromOptions(provider, roots, options)
}

// jsonlSourceSetFromOptions is the shared constructor used by newJSONLSourceSet
// and newDirectoryJSONLSourceSet once options have been resolved.
func jsonlSourceSetFromOptions(
	provider AgentType,
	roots []string,
	options JSONLSourceSetOptions,
) JSONLSourceSet {
	return JSONLSourceSet{
		provider:   provider,
		roots:      cleanJSONLRoots(roots),
		options:    options,
		extensions: normalizeJSONLExtensions(options.Extensions),
	}
}

// Discover returns stable, deduped source references for configured roots.
func (s JSONLSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := s.discoverDir(ctx, root, root, &sources, seen); err != nil {
			return nil, err
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// WatchPlan returns one watch root for each configured JSONL root.
func (s JSONLSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	globs := s.includeGlobs()
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    s.options.Recursive,
			IncludeGlobs: append([]string(nil), globs...),
			DebounceKey:  string(s.provider) + ":jsonl:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

// SourcesForChangedPath maps a filesystem event path back to JSONL sources.
func (s JSONLSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, ok, err := s.sourceForPath(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	if !ok {
		if !jsonlMissingPathFallbackAllowed(req) {
			return nil, nil
		}
		source, ok, err = s.sourceForMissingPath(ctx, req.Path)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
	}
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		src := source.Opaque.(JSONLSource)
		if !samePath(root, src.Root) {
			return nil, nil
		}
	}
	return []SourceRef{source}, nil
}

// FindSource resolves persisted source hints or a raw filename-stem session ID.
func (s JSONLSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, stored := range []string{req.StoredFilePath, req.FingerprintKey} {
		if stored == "" {
			continue
		}
		source, ok, err := s.sourceForPath(ctx, stored)
		if err != nil {
			return SourceRef{}, false, err
		}
		if ok {
			return source, true, nil
		}
		if s.options.StoredPathFallbackRoot != nil {
			if root, ok := s.options.StoredPathFallbackRoot(stored); ok {
				if source, ok := s.sourceRefFromPath(
					root, filepath.Clean(stored),
				); ok {
					return source, true, nil
				}
			}
		}
	}
	if s.options.RawSessionIDForLookup != nil && req.RawSessionID != "" {
		req.RawSessionID = s.options.RawSessionIDForLookup(req.RawSessionID)
	}
	if req.RawSessionID != "" && s.options.RawSessionIDSourceFiles != nil {
		for _, candidate := range s.options.RawSessionIDSourceFiles(
			s.roots, req.RawSessionID,
		) {
			source, ok, err := s.sourceForPath(ctx, candidate)
			if err != nil {
				return SourceRef{}, false, err
			}
			if ok {
				return source, true, nil
			}
		}
	}
	validRawID := req.RawSessionID != "" && s.lookupIDValid(req.RawSessionID)
	if req.FingerprintKey == "" && !validRawID {
		return SourceRef{}, false, nil
	}
	sources, err := s.Discover(ctx)
	if err != nil {
		return SourceRef{}, false, err
	}
	for _, source := range sources {
		if req.FingerprintKey != "" && source.FingerprintKey == req.FingerprintKey {
			return source, true, nil
		}
		if !validRawID {
			continue
		}
		src := source.Opaque.(JSONLSource)
		if s.sessionID(src.Root, src.Path) == req.RawSessionID {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// Fingerprint returns the filesystem freshness identity for a JSONL source.
func (s JSONLSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok, err := s.pathFromSource(ctx, source)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("jsonl source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	inode, device := sourceFileIdentity(info)
	fingerprint := SourceFingerprint{
		Key: firstNonEmptyJSONLString(
			source.FingerprintKey,
			source.Key,
			path,
		),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Inode:   inode,
		Device:  device,
	}
	if s.options.Hash {
		hash, err := hashJSONLSourceFile(path)
		if err != nil {
			return SourceFingerprint{}, err
		}
		fingerprint.Hash = hash
	}
	return fingerprint, nil
}

// Parse resolves the request's source to a file and parses it via the ParseFile
// option, making JSONLSourceSet a full SourceSet. It mirrors the single-file
// base's parse semantics: empty results with no exclusions is a clean
// no-session skip. sourceSetProvider resolves req.Machine before calling in.
func (s JSONLSourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	if s.options.ParseFile == nil {
		return ParseOutcome{}, fmt.Errorf(
			"%s: JSONLSourceSet has no ParseFile configured", s.provider,
		)
	}
	path, ok, err := s.pathFromSource(ctx, req.Source)
	if err != nil {
		return ParseOutcome{}, err
	}
	if !ok {
		return ParseOutcome{}, fmt.Errorf(
			"%s source path unavailable", s.provider,
		)
	}
	results, excluded, err := s.options.ParseFile(ctx, path, req)
	if err != nil {
		return ParseOutcome{}, err
	}
	if len(results) == 0 && len(excluded) == 0 {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for i := range results {
		out = append(out, ParseResultOutcome{
			Result:      results[i],
			DataVersion: DataVersionCurrent,
		})
	}
	return ParseOutcome{
		Results:            out,
		ExcludedSessionIDs: excluded,
		ResultSetComplete:  true,
		ForceReplace:       s.options.ForceReplace,
	}, nil
}

var (
	_ SourceSet = JSONLSourceSet{}
	_ SourceSet = DirectoryJSONLSourceSet{}
)

func (s JSONLSourceSet) discoverDir(
	ctx context.Context,
	root string,
	dir string,
	sources *[]SourceRef,
	seen map[string]struct{},
) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(dir, entry.Name())
		if s.shouldDescend(entry, dir) {
			if s.options.Recursive && s.descendPathIncluded(root, path) {
				if err := s.discoverDir(
					ctx, root, path, sources, seen,
				); err != nil {
					return err
				}
			}
			continue
		}
		info, err := s.sourceFileInfo(entry, path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		source, ok := s.sourceRef(root, path, info)
		if !ok {
			continue
		}
		addJSONLSource(source, sources, seen)
	}
	return nil
}

func (s JSONLSourceSet) shouldDescend(entry os.DirEntry, dir string) bool {
	if entry.IsDir() {
		return true
	}
	return s.options.FollowSymlinkDirs && isDirOrSymlink(entry, dir)
}

func (s JSONLSourceSet) sourceFileInfo(
	entry os.DirEntry,
	path string,
) (os.FileInfo, error) {
	info, err := entry.Info()
	if err != nil {
		return nil, err
	}
	if !s.options.FollowSymlinkFiles || info.Mode()&os.ModeSymlink == 0 {
		return info, nil
	}
	return os.Stat(path)
}

func (s JSONLSourceSet) sourceForPath(
	ctx context.Context,
	path string,
) (SourceRef, bool, error) {
	path = filepath.Clean(path)
	info, err := s.sourcePathInfo(path)
	if err != nil || !info.Mode().IsRegular() {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		if !s.pathAllowedByRoot(root, path) {
			continue
		}
		if !s.sourcePathAllowedByDescendPath(root, path) {
			continue
		}
		if !s.pathIncluded(root, path) {
			continue
		}
		source, ok := s.sourceRef(root, path, info)
		if !ok {
			return SourceRef{}, false, nil
		}
		return s.discoveredSourceForCandidate(ctx, source)
	}
	return SourceRef{}, false, nil
}

func (s JSONLSourceSet) sourcePathInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !s.options.FollowSymlinkFiles || info.Mode()&os.ModeSymlink == 0 {
		return info, nil
	}
	return os.Stat(path)
}

func (s JSONLSourceSet) sourceForMissingPath(
	ctx context.Context,
	path string,
) (SourceRef, bool, error) {
	path = filepath.Clean(path)
	for _, root := range s.roots {
		if !s.pathAllowedByRoot(root, path) {
			continue
		}
		if !s.sourcePathAllowedByDescendPath(root, path) {
			continue
		}
		if !s.matchesExtension(path) || !s.pathIncluded(root, path) {
			continue
		}
		source, ok := s.sourceRefFromPath(root, path)
		if !ok {
			return SourceRef{}, false, nil
		}
		return s.discoveredSourceForCandidate(ctx, source)
	}
	return SourceRef{}, false, nil
}

func jsonlMissingPathFallbackAllowed(req ChangedPathRequest) bool {
	if req.Path == "" {
		return false
	}
	if _, err := os.Lstat(req.Path); err == nil {
		return false
	} else if os.IsNotExist(err) {
		return true
	}
	switch strings.ToLower(req.EventKind) {
	case "remove", "removed", "delete", "deleted", "rename", "renamed":
		return true
	default:
		return false
	}
}

func (s JSONLSourceSet) pathAllowedByRoot(root, path string) bool {
	if s.options.Recursive {
		return pathIsUnderRoot(path, root)
	}
	return samePath(filepath.Dir(path), root)
}

func (s JSONLSourceSet) sourceRef(
	root string,
	path string,
	info os.FileInfo,
) (SourceRef, bool) {
	if !s.matchesExtension(path) {
		return SourceRef{}, false
	}
	if !s.pathIncluded(root, path) {
		return SourceRef{}, false
	}
	if s.options.Include != nil && !s.options.Include(path, info) {
		return SourceRef{}, false
	}
	return s.sourceRefFromPath(root, path)
}

func (s JSONLSourceSet) sourceRefFromPath(
	root string,
	path string,
) (SourceRef, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return SourceRef{}, false
	}
	displayPath := firstNonEmptyJSONLString(
		callPathFunc(s.options.DisplayPath, root, path),
		path,
	)
	fingerprintKey := firstNonEmptyJSONLString(
		callPathFunc(s.options.FingerprintKey, root, path),
		displayPath,
	)
	key := firstNonEmptyJSONLString(
		callPathFunc(s.options.Key, root, path),
		displayPath,
	)
	return SourceRef{
		Provider:       s.provider,
		Key:            key,
		DisplayPath:    displayPath,
		FingerprintKey: fingerprintKey,
		ProjectHint:    callPathFunc(s.options.ProjectHint, root, path),
		Opaque: JSONLSource{
			Root:    root,
			Path:    path,
			RelPath: rel,
		},
	}, true
}

func (s JSONLSourceSet) discoveredSourceForCandidate(
	ctx context.Context,
	candidate SourceRef,
) (SourceRef, bool, error) {
	discovered, err := s.Discover(ctx)
	if err != nil {
		return SourceRef{}, false, err
	}
	for _, source := range discovered {
		if source.Provider == candidate.Provider && source.Key == candidate.Key {
			return source, true, nil
		}
	}
	return candidate, true, nil
}

func (s JSONLSourceSet) pathIncluded(root, path string) bool {
	return s.options.IncludePath == nil || s.options.IncludePath(root, path)
}

func (s JSONLSourceSet) descendPathIncluded(root, path string) bool {
	return s.options.DescendPath == nil || s.options.DescendPath(root, path)
}

func (s JSONLSourceSet) sourcePathAllowedByDescendPath(root, path string) bool {
	if s.options.DescendPath == nil {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	dir := filepath.Dir(rel)
	if dir == "." {
		return true
	}
	current := root
	for part := range strings.SplitSeq(dir, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return false
		}
		current = filepath.Join(current, part)
		if !s.descendPathIncluded(root, current) {
			return false
		}
	}
	return true
}

func (s JSONLSourceSet) matchesExtension(path string) bool {
	ext := filepath.Ext(path)
	return slices.Contains(s.extensions, ext)
}

func (s JSONLSourceSet) includeGlobs() []string {
	globs := make([]string, 0, len(s.extensions))
	for _, ext := range s.extensions {
		globs = append(globs, "*"+ext)
	}
	return globs
}

func (s JSONLSourceSet) sessionID(root, path string) string {
	if s.options.SessionIDFromPath != nil {
		return s.options.SessionIDFromPath(root, path)
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func (s JSONLSourceSet) lookupIDValid(rawID string) bool {
	if s.options.LookupIDValid != nil {
		return s.options.LookupIDValid(rawID)
	}
	return IsValidSessionID(rawID)
}

func (s JSONLSourceSet) pathFromSource(
	ctx context.Context,
	source SourceRef,
) (string, bool, error) {
	switch src := source.Opaque.(type) {
	case JSONLSource:
		if src.Path != "" {
			return src.Path, true, nil
		}
	case *JSONLSource:
		if src != nil && src.Path != "" {
			return src.Path, true, nil
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if ref, ok, err := s.sourceForPath(ctx, candidate); err != nil {
			return "", false, err
		} else if ok {
			src := ref.Opaque.(JSONLSource)
			return src.Path, true, nil
		}
	}
	return "", false, nil
}

func cleanJSONLRoots(roots []string) []string {
	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		cleaned = append(cleaned, filepath.Clean(root))
	}
	return cleaned
}

func normalizeJSONLExtensions(exts []string) []string {
	if len(exts) == 0 {
		return []string{".jsonl"}
	}
	seen := make(map[string]struct{}, len(exts))
	normalized := make([]string, 0, len(exts))
	for _, ext := range exts {
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}
		normalized = append(normalized, ext)
	}
	if len(normalized) == 0 {
		return []string{".jsonl"}
	}
	sort.Strings(normalized)
	return normalized
}

func addJSONLSource(
	source SourceRef,
	sources *[]SourceRef,
	seen map[string]struct{},
) bool {
	key := string(source.Provider) + "\x00" + source.Key
	if _, ok := seen[key]; ok {
		return false
	}
	seen[key] = struct{}{}
	*sources = append(*sources, source)
	return true
}

func sortJSONLSources(sources []SourceRef) {
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].DisplayPath != sources[j].DisplayPath {
			return sources[i].DisplayPath < sources[j].DisplayPath
		}
		return sources[i].Key < sources[j].Key
	})
}

func callPathFunc(fn func(root, path string) string, root, path string) string {
	if fn == nil {
		return ""
	}
	return fn(root, path)
}

func pathIsUnderRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func firstNonEmptyJSONLString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func hashJSONLSourceFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
