package parser

import (
	"fmt"
	"os"
	"path/filepath"
)

// siblingMetadataFileInfo preserves the legacy companion behavior: a symlink
// to a regular companion is followed. Providers that require strict symlink
// rejection must use siblingMetadataFileInfoStrict instead.
func siblingMetadataFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	return info, nil
}

// siblingMetadataFileInfoStrict inspects the directory entry itself and never
// follows a symlink. It is used by providers whose companion files are part of
// a security-sensitive source boundary.
func siblingMetadataFileInfoStrict(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil
	}
	return info, nil
}

// addSiblingMetadataFingerprintPart folds a source or companion file's name,
// size, mtime, and content hash into the fingerprint hasher.
func addSiblingMetadataFingerprintPart(
	h interface{ Write([]byte) (int, error) },
	label string,
	path string,
	info os.FileInfo,
) error {
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(
		h,
		"%s:%s:%d:%d:%s\n",
		label,
		filepath.Base(path),
		info.Size(),
		info.ModTime().UnixNano(),
		hash,
	)
	return nil
}
