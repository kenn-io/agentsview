package sync

import (
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestStagedImportHonorsDisabledSignalRecomputation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.Zero(stats.Failed)
	require.Equal(1, stats.Synced)
	sess, err := database.GetSessionFull(t.Context(), "codex:"+uuid)
	require.NoError(err)
	require.NotNil(sess)
	assert.Zero(sess.ToolFailureSignalCount)
	assert.Zero(sess.SecretLeakCount)
	findings, err := database.SessionSecretFindings(t.Context(), "codex:"+uuid)
	require.NoError(err)
	assert.Empty(findings)
	_, hasState, err := database.GetSessionSignalState("codex:" + uuid)
	require.NoError(err)
	assert.False(hasState)
	msgs, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
	require.NoError(err)
	require.NotEmpty(msgs, "disabling derived work still publishes transcript content")
}

func TestClaudeImportDoesNotBuildCodexSignalState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	require.NoError(os.MkdirAll(project, 0o755))
	fixture := testjsonl.NewSessionBuilder().AddClaudeUser("2026-07-10T07:00:00Z", "hello").AddClaudeAssistant("2026-07-10T07:00:01Z", "finished")
	require.NoError(os.WriteFile(filepath.Join(project, "session-a.jsonl"), []byte(fixture.String()), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", Ephemeral: true, DisableFilesystemProjectDiscovery: true})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(stats.Failed)
	require.Equal(1, stats.Synced)
	var sessionID string
	require.NoError(database.Reader().QueryRow("SELECT id FROM sessions").Scan(&sessionID))
	_, exists, err := database.GetSessionSignalState(sessionID)
	require.NoError(err)
	assert.False(exists, "only checkpoint-backed Codex appends consume compact state")
	_, err = engine.recomputeSignalsFromDB(t.Context(), sessionID)
	require.NoError(err)
	_, exists, err = database.GetSessionSignalState(sessionID)
	require.NoError(err)
	assert.False(exists)
	sess, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(err)
	require.NotNil(sess)
	assert.NotEmpty(sess.SecretsRulesVersion, "normal derived signals are still computed")
}
