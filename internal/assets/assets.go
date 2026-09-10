package assets

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// allowedImageExts is the set of passive image formats safe to serve inline.
// Active content (svg, html, js) is rejected to prevent stored XSS.
var allowedImageExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".gif":  true,
}

// mediaTypeToExt maps accepted image media types to their canonical extension.
var mediaTypeToExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// ExtForMediaType returns the canonical asset-store extension for an image
// media type Put accepts. Callers that decide what to migrate must ask here so
// their acceptance cannot drift from what Put will store.
func ExtForMediaType(mediaType string) (string, bool) {
	ext, ok := mediaTypeToExt[mediaType]
	return ext, ok
}

// Put writes body to the assets directory under its SHA-256 hash and returns
// the asset:// reference. created is false when a complete object already
// existed; repairing a partial one reports true. mediaType must name a passive
// image format.
func Put(assetsDir, mediaType string, body []byte) (ref string, created bool, err error) {
	ext, ok := mediaTypeToExt[mediaType]
	if !ok {
		return "", false, fmt.Errorf("unsupported asset type: %s", mediaType)
	}

	sum := sha256.Sum256(body)
	hash := fmt.Sprintf("%x", sum[:])
	filename := hash + ext
	destPath := filepath.Join(assetsDir, filename)

	if isCompleteObject(destPath, int64(len(body)), hash) {
		return "asset://" + filename, false, nil
	}

	if err := writeObject(assetsDir, destPath, func(out *os.File) error {
		_, err := out.Write(body)
		return err
	}); err != nil {
		return "", false, err
	}

	return "asset://" + filename, true, nil
}

// isCompleteObject reports whether the content-addressed path already holds a
// regular file with the expected size and SHA-256 digest.
func isCompleteObject(destPath string, size int64, expectedHash string) bool {
	info, err := os.Lstat(destPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false
	}

	f, err := os.Open(destPath)
	if err != nil {
		return false
	}
	h := sha256.New()
	actualSize, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || actualSize != size {
		return false
	}
	return fmt.Sprintf("%x", h.Sum(nil)) == expectedHash
}

// writeObject fills a temp file in assetsDir and renames it onto destPath, so
// every byte is on disk before the content-addressed path exists. A crash or a
// full disk mid-write leaves the temp file, never a short object.
func writeObject(assetsDir, destPath string, fill func(*os.File) error) error {
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		return fmt.Errorf("creating assets dir: %w", err)
	}

	tmp, err := os.CreateTemp(assetsDir, filepath.Base(destPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing asset: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename has moved it

	if err := fill(tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("copying asset: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flushing asset: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing asset: %w", err)
	}
	// os.CreateTemp opens at 0600; assets are served read-only to everyone.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("setting asset mode: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("publishing asset: %w", err)
	}
	return nil
}

// CopyAsset copies a file to the assets directory using its SHA-256 hash as
// the filename. Returns the asset:// reference. Only passive image types are
// accepted; active content is rejected.
func CopyAsset(srcPath, assetsDir string) (string, error) {
	ext := strings.ToLower(filepath.Ext(srcPath))
	if !allowedImageExts[ext] {
		return "", fmt.Errorf(
			"unsupported asset type: %s", ext,
		)
	}
	// Normalize .jpeg to .jpg so Put and CopyAsset produce the same filename.
	// Gated first, so allowedImageExts stays the list of accepted suffixes.
	if ext == ".jpeg" {
		ext = ".jpg"
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("reading asset: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", fmt.Errorf("hashing asset: %w", err)
	}

	hash := fmt.Sprintf("%x", h.Sum(nil))
	filename := hash + ext // ext is already normalized to .jpg for .jpeg sources
	destPath := filepath.Join(assetsDir, filename)

	if isCompleteObject(destPath, size, hash) {
		return "asset://" + filename, nil
	}

	// Re-read source for copy (we consumed it for hashing).
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("reopening asset: %w", err)
	}
	defer src.Close()

	if err := writeObject(assetsDir, destPath, func(out *os.File) error {
		_, err := io.Copy(out, src)
		return err
	}); err != nil {
		return "", err
	}

	return "asset://" + filename, nil
}
