package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCaptureCorpusRestoresChooseSameTranscript(t *testing.T) {
	python := requireRunnablePython3(t)
	module, err := filepath.Abs(filepath.Join("..", "docs", "screenshots"))
	require.NoError(t, err)
	source := t.TempDir()
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(source, name, "project", "duplicate.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2024-01-01T00:00:00Z", "root "+name).String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	for _, newer := range []string{"a", "b"} {
		t.Run(newer, func(t *testing.T) {
			for _, name := range []string{"a", "b"} {
				stamp := time.Unix(10, 0)
				if name == newer {
					stamp = time.Unix(20, 0)
				}
				require.NoError(t, os.Chtimes(filepath.Join(source, name, "project", "duplicate.jsonl"), stamp, stamp))
			}
			target := filepath.Join(t.TempDir(), "corpus")
			cmd := exec.CommandContext(t.Context(), python, "-c", `import sys; from pathlib import Path; sys.path.insert(0, sys.argv[1]); from capture import copy_corpus; copy_corpus(Path(sys.argv[2]), Path(sys.argv[3]), "2024-01-01T00:00:00Z")`, module, source, target)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {filepath.Join(target, "a"), filepath.Join(target, "b")}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Equal(t, 1, stats.Synced)
			session, err := database.GetSession(t.Context(), "duplicate")
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.FirstMessage)
			assert.Equal(t, "root a", *session.FirstMessage)
		})
	}
}

func TestUpdateDocsRequiresReviewedAssetsBeforeDoingWork(t *testing.T) {
	repo := t.TempDir()
	script := installScript(t, repo, filepath.Join("scripts", "update-docs.sh"))
	assets := filepath.Join(repo, "docs", "assets", "generated")
	require.NoError(t, os.MkdirAll(assets, 0o755))
	marker := filepath.Join(assets, "keep.png")
	require.NoError(t, os.WriteFile(marker, []byte("reviewed image"), 0o644))
	output, err := runBash(t, repo, nil, script)
	require.Error(t, err)
	assert.Contains(t, string(output), "--source must name")
	contents, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "reviewed image", string(contents))
}
