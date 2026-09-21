package importer

import (
	"os"
	"path/filepath"
	"strings"
)

// AssetIndex maps asset pointer prefixes to file paths on disk.
type AssetIndex struct {
	entries map[string]string
}

// BuildAssetIndex scans an export directory for image assets.
// ChatGPT exports store images in three locations:
//   - dalle-generations/ (DALL-E outputs)
//   - user-*/ directories (user uploads via sediment://)
//   - root directory as file-* (user uploads via file-service://)
func BuildAssetIndex(exportDir string) AssetIndex {
	idx := AssetIndex{entries: make(map[string]string)}

	// DALL-E generated images.
	dalleDir := filepath.Join(exportDir, "dalle-generations")
	scanAssetDir(dalleDir, idx.entries)

	// User uploads in user-*/ subdirectories.
	matches, _ := filepath.Glob(filepath.Join(exportDir, "user-*"))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err == nil && info.IsDir() {
			scanAssetDir(m, idx.entries)
		}
	}

	// User uploads as loose file-* in the root directory.
	scanAssetFiles(exportDir, idx.entries)

	return idx
}

func scanAssetDir(dir string, entries map[string]string) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		prefix := extractAssetPrefix(name)
		if prefix != "" {
			entries[prefix] = filepath.Join(dir, name)
		}
	}
}

// scanAssetFiles indexes only file-* and file_* entries in a
// directory. Used for the root dir where non-asset files exist.
func scanAssetFiles(dir string, entries map[string]string) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if !strings.HasPrefix(name, "file-") &&
			!strings.HasPrefix(name, "file_") {
			continue
		}
		prefix := extractAssetPrefix(name)
		if prefix != "" {
			entries[prefix] = filepath.Join(dir, name)
		}
	}
}

// extractAssetPrefix gets the stable prefix from filenames like
// "file-abc123-aaaa-bbbb-cccc-dddddddddddd.webp"
func extractAssetPrefix(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	// UUID is 36 chars at end, preceded by hyphen = 37 chars total
	if len(base) > 37 && base[len(base)-37] == '-' {
		return base[:len(base)-37]
	}
	return base
}

// Resolve maps an asset pointer URL to a file path.
func (idx AssetIndex) Resolve(pointer string) (string, bool) {
	var prefix string
	switch {
	case strings.HasPrefix(pointer, "file-service://"):
		prefix = strings.TrimPrefix(pointer, "file-service://")
	case strings.HasPrefix(pointer, "sediment://"):
		prefix = strings.TrimPrefix(pointer, "sediment://")
	default:
		return "", false
	}
	path, ok := idx.entries[prefix]
	return path, ok
}
