package parser

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CodexMetadata is an immutable snapshot of the metadata directories for each
// transcript root. Each provider owns its snapshot; imports use their own paths.
type CodexMetadata struct{ roots map[string][]string }

func (m CodexMetadata) dirs(root string) []string {
	if dirs, ok := m.roots[root]; ok {
		return dirs
	}
	return []string{filepath.Dir(root)}
}

func (m CodexMetadata) IndexPaths(path string) []string {
	root := codexSessionRootForIndex(path)
	if root == "" {
		return nil
	}
	return m.IndexFiles(root)
}

// IndexFiles lists title indexes for one configured transcript root.
func (m CodexMetadata) IndexFiles(root string) []string {
	var paths []string
	for _, dir := range m.dirs(root) {
		paths = append(paths, filepath.Join(dir, CodexSessionIndexFilename))
	}
	return paths
}

func codexSessionRootForIndex(path string) string {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if base := filepath.Base(dir); base == "sessions" || base == "archived_sessions" {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
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
		maps.Copy(merged, entry.titles)
	}
	return merged, nil
}

// CodexMetadataProvider exposes the same metadata snapshot used by parsing to
// the engine's Codex title and freshness operations.
type CodexMetadataProvider interface{ Metadata() CodexMetadata }

func (m CodexMetadata) ReadThreadName(
	sessionPath, sessionID string,
) (string, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", false, nil
	}
	indexPaths := m.IndexPaths(sessionPath)
	if len(indexPaths) == 0 {
		return "", false, nil
	}
	titles, err := loadCodexSessionIndexes(indexPaths)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	title, ok := titles[sessionID]
	return strings.TrimSpace(title), ok, nil
}

func (m CodexMetadata) Verify(sessionPath string) error {
	indexPaths := m.IndexPaths(sessionPath)
	if len(indexPaths) == 0 {
		return nil
	}
	_, err := loadCodexSessionIndexes(indexPaths)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (m CodexMetadata) EffectiveMtime(sessionPath string, fileMtime int64) int64 {
	for _, idxPath := range m.IndexPaths(sessionPath) {
		if si, err := os.Stat(idxPath); err == nil {
			if idxMtime := si.ModTime().UnixNano(); idxMtime > fileMtime {
				fileMtime = idxMtime
			}
		}
	}
	return fileMtime
}
