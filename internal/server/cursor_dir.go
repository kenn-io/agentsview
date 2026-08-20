package server

import (
	"path/filepath"

	"go.kenn.io/agentsview/internal/parser"
)

// resolveCursorProjectDirFromSessionFile derives the real workspace
// directory for a Cursor session from the stored transcript path.
// The bool reports whether multiple matching paths exist on disk.
func resolveCursorProjectDirFromSessionFile(
	filePath string,
) (string, bool) {
	projectDir := cursorProjectDirNameFromTranscriptPath(filePath)
	if projectDir == "" {
		return "", false
	}
	return resolveCursorProjectDirName(projectDir)
}

// resolveCursorProjectDirFromSessionFileHint derives the real workspace
// directory for a Cursor session from the stored transcript path,
// preferring candidates that contain the provided hint.
func resolveCursorProjectDirFromSessionFileHint(
	filePath, hint string,
) string {
	projectDir := cursorProjectDirNameFromTranscriptPath(filePath)
	if projectDir == "" {
		return ""
	}
	return resolveCursorProjectDirNameHint(projectDir, hint)
}

// cursorProjectDirNameFromTranscriptPath extracts the encoded Cursor
// project directory name from either flat or nested transcript paths.
func cursorProjectDirNameFromTranscriptPath(path string) string {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	for {
		base := filepath.Base(dir)
		if base == "." || base == string(filepath.Separator) {
			return ""
		}
		if base == "agent-transcripts" {
			parent := filepath.Dir(dir)
			name := filepath.Base(parent)
			if name == "." || name == string(filepath.Separator) {
				return ""
			}
			return name
		}
		next := filepath.Dir(dir)
		if next == dir {
			return ""
		}
		dir = next
	}
}

// resolveCursorProjectDirName derives a real workspace path from a
// Cursor-encoded directory name. The bool reports whether more than
// one matching path exists on disk.
func resolveCursorProjectDirName(dirName string) (string, bool) {
	resolution := parser.ResolveCursorWorkspaceDirPassive(dirName)
	return resolution.Path, resolution.State == parser.SourceCwdAmbiguous
}

// resolveCursorProjectDirNameHint derives a real workspace path from a
// Cursor-encoded directory name, preferring candidates that contain the
// provided hint.
func resolveCursorProjectDirNameHint(dirName, hint string) string {
	resolution := parser.ResolveCursorWorkspaceDirExplicit("", dirName, hint)
	if resolution.State != parser.SourceCwdResolved {
		return ""
	}
	return resolution.Path
}

// resolveCursorProjectDirNameFromRoot reconstructs a real path from a
// Cursor-encoded project directory name by walking an existing
// filesystem tree and matching each component against the encoded token
// stream. The root parameter is mainly for tests; empty means use the
// OS default root.
func resolveCursorProjectDirNameFromRoot(
	root, dirName string,
) string {
	resolution := parser.ResolveCursorWorkspaceDirResolution(
		root, dirName, "", parser.CursorResolvePassiveDiscovery,
	)
	if resolution.State != parser.SourceCwdResolved {
		return ""
	}
	return resolution.Path
}

// resolveCursorProjectDirNameFromRootHint reconstructs a real path from
// a Cursor-encoded project directory name. It backtracks across matching
// path components instead of committing to the first greedy match, and
// prefers candidates that contain the latest transcript cwd when one is
// available.
func resolveCursorProjectDirNameFromRootHint(
	root, dirName, hint string,
) string {
	return parser.ResolveCursorWorkspaceDirHint(root, dirName, hint)
}

func resolveCursorProjectDirNameFromRootMatches(
	root, dirName, hint string, limit int,
) []string {
	return parser.ResolveCursorWorkspaceDirMatchesIn(root, dirName, hint, limit)
}
