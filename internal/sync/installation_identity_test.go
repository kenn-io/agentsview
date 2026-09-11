package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestInstallationAdoptionPreservesHistoryAndRulesThroughResync(t *testing.T) {
	const owner = "oldhost.example"
	const identity = "0123456789abcdef0123456789abcdef"
	database := openTestDB(t)
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "project.worktrees", "branch")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	_, err := database.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
		Machine: owner, PathPrefix: filepath.Dir(cwd), Project: "mapped-project", Enabled: true,
	})
	require.NoError(t, err)
	first := writeSessionSourceClaudeFile(t, root, "before.jsonl")
	require.NoError(t, os.WriteFile(first, []byte(testjsonl.ClaudeUserJSON("before upgrade", "2026-07-01T10:00:00Z", cwd)+"\n"), 0o600))
	oldEngine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: owner})
	t.Cleanup(oldEngine.Close)
	require.Equal(t, 1, oldEngine.SyncAll(t.Context(), nil).Synced)
	oldEngine.Close()
	for id, machine := range map[string]string{"orphan": owner, "peer": "peer.example"} {
		require.NoError(t, database.UpsertSession(db.Session{ID: id, Machine: machine, Project: "project", Agent: "claude"}))
	}
	require.NoError(t, database.SetSyncState("artifact_local_machine_name", owner))
	require.NoError(t, database.EnsureInstallationIdentity(t.Context(), identity))
	second := writeSessionSourceClaudeFile(t, root, "after.jsonl")
	require.NoError(t, os.WriteFile(second, []byte(testjsonl.ClaudeUserJSON("after upgrade", "2026-07-01T11:00:00Z", cwd)+"\n"), 0o600))
	// Reparse existing content as well as ingesting a new session.
	require.NoError(t, os.WriteFile(first, []byte(testjsonl.ClaudeUserJSON("replacement", "2026-07-01T12:00:00Z", cwd)+"\n"), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: identity})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	require.False(t, engine.ResyncAll(t.Context(), nil).Aborted)
	require.NoError(t, database.EnsureInstallationIdentity(t.Context(), identity))
	for _, id := range []string{"before", "after", "orphan"} {
		session, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, identity, session.Machine)
		if id != "orphan" {
			assert.Equal(t, "mapped_project", session.Project)
		}
	}
	rules, err := database.ListWorktreeProjectMappings(t.Context(), identity)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	aliases, err := database.GetMachineAliases(t.Context())
	require.NoError(t, err)
	assert.Equal(t, identity, aliases[owner])
	machines, err := database.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{identity, "peer.example"}, machines)
}
