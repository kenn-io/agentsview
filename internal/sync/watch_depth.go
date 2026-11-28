package sync

import (
	"path/filepath"
	"slices"
	"strings"
)

// watchDepthBeyond reports whether path lies more than limit+slack directory
// levels below root and is not covered by ExtraDirectories patterns. A listed
// extra directory and every intermediate directory on the way to it stay inside
// the limit. With slack > 0, a file directly inside a listed extra directory
// stays inside too. Roots with limit <= 0 never exclude by depth.
func watchDepthBeyond(
	path, root string,
	limit int,
	patterns []string,
	slack int,
) bool {
	if limit <= 0 {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) <= limit+slack {
		return false
	}
	if extraCoversDirectory(parts, patterns) {
		return false
	}
	return slack <= 0 || len(parts) < 2 ||
		!extraCoversFiles(parts[:len(parts)-1], patterns)
}

// watchDepthFilesVisibleIn reports whether files directly inside dir are events
// this root asked to see: dir is within limit, or dir exactly matches a listed
// ExtraDirectories pattern rather than an unlisted intermediate directory on
// the way to one.
func watchDepthFilesVisibleIn(
	dir, root string,
	limit int,
	patterns []string,
) bool {
	if limit <= 0 {
		return true
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	if err != nil || rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) <= limit {
		return true
	}
	return extraCoversFiles(parts, patterns)
}

// watchDepthIncludePath reports whether a directory or file discovered under a
// newly created subtree stays within the root's depth limit and extra
// directories.
func watchDepthIncludePath(
	root, path string,
	isDir bool,
	limit int,
	patterns []string,
) bool {
	if isDir {
		return !watchDepthBeyond(path, root, limit, patterns, 0)
	}
	return !watchDepthBeyond(path, root, limit, patterns, 1)
}

// watchDepthShouldDropEvent reports whether a watch event falls outside a
// recursive root's MaxDepth and ExtraDirectories without performing filesystem
// I/O (safe inside native FSEvents callbacks).
//
// Directories stay visible within MaxDepth or when covered by an
// ExtraDirectories pattern (including intermediate directories leading to one).
// Files stay visible when their parent directory is within MaxDepth or exactly
// matches an ExtraDirectories pattern. Unknown item types stay visible if
// either rule admits them.
func watchDepthShouldDropEvent(
	path, root string,
	itemType backendItemType,
	limit int,
	patterns []string,
) bool {
	if limit <= 0 {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if cleanPath == cleanRoot {
		return false
	}
	switch itemType {
	case backendItemDirectory:
		return watchDepthBeyond(cleanPath, cleanRoot, limit, patterns, 0)
	case backendItemFile:
		return !watchDepthFilesVisibleIn(filepath.Dir(cleanPath), cleanRoot, limit, patterns)
	default:
		return watchDepthBeyond(cleanPath, cleanRoot, limit, patterns, 0) &&
			!watchDepthFilesVisibleIn(filepath.Dir(cleanPath), cleanRoot, limit, patterns)
	}
}

// extraCoversDirectory reports whether parts is a listed extra directory or
// an intermediate directory on the way to one.
func extraCoversDirectory(parts, patterns []string) bool {
	for _, pattern := range patterns {
		want := extraPatternParts(pattern)
		if matchExtraPrefix(parts, want) {
			return true
		}
	}
	return false
}

// extraCoversFiles reports whether parts exactly matches a listed extra
// directory whose files should be reported. Unlisted intermediate directories
// traversed only to reach a longer pattern fail the length check and stay
// quiet; directories explicitly listed in patterns stay visible even when
// another pattern descends further beneath them.
func extraCoversFiles(parts, patterns []string) bool {
	for _, pattern := range patterns {
		want := extraPatternParts(pattern)
		if len(want) == len(parts) && matchExtraPrefix(parts, want) {
			return true
		}
	}
	return false
}

func extraPatternParts(pattern string) []string {
	if pattern == "" {
		return nil
	}
	parts := strings.Split(pattern, "/")
	if slices.Contains(parts, "") {
		return nil
	}
	return parts
}

// matchExtraPrefix reports whether parts equals want or a proper prefix of
// want. "*" matches one directory name.
func matchExtraPrefix(parts, want []string) bool {
	if len(want) == 0 || len(parts) == 0 || len(parts) > len(want) {
		return false
	}
	for i := range parts {
		if want[i] != "*" && want[i] != parts[i] {
			return false
		}
	}
	return true
}
