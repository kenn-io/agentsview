package agentsview_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDesktopAppIconIncludesTransparentDockPadding(t *testing.T) {
	iconPaths := []string{
		"desktop/src-tauri/icons/icon.png",
		"desktop/src-tauri/icons/32x32.png",
		"desktop/src-tauri/icons/128x128.png",
		"desktop/src-tauri/icons/128x128@2x.png",
	}

	for _, path := range iconPaths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			img := decodePNGFile(t, path)
			assertIconHasTransparentPadding(t, path, img)
		})
	}
}

func TestDesktopMacIconBundleIncludesTransparentDockPadding(t *testing.T) {
	data, err := os.ReadFile("desktop/src-tauri/icons/icon.icns")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(data), 8, "icon.icns should contain an ICNS header")
	require.Equal(t, "icns", string(data[:4]))
	require.Equal(t, len(data), int(binary.BigEndian.Uint32(data[4:8])))

	const pngSignature = "\x89PNG\r\n\x1a\n"
	decoded := 0
	for offset := 8; offset < len(data); {
		require.LessOrEqual(t, offset+8, len(data), "truncated ICNS entry header")
		entryType := string(data[offset : offset+4])
		entrySize := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		require.GreaterOrEqual(t, entrySize, 8, "invalid ICNS entry size for %s", entryType)
		require.LessOrEqual(t, offset+entrySize, len(data), "truncated ICNS entry %s", entryType)

		payload := data[offset+8 : offset+entrySize]
		if bytes.HasPrefix(payload, []byte(pngSignature)) {
			img, err := png.Decode(bytes.NewReader(payload))
			require.NoError(t, err, "decode ICNS PNG entry %s", entryType)
			assertIconHasTransparentPadding(t, entryType, img)
			decoded++
		}

		offset += entrySize
	}

	require.NotZero(t, decoded, "icon.icns should include PNG icon renditions")
}

func decodePNGFile(t *testing.T, path string) image.Image {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	img, err := png.Decode(file)
	require.NoError(t, err)
	return img
}

func assertIconHasTransparentPadding(t *testing.T, name string, img image.Image) {
	t.Helper()

	bounds := img.Bounds()
	left, top, right, bottom, ok := visibleAlphaMargins(img)
	require.True(t, ok, "%s should contain visible pixels", name)

	minMargin := max(1, bounds.Dx()/12)
	assert.GreaterOrEqual(t, left, minMargin, "%s left transparent padding", name)
	assert.GreaterOrEqual(t, top, minMargin, "%s top transparent padding", name)
	assert.GreaterOrEqual(t, right, minMargin, "%s right transparent padding", name)
	assert.GreaterOrEqual(t, bottom, minMargin, "%s bottom transparent padding", name)
}

func visibleAlphaMargins(img image.Image) (int, int, int, int, bool) {
	bounds := img.Bounds()
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X-1, bounds.Min.Y-1

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := img.At(x, y).RGBA()
			if alpha == 0 {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}

	if maxX < minX || maxY < minY {
		return 0, 0, 0, 0, false
	}

	return minX - bounds.Min.X,
		minY - bounds.Min.Y,
		bounds.Max.X - 1 - maxX,
		bounds.Max.Y - 1 - maxY,
		true
}
