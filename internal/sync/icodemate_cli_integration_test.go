package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestIcodemateCLIForkSessionsSurviveSourceReparse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "fork-session.jsonl")
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"root","message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"root","message":{"content":[{"type":"text","text":"main reply 1"}]}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"main prompt 2"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u3","parentUuid":"u2","message":{"content":"main prompt 3"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"u4","parentUuid":"u3","message":{"content":"main prompt 4"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u5","parentUuid":"u4","message":{"content":"main prompt 5"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:06Z","uuid":"fork","parentUuid":"root","message":{"content":[{"type":"text","text":"fork reply"}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentIcodemate: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(context.Background(), nil)
	require.Zero(t, first.Failed)
	require.Equal(t, 2, first.Synced)

	mainSession, err := database.GetSession(
		context.Background(), "icodemate:fork-session",
	)
	require.NoError(t, err)
	require.NotNil(t, mainSession)
	forkSession, err := database.GetSession(
		context.Background(), "icodemate:fork-session-fork",
	)
	require.NoError(t, err)
	require.NotNil(t, forkSession)
	require.NotNil(t, forkSession.ParentSessionID)
	assert.Equal(t, "icodemate:fork-session", *forkSession.ParentSessionID)

	updated := content + `{"type":"ai-title","aiTitle":"Updated title"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
	second := engine.SyncAll(context.Background(), nil)
	require.Zero(t, second.Failed)
	require.Equal(t, 2, second.Synced)

	mainSession, err = database.GetSession(
		context.Background(), "icodemate:fork-session",
	)
	require.NoError(t, err)
	require.NotNil(t, mainSession)
	require.NotNil(t, mainSession.DisplayName)
	assert.Equal(t, "Updated title", *mainSession.DisplayName)
	forkSession, err = database.GetSession(
		context.Background(), "icodemate:fork-session-fork",
	)
	require.NoError(t, err)
	require.NotNil(t, forkSession)
	require.NotNil(t, forkSession.ParentSessionID)
	assert.Equal(t, "icodemate:fork-session", *forkSession.ParentSessionID)
}
