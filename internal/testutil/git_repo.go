package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

// GitRepo initializes an isolated temporary repository, using dir when provided.
func GitRepo(t *testing.T, dir string) (string, func(...string) string) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	require.NoError(t, os.MkdirAll(dir, 0700))
	t.Setenv("HOME", t.TempDir())
	runner := gitcmd.New().WithEnv(gitenv.StripAll(os.Environ()))
	runner = runner.WithConfig("user.name", "Example").WithConfig("user.email", "example@example.invalid").WithConfig("core.hooksPath", filepath.Join(dir, "no-hooks"))
	git := func(args ...string) string {
		t.Helper()
		out, stderr, err := runner.Run(t.Context(), dir, nil, args...)
		require.NoError(t, err, string(stderr))
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	return dir, git
}
