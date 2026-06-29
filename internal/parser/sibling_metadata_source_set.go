package parser

import (
	"fmt"
	"os"
	"path/filepath"
)

// siblingMetadataFileInfo stats a companion file that contributes to a JSONL
// source's freshness fingerprint, returning (nil, nil) when the file is absent
// or is a directory so callers can skip it without treating it as an error.
func siblingMetadataFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
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
