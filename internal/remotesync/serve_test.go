package remotesync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// serveTestConfig returns a config whose only Claude directory is a fresh
// temp dir, and that directory.
func serveTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	claudeDir := filepath.Join(root, "claude-projects")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {claudeDir},
	}}
	return cfg, claudeDir
}

func writeServeTestSession(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "project-a", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "serve test").
			String(),
	), 0o644))
}

func TestServeManifestAuthorizesRequestedTargets(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)

	tests := []struct {
		name      string
		requested TargetSet
		wantErr   error
	}{
		{
			"allowed dir",
			TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}}},
			nil,
		},
		{
			"outside allowed roots",
			TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}}},
			ErrTargetNotAllowed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := ServeManifest(t.Context(), cfg, tt.requested)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, manifest.Files, 1)
		})
	}
}

func TestPlanArchiveRejectsDeltaOutsideAllowedFiles(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)

	_, err := PlanArchive(cfg, ArchiveRequest{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}},
		DeltaFiles: []string{filepath.Join(t.TempDir(), "elsewhere.jsonl")},
	})

	require.ErrorIs(t, err, ErrFileNotAllowed)
}
