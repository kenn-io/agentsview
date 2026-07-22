package parser

import (
	"context"
	"os"
)

// jsonlParseFileFunc parses one discovered source file into zero or more
// sessions plus the IDs of any sessions to exclude.
type jsonlParseFileFunc func(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error)

// JSONLOption configures a JSONLSourceSet (or DirectoryJSONLSourceSet) at
// construction. Options compose left to right; a later option of the same kind
// overwrites an earlier one. Every field has a sensible zero value, so a source
// set only states what differs from the default.
type JSONLOption func(*JSONLSourceSetOptions)

// --- discovery shape ---

// WithRecursive traverses subdirectories below each root rather than only the
// direct children.
func WithRecursive() JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.Recursive = true }
}

// WithExtensions restricts sources to the given file extensions (default
// .jsonl). Matching is case-sensitive to mirror legacy discovery.
func WithExtensions(exts ...string) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.Extensions = exts }
}

// WithContentHashing includes a full content hash in the source fingerprint.
// Use only when size/mtime freshness is insufficient.
func WithContentHashing() JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.Hash = true }
}

// WithSymlinkFollowing treats symlinks to both directories and regular files as
// traversable/source candidates. It is the common bundle for providers whose
// legacy discovery followed symlinked session trees.
func WithSymlinkFollowing() JSONLOption {
	return func(o *JSONLSourceSetOptions) {
		o.FollowSymlinkDirs = true
		o.FollowSymlinkFiles = true
	}
}

// WithFollowSymlinkFiles treats symlinks to regular files as sources.
func WithFollowSymlinkFiles() JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.FollowSymlinkFiles = true }
}

// WithDescendPath gates which directories recursive discovery descends into and
// which source ancestors a changed path may sit under.
func WithDescendPath(fn func(root, path string) bool) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.DescendPath = fn }
}

// WithIncludePath sets the path-only source predicate, also used for
// deleted/renamed changed paths where os.FileInfo is unavailable.
func WithIncludePath(fn func(root, path string) bool) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.IncludePath = fn }
}

// WithInclude sets a source predicate for existing files that also sees the
// os.FileInfo. It is not called for deleted/renamed changed paths.
func WithInclude(fn func(path string, info os.FileInfo) bool) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.Include = fn }
}

// --- identity / metadata ---

// WithKey sets the stable per-source dedup key.
func WithKey(fn func(root, path string) string) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.Key = fn }
}

// WithProjectHint sets display-only project metadata for a source.
func WithProjectHint(fn func(root, path string) string) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.ProjectHint = fn }
}

// WithSessionIDFromPath sets the raw (unprefixed) session ID used by FindSource
// fallback lookups.
func WithSessionIDFromPath(fn func(root, path string) string) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.SessionIDFromPath = fn }
}

// --- lookup ---

// WithLookupIDValid overrides the IsValidSessionID gate for the FindSource
// discovery fallback, for providers whose IDs carry separators it rejects.
func WithLookupIDValid(fn func(rawID string) bool) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.LookupIDValid = fn }
}

// WithRawSessionIDForLookup normalizes a raw session ID before the FindSource
// discovery comparison.
func WithRawSessionIDForLookup(fn func(rawID string) string) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.RawSessionIDForLookup = fn }
}

// WithRawSessionIDSourceFiles reconstructs candidate file paths from a raw
// session ID for providers whose IDs encode the on-disk layout.
func WithRawSessionIDSourceFiles(
	fn func(roots []string, rawID string) []string,
) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.RawSessionIDSourceFiles = fn }
}

// WithStoredPathFallbackRoot resolves the configured root for a stored source
// path that is not under any current root.
func WithStoredPathFallbackRoot(
	fn func(storedPath string) (string, bool),
) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.StoredPathFallbackRoot = fn }
}

// --- parse ---

// WithParseFile makes the source set a full SourceSet by supplying its parse
// step. Leave it unset for discovery-only embedders that supply their own Parse.
func WithParseFile(fn jsonlParseFileFunc) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.ParseFile = fn }
}

// WithForceReplace marks every non-empty ParseFile outcome as a full
// replacement of the source's existing sessions.
func WithForceReplace() JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.ForceReplace = true }
}

// --- companions ---

// WithCompanionFiles registers a sidecar hook that returns the companion files
// belonging to a transcript, given the transcript's path. The base folds the
// companions into the watch plan globs, the SourceFingerprint, and changed-path
// mapping, so a companion change re-parses its transcript. It reuses the
// sibling-metadata plumbing rather than adding an independent mechanism.
func WithCompanionFiles(fn func(transcriptPath string) []string) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.CompanionFiles = fn }
}

// WithCompanionTranscript registers the inverse of WithCompanionFiles: given a
// changed sidecar path, derive the owning transcript path directly. With the
// inverse configured, a companion event resolves through the same per-path
// lookup as a transcript event instead of scanning every discovered
// transcript's companion list, keeping per-event work bounded by the changed
// batch. Return ok=false when the path is not a recognizable companion.
func WithCompanionTranscript(
	fn func(companionPath string) (transcriptPath string, ok bool),
) JSONLOption {
	return func(o *JSONLSourceSetOptions) { o.CompanionTranscript = fn }
}
