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

// jsonlOption configures a JSONLSourceSet (or DirectoryJSONLSourceSet) at
// construction. Options compose left to right; a later option of the same kind
// overwrites an earlier one. Every field has a sensible zero value, so a source
// set only states what differs from the default.
type jsonlOption func(*JSONLSourceSetOptions)

// --- discovery shape ---

// withRecursive traverses subdirectories below each root rather than only the
// direct children.
func withRecursive() jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.Recursive = true }
}

// withExtensions restricts sources to the given file extensions (default
// .jsonl). Matching is case-sensitive to mirror legacy discovery.
func withExtensions(exts ...string) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.Extensions = exts }
}

// withContentHashing includes a full content hash in the source fingerprint.
// Use only when size/mtime freshness is insufficient.
func withContentHashing() jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.Hash = true }
}

// withSymlinkFollowing treats symlinks to both directories and regular files as
// traversable/source candidates. It is the common bundle for providers whose
// legacy discovery followed symlinked session trees.
func withSymlinkFollowing() jsonlOption {
	return func(o *JSONLSourceSetOptions) {
		o.FollowSymlinkDirs = true
		o.FollowSymlinkFiles = true
	}
}

// withFollowSymlinkFiles treats symlinks to regular files as sources.
func withFollowSymlinkFiles() jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.FollowSymlinkFiles = true }
}

// withDescendPath gates which directories recursive discovery descends into and
// which source ancestors a changed path may sit under.
func withDescendPath(fn func(root, path string) bool) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.DescendPath = fn }
}

// withIncludePath sets the path-only source predicate, also used for
// deleted/renamed changed paths where os.FileInfo is unavailable.
func withIncludePath(fn func(root, path string) bool) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.IncludePath = fn }
}

// withInclude sets a source predicate for existing files that also sees the
// os.FileInfo. It is not called for deleted/renamed changed paths.
func withInclude(fn func(path string, info os.FileInfo) bool) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.Include = fn }
}

// --- identity / metadata ---

// withKey sets the stable per-source dedup key.
func withKey(fn func(root, path string) string) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.Key = fn }
}

// withFingerprintKey overrides the persisted lookup/freshness identity when the
// display path is not the value that should survive a provider migration.
func withFingerprintKey(fn func(root, path string) string) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.FingerprintKey = fn }
}

// withProjectHint sets display-only project metadata for a source.
func withProjectHint(fn func(root, path string) string) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.ProjectHint = fn }
}

// withSessionIDFromPath sets the raw (unprefixed) session ID used by FindSource
// fallback lookups.
func withSessionIDFromPath(fn func(root, path string) string) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.SessionIDFromPath = fn }
}

// --- lookup ---

// withLookupIDValid overrides the IsValidSessionID gate for the FindSource
// discovery fallback, for providers whose IDs carry separators it rejects.
func withLookupIDValid(fn func(rawID string) bool) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.LookupIDValid = fn }
}

// withRawSessionIDForLookup normalizes a raw session ID before the FindSource
// discovery comparison.
func withRawSessionIDForLookup(fn func(rawID string) string) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.RawSessionIDForLookup = fn }
}

// withRawSessionIDSourceFiles reconstructs candidate file paths from a raw
// session ID for providers whose IDs encode the on-disk layout.
func withRawSessionIDSourceFiles(
	fn func(roots []string, rawID string) []string,
) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.RawSessionIDSourceFiles = fn }
}

// withStoredPathFallbackRoot resolves the configured root for a stored source
// path that is not under any current root.
func withStoredPathFallbackRoot(
	fn func(storedPath string) (string, bool),
) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.StoredPathFallbackRoot = fn }
}

// --- parse ---

// withParseFile makes the source set a full SourceSet by supplying its parse
// step. Leave it unset for discovery-only embedders that supply their own Parse.
func withParseFile(fn jsonlParseFileFunc) jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.ParseFile = fn }
}

// withForceReplace marks every non-empty ParseFile outcome as a full
// replacement of the source's existing sessions.
func withForceReplace() jsonlOption {
	return func(o *JSONLSourceSetOptions) { o.ForceReplace = true }
}
