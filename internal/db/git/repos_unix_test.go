//go:build !windows

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindRepoRoot_OwnershipAndAccess(t *testing.T) {
	if os.Geteuid() == 0 && os.Getenv("AGENTSVIEW_TEST_TRUST_CONFIG") == "" {
		config := filepath.Join(t.TempDir(), "gitconfig")
		require.NoError(t, os.WriteFile(config, nil, 0o600))
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFindRepoRoot_OwnershipAndAccess$", "-test.v")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+config, "AGENTSVIEW_TEST_TRUST_CONFIG="+config)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return
	}
	skipIfNoGit(t)
	for _, path := range []string{"", ".git"} {
		t.Run("ownership "+path, func(t *testing.T) {
			if os.Geteuid() != 0 {
				t.Skip("changing ownership requires root")
			}
			repo := initBareRepo(t)
			require.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), repo))
			target := filepath.Join(repo, path)
			require.NoError(t, os.Chown(target, 65534, -1))
			t.Cleanup(func() { require.NoError(t, os.Chown(target, 0, -1)) })
			config := os.Getenv("AGENTSVIEW_TEST_TRUST_CONFIG")
			gitRun(t, repo, nil, "config", "--file", config, "--add", "safe.directory", filepath.ToSlash(repo))
			require.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), repo))
			gitRun(t, repo, nil, "config", "--file", config, "--unset-all", "safe.directory")
			assert.Empty(t, findRepoRoot(t.Context(), repo))
		})
	}
	t.Run("directory access", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		repo := initBareRepo(t)
		require.Equal(t, canonAll([]string{repo})[0], findRepoRoot(t.Context(), repo))
		for _, name := range []string{"objects", "refs"} {
			path := filepath.Join(repo, ".git", name)
			require.NoError(t, os.Chmod(path, 0o600))
			assert.Empty(t, findRepoRoot(t.Context(), repo))
			require.NoError(t, os.Chmod(path, 0o755))
		}
	})
}
