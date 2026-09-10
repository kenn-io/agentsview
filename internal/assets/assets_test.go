package assets_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
)

// TestCopyAssetNormalizesJPEGExtension verifies PI-7 extension parity:
// a .jpeg source and Put with image/jpeg produce the same file and reference.
func TestCopyAssetNormalizesJPEGExtension(t *testing.T) {
	assetsDir := t.TempDir()
	body := []byte{0xff, 0xd8, 0xff, 0xe0} // JPEG magic bytes

	// Write source file with .jpeg extension.
	srcPath := filepath.Join(t.TempDir(), "photo.jpeg")
	require.NoError(t, os.WriteFile(srcPath, body, 0o644))

	// CopyAsset normalizes .jpeg → .jpg before building the destination path.
	refCopy, err := assets.CopyAsset(srcPath, assetsDir)
	require.NoError(t, err)
	assert.Contains(t, refCopy, ".jpg")
	assert.NotContains(t, refCopy, ".jpeg")

	// Put with image/jpeg maps to .jpg.
	refPut, _, err := assets.Put(assetsDir, "image/jpeg", body)
	require.NoError(t, err)
	assert.Contains(t, refPut, ".jpg")

	// Same bytes through both paths yield one reference and one file.
	assert.Equal(t, refCopy, refPut)
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	t.Logf("1 file for 2 writes (CopyAsset .jpeg and Put image/jpeg): %d on disk", len(entries))
}

// TestPutContract verifies the bytes-in Put entry point:
//   - identical bytes under image/png yield the same asset:// reference
//   - the first call reports created=true, the second reports created=false
//   - exactly one file is written after two calls with identical bytes
//   - image/svg+xml is rejected with an error
func TestPutContract(t *testing.T) {
	assetsDir := t.TempDir()

	body := []byte{0x89, 0x50, 0x4e, 0x47} // PNG magic bytes

	ref1, created1, err := assets.Put(assetsDir, "image/png", body)
	require.NoError(t, err)
	assert.True(t, created1)
	assert.Contains(t, ref1, "asset://")
	assert.Contains(t, ref1, ".png")

	ref2, created2, err := assets.Put(assetsDir, "image/png", body)
	require.NoError(t, err)
	assert.False(t, created2)
	assert.Equal(t, ref1, ref2)

	// Boundary: exactly 1 file on disk after 2 writes.
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	t.Logf("1 file on disk after 2 writes: %d", len(entries))

	// Verify the stored file matches the written bytes.
	filename := entries[0].Name()
	stored, err := os.ReadFile(filepath.Join(assetsDir, filename))
	require.NoError(t, err)
	assert.Equal(t, body, stored)

	// Verify the sha256 in the filename matches the content.
	sum := sha256.Sum256(body)
	wantFilename := fmt.Sprintf("%x.png", sum[:])
	assert.Equal(t, wantFilename, filename)

	// svg+xml is not in the passive format set.
	_, _, err = assets.Put(assetsDir, "image/svg+xml", []byte("<svg/>"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported asset type")
}

// TestPutReplacesIncompleteObject covers truncated and equal-length corrupt
// objects at the content-addressed path.
func TestPutReplacesIncompleteObject(t *testing.T) {
	body := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // PNG header
	sum := sha256.Sum256(body)

	corrupt := append([]byte(nil), body...)
	corrupt[0] ^= 0xff
	for _, tt := range []struct {
		name     string
		existing []byte
	}{
		{name: "truncated", existing: body[:3]},
		{name: "same-size-corrupt", existing: corrupt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assetsDir := t.TempDir()
			destPath := filepath.Join(assetsDir, fmt.Sprintf("%x.png", sum[:]))
			require.NoError(t, os.WriteFile(destPath, tt.existing, 0o644))

			ref, created, err := assets.Put(assetsDir, "image/png", body)
			require.NoError(t, err)
			assert.Equal(t, "asset://"+filepath.Base(destPath), ref)
			assert.True(t, created)

			stored, err := os.ReadFile(destPath)
			require.NoError(t, err)
			assert.Equal(t, body, stored)
			assert.Equal(t, sum, sha256.Sum256(stored))

			entries, err := os.ReadDir(assetsDir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)

			ref2, created2, err := assets.Put(assetsDir, "image/png", body)
			require.NoError(t, err)
			assert.Equal(t, ref, ref2)
			assert.False(t, created2)
			t.Logf("%s object repaired: %d bytes, digest=%x, files=%d", tt.name, len(stored), sum, len(entries))
		})
	}
}

// TestCopyAssetContract covers the path-in entry point, deduplication, repair
// of incomplete objects, and rejection of a non-image extension.
func TestCopyAssetContract(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source.webp")
	body := []byte("image data")
	sum := sha256.Sum256(body)
	require.NoError(t, os.WriteFile(src, body, 0o644))

	assetsDir := filepath.Join(t.TempDir(), "assets")

	ref, err := assets.CopyAsset(src, assetsDir)
	require.NoError(t, err)
	assert.Contains(t, ref, "asset://")
	assert.Contains(t, ref, ".webp")

	destPath := filepath.Join(assetsDir, strings.TrimPrefix(ref, "asset://"))
	stored, err := os.ReadFile(destPath)
	require.NoError(t, err)
	assert.Equal(t, body, stored)
	assert.Equal(t, sum, sha256.Sum256(stored))

	// A repeat copy returns the same reference without a second file.
	ref2, err := assets.CopyAsset(src, assetsDir)
	require.NoError(t, err)
	assert.Equal(t, ref, ref2)
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	corrupt := append([]byte(nil), body...)
	corrupt[0] ^= 0xff
	for _, tt := range []struct {
		name     string
		existing []byte
	}{
		{name: "truncated", existing: body[:2]},
		{name: "same-size-corrupt", existing: corrupt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(destPath, tt.existing, 0o644))
			ref3, err := assets.CopyAsset(src, assetsDir)
			require.NoError(t, err)
			assert.Equal(t, ref, ref3)
			stored, err := os.ReadFile(destPath)
			require.NoError(t, err)
			assert.Equal(t, body, stored)
			assert.Equal(t, sum, sha256.Sum256(stored))
		})
	}
	entries, err = os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	// Active content is rejected on extension.
	_, err = assets.CopyAsset(filepath.Join(t.TempDir(), "payload.svg"), assetsDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported asset type")
}
