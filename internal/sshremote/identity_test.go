package sshremote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalBuildIdentities(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agentsview")
	require.NoError(t, os.WriteFile(exe, []byte("build one"), 0o755))

	tests := []struct {
		version     string
		wantRelease string
		isRelease   bool
	}{
		{"v0.45.0", "v0.45.0", true},
		{"0.45.0", "v0.45.0", true},
		{"dev", "", false},
		{"v0.45.0-50-g86f100ea", "", false},
		{"v0.45.0-50-g86f100ea-dirty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			build := LocalBuild{Version: tt.version, Executable: exe}
			id, ok := build.ReleaseID()
			assert.Equal(t, tt.isRelease, ok)
			assert.Equal(t, tt.wantRelease, id)
			content, err := build.ContentID()
			require.NoError(t, err)
			assert.Regexp(t, `^[0-9A-Za-z._+-]+-[0-9a-f]{16}$`, content)
		})
	}
}

func TestContentIDChangesWithBytes(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agentsview")
	require.NoError(t, os.WriteFile(exe, []byte("build one"), 0o755))
	first, err := LocalBuild{Version: "dev", Executable: exe}.ContentID()
	require.NoError(t, err)
	// A rebuild changes size or mtime, which invalidates the cached hash.
	require.NoError(t, os.WriteFile(exe, []byte("build two, longer"), 0o755))

	second, err := LocalBuild{Version: "dev", Executable: exe}.ContentID()

	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

func TestContentIDSanitizesVersion(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agentsview")
	require.NoError(t, os.WriteFile(exe, []byte("build one"), 0o755))

	id, err := LocalBuild{Version: "v1/../x y", Executable: exe}.ContentID()

	require.NoError(t, err)
	assert.Regexp(t, `^v1_\.\._x_y-[0-9a-f]{16}$`, id)
}

func TestContentIDMissingExecutable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	_, err := LocalBuild{Version: "dev", Executable: missing}.ContentID()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "local executable")
}

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		s, m    string
		want    platform
		wantErr bool
	}{
		{"Linux", "x86_64", platform{"linux", "amd64"}, false},
		{"Linux", "aarch64", platform{"linux", "arm64"}, false},
		{"Darwin", "arm64", platform{"darwin", "arm64"}, false},
		{"Darwin", "x86_64", platform{"darwin", "amd64"}, false},
		{"FreeBSD", "amd64", platform{}, true},
		{"Linux", "riscv64", platform{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.s+"/"+tt.m, func(t *testing.T) {
			got, err := parsePlatform(tt.s, tt.m)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.s)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
