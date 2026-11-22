package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestFirstImportResumesAfterFirstWave(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 250 {
		path := filepath.Join(project, fmt.Sprintf("session-%03d.jsonl", i))
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "hello").String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		mtime := time.Unix(1700000000+int64(i), 0)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
	archive := openTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldWaves := firstImportWaves
	firstImportWaves = []int{3, 10, 50}
	t.Cleanup(func() { firstImportWaves = oldWaves })
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", DeferStartupMaintenance: true,
		Emitter: emitterFunc(func(scope string) {
			if scope == "sessions" {
				cancel()
			}
		}),
	})
	t.Cleanup(engine.Close)
	_, ran, err := engine.RunFirstImport(ctx, nil)
	require.True(t, ran)
	require.ErrorIs(t, err, context.Canceled)
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 3)
	require.True(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
	engine.emitter = nil
	_, ran, err = engine.RunFirstImport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ran)
	page, err = archive.ListSessions(t.Context(), db.SessionFilter{Limit: 300})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 250)
	require.False(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
}
