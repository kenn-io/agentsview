package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildAssetIndex(t *testing.T) {
	dir := t.TempDir()

	dalleDir := filepath.Join(dir, "dalle-generations")
	require.NoError(t, os.MkdirAll(dalleDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dalleDir, "file-abc123-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee.webp"),
		[]byte("fake image"), 0o644,
	))

	userDir := filepath.Join(dir, "user-xyz")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(userDir, "file_deadbeef-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee.png"),
		[]byte("fake upload"), 0o644,
	))

	idx := BuildAssetIndex(dir)

	path, ok := idx.Resolve("file-service://file-abc123")
	assert.True(t, ok)
	assert.Contains(t, path, "file-abc123")

	path, ok = idx.Resolve("sediment://file_deadbeef")
	assert.True(t, ok)
	assert.Contains(t, path, "file_deadbeef")

	_, ok = idx.Resolve("file-service://file-unknown")
	assert.False(t, ok)
}

// TestAssetResolverAdapterCopiesThroughParserBoundary drives the production
// call site the package move touched: parser.AssetResolver.Copy, implemented by
// assetResolverAdapter, is what chatgpt.resolveImageAsset calls to turn an
// export pointer into an asset:// reference. Boundary: 1 file on disk, the
// source bytes unchanged, and a repeat copy that adds no second file.
func TestAssetResolverAdapterCopiesThroughParserBoundary(t *testing.T) {
	exportDir := t.TempDir()
	userDir := filepath.Join(exportDir, "user-xyz")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	body := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // PNG header
	require.NoError(t, os.WriteFile(
		filepath.Join(userDir, "file_deadbeef-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee.png"),
		body, 0o644,
	))

	assetsDir := filepath.Join(t.TempDir(), "assets")
	var resolver parser.AssetResolver = &assetResolverAdapter{
		index:     BuildAssetIndex(exportDir),
		assetsDir: assetsDir,
	}

	srcPath, ok := resolver.Resolve("sediment://file_deadbeef")
	require.True(t, ok)

	ref, err := resolver.Copy(srcPath)
	require.NoError(t, err)
	assert.Contains(t, ref, "asset://")
	assert.Contains(t, ref, ".png")

	// The reference names a file holding exactly the export's bytes.
	stored, err := os.ReadFile(filepath.Join(assetsDir, strings.TrimPrefix(ref, "asset://")))
	require.NoError(t, err)
	assert.Equal(t, body, stored)

	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	// A second reference to the same image reuses the stored object.
	ref2, err := resolver.Copy(srcPath)
	require.NoError(t, err)
	assert.Equal(t, ref, ref2)
	entries, err = os.ReadDir(assetsDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	t.Logf("1 file for 2 copies through parser.AssetResolver: files=%d, ref=%s", len(entries), ref)
}
