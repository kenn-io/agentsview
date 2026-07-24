package remotesync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"go.kenn.io/agentsview/internal/parser"
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
// Sanitized file-scoped agents (Windsurf) are rejected: their raw
// directory tree differs from the sanitized subset WriteArchive
// streams, so a manifest of the raw walk would advertise files the
// full archive never exports. Callers must route such targets through
// the full-archive flow. Verbatim file-scoped agents (RooCode) are
// listed by their curated files instead of a raw walk — the manifest
// never advertises settings or caches under their directory roots.
func BuildManifest(targets TargetSet) (Manifest, error) {
	if targets.HasSanitizedFileScopedAgents() {
		return Manifest{}, fmt.Errorf(
			"manifest not supported for sanitized file-scoped agents")
	}
	m := Manifest{Files: []ManifestEntry{}}
	hermesStateDBs := hermesStateDBTargets(targets)
	hermesSQLite := make(map[string]struct{}, len(hermesStateDBs)*4)
	for _, stateDB := range hermesStateDBs {
		for _, path := range hermesSQLitePaths(stateDB) {
			hermesSQLite[path] = struct{}{}
		}
	}
	add := func(path string, info os.FileInfo) {
		m.Files = append(m.Files, ManifestEntry{
			Path:    path,
			Size:    info.Size(),
			MtimeNS: info.ModTime().UnixNano(),
		})
	}
	addLstat := func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("stat manifest file %q: %w", path, err)
		}
		if info.Mode().IsRegular() {
			add(path, info)
		}
		return nil
	}
	for agent, dirs := range targets.Dirs {
		if agent == parser.AgentOmnigent {
			continue
		}
		if _, fileScoped := targets.Files[agent]; fileScoped {
			continue
		}
		for _, root := range dirs {
			if _, ok := hermesSQLite[filepath.Clean(root)]; ok {
				continue
			}
			if err := manifestWalk(root, add); err != nil {
				return Manifest{}, err
			}
		}
	}
	for agent, files := range targets.Files {
		if agent == parser.AgentOmnigent {
			continue
		}
		for _, path := range files {
			if _, ok := hermesSQLite[filepath.Clean(path)]; ok {
				continue
			}
			if err := addLstat(path); err != nil {
				return Manifest{}, err
			}
		}
	}
	for _, path := range targets.ExtraFiles {
		if _, ok := hermesSQLite[filepath.Clean(path)]; ok {
			continue
		}
		if err := addLstat(path); err != nil {
			return Manifest{}, err
		}
	}
	for _, stateDB := range hermesStateDBs {
		size, modTime, exists := hermesSQLiteSnapshotIdentity(stateDB)
		if exists {
			m.Files = append(m.Files, ManifestEntry{
				Path: stateDB, Size: size, MtimeNS: modTime.UnixNano(),
			})
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
