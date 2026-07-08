package remotesync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ManifestEntry describes one regular file available for remote sync.
// MtimeNS is Unix nanoseconds from the server's stat; combined with
// PAX tar headers it gives the client's mirror diff the same
// (size, mtime) change signal local sync uses.
type ManifestEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
}

// Manifest lists every syncable regular file under a TargetSet.
type Manifest struct {
	Files []ManifestEntry `json:"files"`
}

// BuildManifest walks the target dirs and extra files and returns a
// manifest of regular files, sorted by path. Symlinks and special
// files are excluded, matching WriteArchive. Missing roots and extra
// files are tolerated: sync races against live agents deleting files.
//
// File-scoped agents (Windsurf) are rejected: their raw directory tree
// differs from the sanitized subset WriteArchive streams, so a manifest
// of the raw walk would advertise files the full archive never exports.
// Callers must route such targets through the full-archive flow.
func BuildManifest(targets TargetSet) (Manifest, error) {
	if targets.HasFileScopedAgents() {
		return Manifest{}, fmt.Errorf(
			"manifest not supported for file-scoped agents")
	}
	m := Manifest{Files: []ManifestEntry{}}
	add := func(path string, info os.FileInfo) {
		m.Files = append(m.Files, ManifestEntry{
			Path:    path,
			Size:    info.Size(),
			MtimeNS: info.ModTime().UnixNano(),
		})
	}
	for _, dirs := range targets.Dirs {
		for _, root := range dirs {
			if err := manifestWalk(root, add); err != nil {
				return Manifest{}, err
			}
		}
	}
	for _, path := range targets.ExtraFiles {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Manifest{}, fmt.Errorf("stat manifest file %q: %w", path, err)
		}
		if info.Mode().IsRegular() {
			add(path, info)
		}
	}
	sort.Slice(m.Files, func(i, j int) bool {
		return m.Files[i].Path < m.Files[j].Path
	})
	return m, nil
}

func manifestWalk(root string, add func(string, os.FileInfo)) error {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat manifest root %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			add(root, info)
		}
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		add(path, info)
		return nil
	})
}
