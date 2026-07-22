package parser

import (
	"context"
	"crypto/sha256"
	"errors"
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
	// FingerprintKey is the persisted lookup and freshness identity. When unset,
	// it falls back to the display path (the source's relative-or-absolute path).
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
	// ProviderFindRequestWithRawSessionID and before the LookupIDValid gate.
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
	// their own Parse. ctx and req.Machine are supplied by SourceSetProvider.
	ParseFile jsonlParseFileFunc
	// ForceReplace marks every non-empty parse outcome from ParseFile as a full
	// replacement of the source's existing sessions, for providers whose
	// transcripts are rewritten wholesale rather than appended.
	ForceReplace bool
	// CompanionFiles returns the sidecar files that belong to a transcript
	// source, given the transcript's path. The base folds each existing
	// companion's basename into the watch plan globs, its size/mtime (and hash
	// when Hash is set) into the SourceFingerprint, and maps a changed companion
	// path back to its owning transcript in SourcesForChangedPath. It reuses the
	// sibling-metadata helpers rather than introducing a separate mechanism, so
	// providers describe companions once as transcript->companions and the base
	// drives watch, freshness, and changed-path mapping from that single hook.
	CompanionFiles func(transcriptPath string) []string

	// CompanionTranscript is the inverse of CompanionFiles: it derives the
	// owning transcript path from a changed sidecar path so companion events
	// resolve without scanning the archive. See WithCompanionTranscript.
	CompanionTranscript func(companionPath string) (string, bool)
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

// NewJSONLSourceSet builds a JSONL source set for a provider's roots from
// functional options. Every option has a zero-value default, so callers state
// only what differs.
func NewJSONLSourceSet(
	provider AgentType,
	roots []string,
	opts ...JSONLOption,
) JSONLSourceSet {
	var options JSONLSourceSetOptions
	for _, opt := range opts {
		opt(&options)
	}
	return jsonlSourceSetFromOptions(provider, roots, options)
}

// jsonlSourceSetFromOptions is the shared constructor used by NewJSONLSourceSet
// and NewDirectoryJSONLSourceSet once options have been resolved.
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
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s JSONLSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	var incomplete error
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			incomplete = errors.Join(incomplete, incompleteDiscoveryError(
				s.provider, "stat JSONL root "+root, err,
			))
			continue
		}
		if !info.IsDir() {
			err := fmt.Errorf("not a directory")
			incomplete = errors.Join(incomplete, incompleteDiscoveryError(
				s.provider, "stat JSONL root "+root, err,
			))
			continue
		}
		if err := s.discoverDirEach(ctx, root, root, yield); err != nil {
			if cause, ok := discoveryYieldCause(err); ok {
				return cause
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			incomplete = errors.Join(incomplete, err)
		}
	}
	return incomplete
}

// WatchPlan returns one watch root for each configured JSONL root. When a
// CompanionFiles hook is configured, each discovered transcript's companion
// basenames are added to every root's include globs so sidecar events are not
// filtered out before SourcesForChangedPath can map them back.
func (s JSONLSourceSet) WatchPlan(ctx context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	globs := s.includeGlobs()
	companionGlobs, err := s.companionGlobs(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	for _, root := range s.roots {
		includeGlobs := append([]string(nil), globs...)
		includeGlobs = append(includeGlobs, companionGlobs...)
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    s.options.Recursive,
			IncludeGlobs: includeGlobs,
			DebounceKey:  string(s.provider) + ":jsonl:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

// WatchRoots returns configured root metadata without discovering transcripts
// or enumerating companion basenames. WatchPlan remains archive-aware for
// parser callers that still consume include globs.
func (s JSONLSourceSet) WatchRoots(
	ctx context.Context,
) ([]WatchRoot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:        root,
			Recursive:   s.options.Recursive,
			DebounceKey: string(s.provider) + ":jsonl:" + root,
		})
	}
	return roots, nil
}

// companionGlobs enumerates the distinct sidecar basenames across all
// discovered transcripts so they can be added to every root's include globs.
func (s JSONLSourceSet) companionGlobs(ctx context.Context) ([]string, error) {
	if s.options.CompanionFiles == nil {
		return nil, nil
	}
	sources, err := s.Discover(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var globs []string
	for _, source := range sources {
		src, ok := source.Opaque.(JSONLSource)
		if !ok {
			continue
		}
		for _, companion := range s.options.CompanionFiles(src.Path) {
			base := filepath.Base(companion)
			if base == "" || base == "." {
				continue
			}
			if _, ok := seen[base]; ok {
				continue
			}
			seen[base] = struct{}{}
			globs = append(globs, base)
		}
	}
	sort.Strings(globs)
	return globs, nil
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
		// The changed path is not itself a source. A configured CompanionFiles
		// hook lets an existing sidecar map back to its owning transcript; this
		// runs before the missing-path fallback because a present companion file
		// is not eligible for the tombstone path.
		source, ok, err = s.sourceForCompanionPath(ctx, req.Path)
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

// SourceForReconciliation rebuilds an exact source that streaming discovery
// already admitted. It deliberately avoids changed-path classification, whose
// duplicate-resolution fallback can rediscover the entire archive once per
// candidate during streamed reconciliation.
func (s JSONLSourceSet) SourceForReconciliation(
	ctx context.Context, path, _ string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	path = filepath.Clean(path)
	info, err := s.sourcePathInfo(path)
	if err != nil || !info.Mode().IsRegular() {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		if !s.pathAllowedByRoot(root, path) ||
			!s.sourcePathAllowedByDescendPath(root, path) ||
			!s.pathIncluded(root, path) {
			continue
		}
		source, ok := s.sourceRef(root, path, info)
		return source, ok, nil
	}
	return SourceRef{}, false, nil
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
	if err := s.foldCompanionFingerprint(path, &fingerprint); err != nil {
		return SourceFingerprint{}, err
	}
	return fingerprint, nil
}

// foldCompanionFingerprint folds each existing companion file's size and mtime
// into the transcript fingerprint, and when content hashing is enabled mixes the
// companion contents into the hash. It reuses the sibling-metadata helpers so a
// companion change is reflected in the source's freshness identity. Missing
// companions are ignored, matching sibling-metadata behavior.
func (s JSONLSourceSet) foldCompanionFingerprint(
	transcriptPath string,
	fingerprint *SourceFingerprint,
) error {
	if s.options.CompanionFiles == nil {
		return nil
	}
	companions := s.options.CompanionFiles(transcriptPath)
	if len(companions) == 0 {
		return nil
	}
	var hasher interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	if s.options.Hash {
		h := sha256.New()
		// Seed with the transcript's existing content hash so companion mixing
		// stays anchored to the transcript while preserving its contribution.
		_, _ = io.WriteString(h, fingerprint.Hash)
		hasher = h
	}
	folded := false
	for _, companion := range companions {
		info, err := siblingMetadataFileInfo(companion)
		if err != nil {
			return err
		}
		if info == nil {
			continue
		}
		fingerprint.Size += info.Size()
		if mtime := info.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
			fingerprint.MTimeNS = mtime
		}
		if hasher != nil {
			if err := addSiblingMetadataFingerprintPart(
				hasher, "companion", companion, info,
			); err != nil {
				return err
			}
		}
		folded = true
	}
	if hasher != nil && folded {
		fingerprint.Hash = fmt.Sprintf("%x", hasher.Sum(nil))
	}
	return nil
}

// Parse resolves the request's source to a file and parses it via the ParseFile
// option, making JSONLSourceSet a full SourceSet. It mirrors the single-file
// base's parse semantics: empty results with no exclusions is a clean
// no-session skip. SourceSetProvider resolves req.Machine before calling in.
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
	_ SourceSet           = JSONLSourceSet{}
	_ WatchRootPlanner    = JSONLSourceSet{}
	_ StreamingDiscoverer = JSONLSourceSet{}
	_ SourceSet           = DirectoryJSONLSourceSet{}
	_ WatchRootPlanner    = DirectoryJSONLSourceSet{}
)

func (s JSONLSourceSet) discoverDirEach(
	ctx context.Context,
	root string,
	dir string,
	yield func(SourceRef) error,
) error {
	var incomplete error
	err := streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
		path := filepath.Join(dir, entry.Name())
		descend, descendErr := s.shouldDescend(entry, dir)
		if descendErr != nil {
			incomplete = errors.Join(incomplete, incompleteDiscoveryError(
				s.provider, "resolve symlinked JSONL directory "+path, descendErr,
			))
			return nil
		}
		if descend {
			if s.options.Recursive && s.descendPathIncluded(root, path) {
				if err := s.discoverDirEach(ctx, root, path, yield); err != nil {
					if _, ok := discoveryYieldCause(err); ok || ctx.Err() != nil {
						return err
					}
					incomplete = errors.Join(incomplete, err)
				}
			}
			return nil
		}
		info, err := s.sourceFileInfo(entry, path)
		if err != nil {
			incomplete = errors.Join(incomplete, incompleteDiscoveryError(
				s.provider, "stat JSONL source "+path, err,
			))
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		source, ok := s.sourceRef(root, path, info)
		if !ok {
			return nil
		}
		if err := yield(source); err != nil {
			return discoveryYieldError{cause: err}
		}
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		if _, ok := discoveryYieldCause(err); ok {
			return err
		}
		incomplete = errors.Join(incomplete, incompleteDiscoveryError(
			s.provider, "read JSONL directory "+dir, err,
		))
	}
	return incomplete
}

// shouldDescend reports whether recursive discovery should descend into
// entry. A followed symlink whose target cannot be statted returns an error
// rather than false: silently treating it as absent would let reconciliation
// read an authoritative-empty discovery and tombstone the sessions beneath it.
func (s JSONLSourceSet) shouldDescend(
	entry os.DirEntry, dir string,
) (bool, error) {
	if entry.IsDir() {
		return true, nil
	}
	if !s.options.FollowSymlinkDirs {
		return false, nil
	}
	return streamingDirOrSymlinkCandidate(entry, dir)
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

// sourceForCompanionPath resolves a changed sidecar path back to the transcript
// source that owns it, so a companion write triggers a re-parse of its
// transcript. With a configured CompanionTranscript inverse, the owning
// transcript is derived directly and resolved through the same per-path lookup
// a transcript event uses; without one, it falls back to scanning discovered
// transcripts for the one whose CompanionFiles list contains the changed path.
func (s JSONLSourceSet) sourceForCompanionPath(
	ctx context.Context,
	path string,
) (SourceRef, bool, error) {
	if s.options.CompanionFiles == nil {
		return SourceRef{}, false, nil
	}
	path = filepath.Clean(path)
	if s.options.CompanionTranscript != nil {
		transcript, ok := s.options.CompanionTranscript(path)
		if !ok {
			return SourceRef{}, false, nil
		}
		transcript = filepath.Clean(transcript)
		// The forward hook stays authoritative: a path the transcript does
		// not claim as a companion must not remap to it.
		if !slices.ContainsFunc(
			s.options.CompanionFiles(transcript),
			func(companion string) bool { return samePath(companion, path) },
		) {
			return SourceRef{}, false, nil
		}
		return s.sourceForPath(ctx, transcript)
	}
	sources, err := s.Discover(ctx)
	if err != nil {
		return SourceRef{}, false, err
	}
	for _, source := range sources {
		src, ok := source.Opaque.(JSONLSource)
		if !ok {
			continue
		}
		for _, companion := range s.options.CompanionFiles(src.Path) {
			if samePath(companion, path) {
				return source, true, nil
			}
		}
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
		return pathUnderRoot(root, path)
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
	// RelPath is a forward-slash key by convention, matching how the rest of
	// the parser builds source keys and display paths. filepath.Rel returns
	// OS-native separators, so normalize here; this is a no-op on Unix and
	// keeps RelPath stable for Windows callers and tests.
	rel = filepath.ToSlash(rel)
	displayPath := path
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
	if s.options.Key == nil {
		// Keys default to the absolute source path, so they are unique by
		// construction: discovery cannot hold a different preferred ref for
		// this key, and scanning the archive would only re-derive candidate.
		// Skipping it keeps per-event work bounded by the changed path.
		return candidate, true, nil
	}
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
		// Preserve s3:// roots verbatim: filepath.Clean collapses the "//" in the
		// scheme to "s3:/", which breaks the s3:// prefix checks that route
		// discovery to the object store instead of the local filesystem.
		if strings.HasPrefix(root, "s3://") {
			cleaned = append(cleaned, root)
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
