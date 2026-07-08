package remotesync

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

func WriteArchive(w io.Writer, targets TargetSet) error {
	tw := tar.NewWriter(w)
	for agent, dirs := range targets.Dirs {
		if _, fileScoped := targets.Files[agent]; fileScoped {
			continue
		}
		for _, root := range dirs {
			if err := writeArchivePath(tw, root); err != nil {
				return err
			}
		}
	}
	for agent, files := range targets.Files {
		if agent == parser.AgentWindsurf {
			if err := writeWindsurfArchiveFiles(tw, files); err != nil {
				return err
			}
			continue
		}
		for _, path := range files {
			if err := writeArchivePath(tw, path); err != nil {
				return err
			}
		}
	}
	for _, path := range targets.ExtraFiles {
		if err := writeArchivePath(tw, path); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	return nil
}

func writeWindsurfArchiveFiles(tw *tar.Writer, files []string) error {
	seen := make(map[string]struct{}, len(files))
	for _, path := range files {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		switch filepath.Base(path) {
		case parser.WindsurfStateDBName:
			if err := writeSanitizedWindsurfStateDB(tw, path); err != nil {
				return err
			}
		case parser.WindsurfStateDBName + "-wal",
			parser.WindsurfStateDBName + "-shm":
			continue
		case "workspace.json":
			if err := writeOptionalArchivePath(tw, path); err != nil {
				return err
			}
		default:
			continue
		}
	}
	return nil
}

func writeSanitizedWindsurfStateDB(tw *tar.Writer, dbPath string) error {
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat windsurf state db %q: %w", dbPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	tmpDir, err := os.MkdirTemp("", "agentsview-windsurf-export-*")
	if err != nil {
		return fmt.Errorf("create windsurf export temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, parser.WindsurfStateDBName)
	if err := parser.WriteSanitizedWindsurfStateDB(tmpPath, dbPath); err != nil {
		return fmt.Errorf("sanitize windsurf state db %q: %w", dbPath, err)
	}
	mtime := windsurfArchiveModTime(info, dbPath)
	if err := os.Chtimes(tmpPath, mtime, mtime); err != nil {
		return fmt.Errorf("stamp sanitized windsurf state db: %w", err)
	}
	tmpInfo, err := os.Stat(tmpPath)
	if err != nil {
		return fmt.Errorf("stat sanitized windsurf state db: %w", err)
	}
	return writeArchiveFileAs(tw, dbPath, tmpPath, tmpInfo)
}

func windsurfArchiveModTime(info os.FileInfo, dbPath string) time.Time {
	mtime := info.ModTime()
	for _, companion := range []string{
		dbPath + "-wal",
		filepath.Join(filepath.Dir(dbPath), "workspace.json"),
	} {
		companionInfo, err := os.Stat(companion)
		if err != nil {
			continue
		}
		if companionInfo.ModTime().After(mtime) {
			mtime = companionInfo.ModTime()
		}
	}
	return mtime
}

func writeOptionalArchivePath(tw *tar.Writer, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat archive path %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return writeArchiveFile(tw, path, info)
	}
	return writeArchivePath(tw, path)
}

func writeArchivePath(tw *tar.Writer, root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("stat archive path %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return writeArchiveFile(tw, root, info)
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
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return writeArchiveHeader(tw, path, info, nil)
		}
		return writeArchiveFile(tw, path, info)
	})
}

func writeArchiveFile(tw *tar.Writer, path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	body := io.LimitReader(file, info.Size())
	if err := writeArchiveHeader(tw, path, info, body); err != nil {
		return err
	}
	return nil
}

func writeArchiveFileAs(
	tw *tar.Writer,
	archivePath string,
	bodyPath string,
	info os.FileInfo,
) error {
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(bodyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	body := io.LimitReader(file, info.Size())
	if err := writeArchiveHeader(tw, archivePath, info, body); err != nil {
		return err
	}
	return nil
}

func writeArchiveHeader(
	tw *tar.Writer,
	path string,
	info os.FileInfo,
	body io.Reader,
) error {
	name, err := safeRemotePathArchiveName(path)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	// PAX carries sub-second mtimes; the default (unknown) format
	// makes tar.Writer round ModTime to whole seconds, which would
	// desync the manifest's mtime_ns diff from extracted files.
	hdr.Format = tar.FormatPAX
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if body != nil {
		copied, err := io.Copy(tw, body)
		if err != nil {
			return err
		}
		if copied != info.Size() {
			return fmt.Errorf(
				"copy archive file %q: expected %d bytes, copied %d",
				path, info.Size(), copied,
			)
		}
	}
	return nil
}

// WriteArchiveFiles streams a tar containing exactly the given files,
// each confined to one of allowedRoots. Entries that vanished since
// the client's manifest diff, symlinks, and non-regular files are
// skipped silently: deletions race live agents and are reconciled by
// the next manifest. writeArchivePath is unsuitable here because it
// fails on a missing root.
//
// The allowedRoots re-resolution is defense in depth: callers validate
// the file list before reaching here, but the path handed to the
// filesystem is rebuilt from the trusted root plus a filepath.IsLocal
// validated relative component, so a client-supplied string can never
// escape the resolved targets, even if a future caller forgets to
// validate.
func WriteArchiveFiles(w io.Writer, allowedRoots, files []string) error {
	tw := tar.NewWriter(w)
	for _, path := range files {
		local, ok := resolveDeltaFilePath(allowedRoots, path)
		if !ok {
			continue
		}
		info, err := os.Lstat(local)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat archive file %q: %w", local, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if err := writeArchiveFile(tw, local, info); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	return nil
}

// resolveDeltaFilePath maps a requested delta file onto the trusted
// allowedRoots. An exact root match (extra files, Aider file roots)
// returns the trusted root string itself; a file under a directory
// root returns filepath.Join(root, rel) where rel passed
// filepath.IsLocal, so the path used for filesystem access is always
// derived from a trusted base rather than the request string.
func resolveDeltaFilePath(allowedRoots []string, path string) (string, bool) {
	clean := filepath.Clean(path)
	for _, root := range allowedRoots {
		root = filepath.Clean(root)
		if clean == root {
			return root, true
		}
		rel, err := filepath.Rel(root, clean)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		return filepath.Join(root, rel), true
	}
	return "", false
}
