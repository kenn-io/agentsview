package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSourceScanImportsClaudeCodexAndTitleCompanions(t *testing.T) {
	database := openTestDB(t)
	base := t.TempDir()
	claudeRoot := filepath.Join(base, "projects")
	claudePath := filepath.Join(claudeRoot, "project-a", "scan-claude.jsonl")
	codexHome := filepath.Join(base, "codex")
	codexRoot := filepath.Join(codexHome, "sessions")
	const codexID = "019f0000-0000-7000-8000-000000000001"
	codexPath := filepath.Join(codexRoot, "2026", "10", "03", "rollout-2026-10-03T10-00-00-"+codexID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(codexPath), 0o700))
	claude := testjsonl.NewSessionBuilder().AddClaudeUserWithUUID("2026-10-03T10:00:00Z", "Claude first", "one", "")
	codex := testjsonl.NewSessionBuilder().AddCodexMeta("2026-10-03T10:00:00Z", codexID, "/workspace/project", "codex_cli_rs").AddCodexMessage("2026-10-03T10:00:01Z", "user", "Codex first")
	require.NoError(t, os.WriteFile(claudePath, []byte(claude.String()), 0o600))
	require.NoError(t, os.WriteFile(codexPath, []byte(codex.String()), 0o600))
	dirs := map[parser.AgentType][]string{parser.AgentClaude: {claudeRoot}, parser.AgentCodex: {codexRoot}}
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: dirs, Machine: "local"})
	t.Cleanup(engine.Close)
	var roots []WatchRoot
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex} {
		provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: dirs[agent]})
		require.True(t, ok)
		plan, err := parser.ResolveWatchRoots(t.Context(), provider)
		require.NoError(t, err)
		for _, root := range plan {
			roots = append(roots, WatchRoot{Path: root.Path, Recursive: root.Recursive, SourceFileGlobs: root.SourceFileGlobs, SourceScanScopes: []WatchScope{{Agent: string(agent), SyncDir: dirs[agent][0]}}})
		}
	}
	scanner := newSourceScanner(roots, nil)
	emit := func(ctx context.Context, batch WatchBatch) error { return ApplyWatchBatch(ctx, engine, batch, nil) }
	require.NoError(t, scanner.scan(t.Context(), emit))
	for _, id := range []string{"scan-claude", "codex:" + codexID} {
		messages, err := database.GetMessages(t.Context(), id, 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 1)
	}
	claude.AddClaudeUserWithUUID("2026-10-03T10:00:02Z", "Claude appended", "two", "one")
	codex.AddCodexMessage("2026-10-03T10:00:02Z", "assistant", "Codex appended")
	require.NoError(t, os.WriteFile(claudePath, []byte(claude.String()), 0o600))
	require.NoError(t, os.WriteFile(codexPath, []byte(codex.String()), 0o600))
	index := filepath.Join(codexHome, parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+codexID+`","thread_name":"Renamed by index","updated_at":"2026-10-03T10:00:03Z"}`+"\n"), 0o600))
	require.NoError(t, scanner.scan(t.Context(), emit))
	for _, tc := range []struct{ id, content string }{{"scan-claude", "Claude appended"}, {"codex:" + codexID, "Codex appended"}} {
		messages, err := database.GetMessages(t.Context(), tc.id, 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, tc.content, messages[1].Content)
	}
	session, err := database.GetSession(t.Context(), "codex:"+codexID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "Renamed by index", *session.DisplayName)
	// A fresh scanner must not use the existing title-index directory to
	// reconcile an unavailable transcript tree as empty.
	require.NoError(t, os.Rename(codexRoot, codexRoot+"-away"))
	restarted := newSourceScanner(roots, nil)
	require.NoError(t, restarted.scan(t.Context(), emit))
	preserved, err := database.GetSessionFull(t.Context(), "codex:"+codexID)
	require.NoError(t, err)
	require.NotNil(t, preserved)
	assert.Nil(t, preserved.SourceMissingAt)
}
