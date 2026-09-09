package sync

import (
	"go.kenn.io/agentsview/internal/testjsonl"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestStagedImportHonorsDisabledSignalRecomputation(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122e01"
	database := openTestDB(t)
	root := writeCodexTranscriptRoot(t, uuid, codexParityTranscript(uuid))
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local", Ephemeral: true,
		StagedCodexParseMinBytes: 1, DisableSignalRecomputation: true,
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	sess, err := database.GetSessionFull(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Zero(t, sess.ToolFailureSignalCount)
	assert.Zero(t, sess.SecretLeakCount)
	findings, err := database.SessionSecretFindings(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	assert.Empty(t, findings)
	_, hasState, err := database.GetSessionSignalState("codex:" + uuid)
	require.NoError(t, err)
	assert.False(t, hasState)
	msgs, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "disabling derived work still publishes transcript content")
}

func TestClaudeImportDoesNotBuildCodexSignalState(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(project, 0o755))
	fixture := testjsonl.NewSessionBuilder().AddClaudeUser("2026-07-10T07:00:00Z", "hello").AddClaudeAssistant("2026-07-10T07:00:01Z", "finished")
	require.NoError(t, os.WriteFile(filepath.Join(project, "session-a.jsonl"), []byte(fixture.String()), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", Ephemeral: true, DisableFilesystemProjectDiscovery: true})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	var sessionID string
	require.NoError(t, database.Reader().QueryRow("SELECT id FROM sessions").Scan(&sessionID))
	_, exists, err := database.GetSessionSignalState(sessionID)
	require.NoError(t, err)
	assert.False(t, exists, "only checkpoint-backed Codex appends consume compact state")
	_, err = engine.recomputeSignalsFromDB(t.Context(), sessionID)
	require.NoError(t, err)
	_, exists, err = database.GetSessionSignalState(sessionID)
	require.NoError(t, err)
	assert.False(t, exists)
	sess, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.NotEmpty(t, sess.SecretsRulesVersion, "normal derived signals are still computed")
}
