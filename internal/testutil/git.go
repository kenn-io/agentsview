package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// IsolateGitEnv confines Git fixture commands and reads to test-owned state.
func IsolateGitEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
		}
	}
	// Production Git runners strip GIT_CONFIG_GLOBAL, so isolate the
	// default config locations they read after sanitizing the environment.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	globalConfig := filepath.Join(home, ".gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, nil, 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}
