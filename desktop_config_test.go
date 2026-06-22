package agentsview_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDesktopTauriConfigOverridesAppImageDirIcon(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("desktop", "src-tauri", "tauri.conf.json"))
	require.NoError(t, err)

	var config struct {
		Bundle struct {
			Linux struct {
				AppImage struct {
					Files map[string]string `json:"files"`
				} `json:"appimage"`
			} `json:"linux"`
		} `json:"bundle"`
	}
	require.NoError(t, json.Unmarshal(raw, &config))

	iconPath, ok := config.Bundle.Linux.AppImage.Files["/.DirIcon"]
	require.True(t, ok, "AppImage root .DirIcon must be provided explicitly")
	assert.Equal(t, "icons/icon.png", iconPath)

	info, err := os.Stat(filepath.Join("desktop", "src-tauri", iconPath))
	require.NoError(t, err)
	assert.False(t, info.IsDir(), "configured .DirIcon source must be a file")
}
