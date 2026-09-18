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
	assert := assert.New(t)
	require := require.New(t)

	const owner = "oldhost.example"
	const identity = "0123456789abcdef0123456789abcdef"
	database := openTestDB(t)
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "project.worktrees", "branch")
	require.NoError(os.MkdirAll(cwd, 0o700))
	_, err := database.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
		Machine: owner, PathPrefix: filepath.Dir(cwd), Project: "mapped-project", Enabled: true,
	})
	require.NoError(err)
	first := writeSessionSourceClaudeFile(t, root, "before.jsonl")
	require.NoError(os.WriteFile(first, []byte(testjsonl.ClaudeUserJSON("before upgrade", "2026-07-01T10:00:00Z", cwd)+"\n"), 0o600))
	oldEngine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: owner})
	t.Cleanup(oldEngine.Close)
	require.Equal(1, oldEngine.SyncAll(t.Context(), nil).Synced)
	oldEngine.Close()
	for id, machine := range map[string]string{"orphan": owner, "peer": "peer.example"} {
		require.NoError(database.UpsertSession(db.Session{ID: id, Machine: machine, Project: "project", Agent: "claude"}))
	}
	require.NoError(database.SetSyncState("artifact_local_machine_name", owner))
	_, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	second := writeSessionSourceClaudeFile(t, root, "after.jsonl")
	require.NoError(os.WriteFile(second, []byte(testjsonl.ClaudeUserJSON("after upgrade", "2026-07-01T11:00:00Z", cwd)+"\n"), 0o600))
	// Reparse existing content as well as ingesting a new session.
	require.NoError(os.WriteFile(first, []byte(testjsonl.ClaudeUserJSON("replacement", "2026-07-01T12:00:00Z", cwd)+"\n"), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: identity})
	t.Cleanup(engine.Close)
	require.Equal(2, engine.SyncAll(t.Context(), nil).Synced)
	require.False(engine.ResyncAll(t.Context(), nil).Aborted)
	_, err = database.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	for _, id := range []string{"before", "after", "orphan"} {
		session, err := database.GetSession(t.Context(), id)
		require.NoError(err)
		require.NotNil(session)
		assert.Equal(identity, session.Machine)
		if id != "orphan" {
			assert.Equal("mapped_project", session.Project)
		}
	}
	rules, err := database.ListWorktreeProjectMappings(t.Context(), identity)
	require.NoError(err)
	require.Len(rules, 1)
	aliases, err := database.GetMachineAliases(t.Context())
	require.NoError(err)
	assert.Equal(identity, aliases[owner])
	machines, err := database.GetMachines(t.Context(), false, false)
	require.NoError(err)
	assert.ElementsMatch([]string{identity, "peer.example"}, machines)
}
