package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestTraeXAdoptionRepointsRootFromCodex covers the documented adoption path:
// before TraeX was a first-class agent the only way to index its rollouts was
// to declare ~/.trae/cli/sessions as a codex source, so the DB already holds
// codex:<uuid> rows for those files. Switching the config to agent = "traex"
// must produce traex:<uuid> rows. The stored rows are keyed by file path
// alone, so without an agent check the DB-aware skip would accept the stale
// codex row and the traex row would never be written.
func TestTraeXAdoptionRepointsRootFromCodex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	dir := filepath.Join(root, "2026", "08", "01")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "rollout-2026-08-01T18-07-03-"+uuid+".jsonl")
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/workspace/project", "codex-tui").
		AddCodexMessage(tsEarlyS1, "user", "first prompt").
		String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	database := dbtest.OpenTestDB(t)
	newEngine := func(agent parser.AgentType) *agentsync.Engine {
		engine := agentsync.NewEngine(database, agentsync.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{agent: {root}},
			Machine:   "local",
		})
		t.Cleanup(engine.Close)
		return engine
	}

	// The old workaround: the root is declared as codex.
	require.NoError(t, newEngine(parser.AgentCodex).ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	require.Equal(t, path, database.GetSessionFilePath("codex:"+uuid))

	// The config switch, with no database rebuild.
	traexEngine := newEngine(parser.AgentTraeX)
	require.NoError(t, traexEngine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	assert.Equal(t, path, database.GetSessionFilePath("traex:"+uuid),
		"the unchanged file must be reparsed under its new identity")

	// A later append must extend the TraeX session, not graft relabeled rows
	// onto the codex row that still shares this path.
	appended := testjsonl.CodexMsgJSON("user", "second prompt", tsEarlyS5)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(appended + "\n")
	require.NoError(t, file.Close())
	require.NoError(t, err)

	require.NoError(t, traexEngine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))

	traexSession, err := database.GetSession(t.Context(), "traex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, traexSession)
	assert.Equal(t, 2, traexSession.MessageCount)

	// The codex row is left behind, not silently extended: the source file
	// still exists, so nothing tombstones it. An in-place switch therefore
	// shows each session twice until the database is rebuilt, which is why the
	// documented adoption path is a full resync into a fresh database.
	codexSession, err := database.GetSession(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, codexSession)
	assert.Equal(t, 1, codexSession.MessageCount,
		"the append must not reach the previous agent's session")
}
