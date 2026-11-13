package capture

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocateClaudeRootUsesProducerProjectDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixtures use POSIX absolute paths")
	}
	root := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "checkout")
	require.NoError(t, os.Mkdir(checkout, 0o700))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(checkout, link))
	resolved, err := filepath.EvalSymlinks(checkout)
	require.NoError(t, err)
	tests := []struct{ name, cwd, folder string }{
		{"long", "/" + strings.Repeat("a", 200), "-" + strings.Repeat("a", 199) + "-b6ymvl"},
		{"decomposed", "/cafe\u0301", "-cafe-"},
		{"astral", "/\U0001f600", "---"},
		{"symlink", link, strings.ReplaceAll(resolved, "/", "-")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, tt.folder, "session.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
			got, err := locateRoot(t.Context(), manifest{Provider: string(ProviderClaude), ProviderRoot: root, ProviderWorkDir: tt.cwd, ProviderSessionID: "session"})
			require.NoError(t, err)
			assert.Equal(t, []string{path}, got)
		})
	}
}

func TestClaudeProjectDirLongPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX absolute path")
	}
	cwd := "/" + strings.Repeat("a", 200)
	assert.Equal(t, "-"+strings.Repeat("a", 199)+"-b6ymvl", ClaudeProjectDir(cwd))
}
