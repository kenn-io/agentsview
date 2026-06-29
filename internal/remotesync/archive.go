package remotesync

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func WriteArchive(w io.Writer, targets TargetSet) error {
	tw := tar.NewWriter(w)
	for _, dirs := range targets.Dirs {
		for _, root := range dirs {
			if err := writeArchivePath(tw, root); err != nil {
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
