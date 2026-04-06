package server

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
	matches := resolveCursorProjectDirNameMatches(dirName, "", 2)
	switch len(matches) {
	case 0:
		return "", false
	case 1:
		return matches[0], false
	default:
		return matches[0], true
	}
}

// resolveCursorProjectDirNameHint derives a real workspace path from a
// Cursor-encoded directory name, preferring candidates that contain the
// provided hint.
func resolveCursorProjectDirNameHint(dirName, hint string) string {
	matches := resolveCursorProjectDirNameMatches(dirName, hint, 1)
	if len(matches) == 0 {
		return ""
	}
	nh := normalizeCursorDir(hint)
	if nh != "" && !cursorPathContainsHint(
		normalizeCursorDir(matches[0]), nh,
	) {
		return ""
	}
	return matches[0]
}

func resolveCursorProjectDirNameMatches(
	dirName, hint string, limit int,
) []string {
	dirName = strings.TrimSpace(dirName)
	if dirName == "" {
		return nil
	}
	hint = normalizeCursorDir(hint)

	if runtime.GOOS == "windows" {
		return resolveCursorProjectDirNameFromRootMatches(
			"", dirName, hint, limit,
		)
	}
	return resolveCursorProjectDirNameFromRootMatches(
		string(filepath.Separator), dirName, hint, limit,
	)
}

// resolveCursorProjectDirNameFromRoot reconstructs a real path from a
// Cursor-encoded project directory name by walking an existing
// filesystem tree and matching each component against the encoded token
// stream. The root parameter is mainly for tests; empty means use the
// OS default root.
func resolveCursorProjectDirNameFromRoot(
	root, dirName string,
) string {
	return resolveCursorProjectDirNameFromRootHint(root, dirName, "")
}

// resolveCursorProjectDirNameFromRootHint reconstructs a real path from
// a Cursor-encoded project directory name. It backtracks across matching
// path components instead of committing to the first greedy match, and
// prefers candidates that contain the latest transcript cwd when one is
// available.
func resolveCursorProjectDirNameFromRootHint(
	root, dirName, hint string,
) string {
	matches := resolveCursorProjectDirNameFromRootMatches(
		root, dirName, hint, 1,
	)
	if len(matches) == 0 {
		return ""
	}
	nh := normalizeCursorDir(hint)
	if nh != "" && !cursorPathContainsHint(
		normalizeCursorDir(matches[0]), nh,
	) {
		return ""
	}
	return matches[0]
}

func resolveCursorProjectDirNameFromRootMatches(
	root, dirName, hint string, limit int,
) []string {
	tokens := cursorEncodedTokens(dirName)
	if len(tokens) == 0 {
		return nil
	}

	current := root
	if runtime.GOOS == "windows" {
		if len(tokens[0]) == 1 {
			drive := tokens[0][0]
			if (drive >= 'A' && drive <= 'Z') ||
				(drive >= 'a' && drive <= 'z') {
				current = strings.ToUpper(tokens[0]) + ":" +
					string(filepath.Separator)
				tokens = tokens[1:]
			}
		}
	}
	if current == "" {
		current = string(filepath.Separator)
	}
	if !isDir(current) {
		return nil
	}
	if len(tokens) == 0 {
		return []string{current}
	}

	var matches []string
	collectCursorPathMatches(
		current, tokens, hint, limit, &matches,
	)
	return matches
}

type cursorPathMatch struct {
	name     string
	path     string
	consumed int
	hinted   bool
}

func collectCursorPathMatches(
	dir string, tokens []string, hint string, limit int,
	matches *[]string,
) {
	if limit > 0 && len(*matches) >= limit {
		return
	}
	if len(tokens) == 0 {
		if isDir(dir) {
			*matches = append(*matches, dir)
		}
		return
	}

	for _, match := range matchCursorPathComponents(dir, tokens, hint) {
		collectCursorPathMatches(
			match.path, tokens[match.consumed:], hint,
			limit, matches,
		)
		if limit > 0 && len(*matches) >= limit {
			return
		}
	}
}

func matchCursorPathComponents(
	dir string, tokens []string, hint string,
) []cursorPathMatch {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	matches := make([]cursorPathMatch, 0, len(entries))
	for _, entry := range entries {
		fullPath := filepath.Join(dir, entry.Name())
		if !isDir(fullPath) {
			continue
		}
		candidate := cursorComponentTokens(entry.Name())
		if len(candidate) == 0 || len(candidate) > len(tokens) {
			continue
		}
		if !cursorTokenPrefixMatch(tokens, candidate) {
			continue
		}
		matches = append(matches, cursorPathMatch{
			name:     entry.Name(),
			path:     fullPath,
			consumed: len(candidate),
			hinted:   cursorPathContainsHint(fullPath, hint),
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
	return matches
}

func cursorPathContainsHint(path, hint string) bool {
	if path == "" || hint == "" {
		return false
	}
	rel, err := filepath.Rel(path, hint)
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func cursorTokenPrefixMatch(tokens, candidate []string) bool {
	for i := range candidate {
		if tokens[i] != candidate[i] {
			return false
		}
	}
	return true
}

func cursorEncodedTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '-'
	})
}

func cursorComponentTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '.' || r == '_'
	})
}
