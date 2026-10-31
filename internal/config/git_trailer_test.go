package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGitTrailer(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		enabled       bool
		env           map[string]string
		bad           bool
	}{
		{name: "missing", enabled: true},
		{name: "empty", content: "# empty\n", enabled: true},
		{name: "disabled", content: "[git_trailer]\nenabled=false\n", enabled: false},
		{name: "configured", content: "[git_trailer.session_id_env]\npi='PI_SESSION_ID'\n", enabled: true, env: map[string]string{"pi": "PI_SESSION_ID"}},
		{name: "unrelated semantic error", content: "port=-500\n[git_trailer]\nenabled=true\n", enabled: true},
		{name: "invalid agent key", content: "[git_trailer.session_id_env]\nBad='OTHER'\n", bad: true},
		{name: "malformed syntax", content: "auth_token='private\n[git_trailer]", bad: true},
		{name: "wrong type", content: "[git_trailer]\nenabled='false'\n", bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dir)
			path := filepath.Join(dir, "config.toml")
			if tt.content != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			}
			got, err := LoadGitTrailer()
			if tt.bad {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "private")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.enabled, got.Enabled)
			assert.Equal(t, tt.env, got.SessionIDEnv)
			if tt.content != "" {
				b, e := os.ReadFile(path)
				require.NoError(t, e)
				assert.Equal(t, tt.content, string(b))
			}
		})
	}
}

func TestLoadGitTrailerLocation(t *testing.T) {
	home := t.TempDir()
	legacy := t.TempDir()
	explicit := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AGENTSVIEW_DATA_DIR", "")
	t.Setenv("AGENT_VIEWER_DATA_DIR", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".agentsview"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".agentsview", "config.toml"), []byte("[git_trailer]\nenabled=false\n"), 0o600))
	cfg, err := LoadGitTrailer()
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	t.Setenv("AGENT_VIEWER_DATA_DIR", legacy)
	cfg, err = LoadGitTrailer()
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	t.Setenv("AGENTSVIEW_DATA_DIR", explicit)
	require.NoError(t, os.WriteFile(filepath.Join(explicit, "config.toml"), []byte("[git_trailer]\nenabled=false\n"), 0o600))
	cfg, err = LoadGitTrailer()
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
}

func TestLoadGitTrailerRejectsNonregularAndOversized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err := LoadGitTrailer()
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("#", (2<<20)+1)), 0o600))
	_, err = LoadGitTrailer()
	require.Error(t, err)
}

func TestSettingsPreserveGitTrailer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[git_trailer]\nenabled=false\n[git_trailer.session_id_env]\npi='PI_SESSION_ID'\n"), 0o600))
	cfg := Config{DataDir: dir}
	require.NoError(t, cfg.SaveSettings(map[string]any{"port": 8099}))
	got, err := LoadGitTrailer()
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	assert.Equal(t, map[string]string{"pi": "PI_SESSION_ID"}, got.SessionIDEnv)
}

func TestLoadGitTrailerSizeBoundary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("#"+strings.Repeat("x", (2<<20)-1)), 0o600))
	cfg, err := LoadGitTrailer()
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	require.NoError(t, os.WriteFile(path, []byte("#"+strings.Repeat("x", 2<<20)), 0o600))
	_, err = LoadGitTrailer()
	require.Error(t, err)
}
