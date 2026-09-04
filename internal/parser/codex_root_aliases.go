package parser

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// codexRootAliases maps a configured Codex session root to alias roots that
// resolve to the same directory, such as a second Codex home whose sessions
// directory is a symbolic link to the primary home. Transcripts are scanned
// once through the canonical root, but each alias home may keep its own
// history.jsonl and session_index.jsonl, so sidecar reads fan out to every
// alias. The table is process-wide, like the session index cache it feeds.
var codexRootAliases = struct {
	mu    sync.RWMutex
	roots map[string][]string
}{}

// SetCodexRootAliases replaces the alias table. Keys and values are session
// roots (the sessions or archived_sessions directory), not homes. Passing nil
// clears the table.
func SetCodexRootAliases(aliases map[string][]string) {
	cleaned := make(map[string][]string, len(aliases))
	for root, list := range aliases {
		root = filepath.Clean(root)
		seen := map[string]struct{}{root: {}}
		for _, alias := range list {
			alias = filepath.Clean(alias)
			if _, dup := seen[alias]; dup {
				continue
			}
			seen[alias] = struct{}{}
			cleaned[root] = append(cleaned[root], alias)
		}
	}
	codexRootAliases.mu.Lock()
	codexRootAliases.roots = cleaned
	codexRootAliases.mu.Unlock()
}

// codexAliasRoots returns the alias roots registered for a session root.
func codexAliasRoots(root string) []string {
	codexRootAliases.mu.RLock()
	defer codexRootAliases.mu.RUnlock()
	return append([]string(nil), codexRootAliases.roots[filepath.Clean(root)]...)
}

// codexSidecarDirs returns the directories that may hold Codex sidecar files
// for a session root: the root's own parent followed by each alias parent,
// without repeats.
func codexSidecarDirs(root string) []string {
	root = filepath.Clean(root)
	dirs := []string{filepath.Dir(root)}
	seen := map[string]struct{}{dirs[0]: {}}
	for _, alias := range codexAliasRoots(root) {
		dir := filepath.Dir(alias)
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	return dirs
}

// codexSessionIndexPaths returns every session_index.jsonl path that may
// describe a transcript: the index beside its own root first, then the index
// beside each alias root. It returns nil for paths outside a Codex layout.
func codexSessionIndexPaths(sessionPath string) []string {
	primary := codexSessionIndexPath(sessionPath)
	if primary == "" {
		return nil
	}
	root := codexSessionRootForIndex(sessionPath)
	if root == "" {
		return []string{primary}
	}
	paths := make([]string, 0, 2)
	for _, dir := range codexSidecarDirs(root) {
		paths = append(paths, filepath.Join(dir, CodexSessionIndexFilename))
	}
	return paths
}

// codexSessionRootForIndex returns the sessions or archived_sessions
// directory that contains a transcript, or "" when there is none.
func codexSessionRootForIndex(sessionPath string) string {
	dir := filepath.Dir(sessionPath)
	for dir != "" {
		base := filepath.Base(dir)
		if base == "sessions" || base == "archived_sessions" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// loadCodexSessionIndexes concatenates the title indexes at the given paths.
// Files are applied oldest first so the most recently written index wins
// when two homes both name the same session. A path that does not exist is
// skipped; the result is os.ErrNotExist only when every path is absent.
func loadCodexSessionIndexes(paths []string) (map[string]string, error) {
	type loaded struct {
		mtime  int64
		titles map[string]string
	}
	var found []loaded
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		titles, err := loadCodexSessionIndex(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = append(found, loaded{
			mtime: info.ModTime().UnixNano(), titles: titles,
		})
	}
	if len(found) == 0 {
		return nil, os.ErrNotExist
	}
	if len(found) == 1 {
		return found[0].titles, nil
	}
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].mtime < found[j].mtime
	})
	merged := make(map[string]string)
	for _, entry := range found {
		for id, title := range entry.titles {
			merged[id] = title
		}
	}
	return merged, nil
}
