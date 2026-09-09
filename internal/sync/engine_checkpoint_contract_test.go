package sync

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexPendingOverflowKeepsHashAndAppendPath(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const stamp = "2024-01-01T10:00:00Z"
	fixture := testjsonl.NewSessionBuilder().AddCodexMeta(stamp, uuid, "/workspace/project-a", "codex_cli_rs").AddCodexMessage(stamp, "user", "run the commands")
	for i := range 10 {
		fixture.AddRaw(testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", fmt.Sprintf("call_%d", i), nil, stamp))
	}
	fixture.AddRaw(`{"timestamp":"2024-01-01T10:00:01Z","type":"event_msg","payload":{"type":"turn_aborted"}}`)
	root := writeCodexTranscriptRoot(t, uuid, fixture.String())
	paths, err := filepath.Glob(filepath.Join(root, "2024", "01", "01", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", Ephemeral: true, DisableFilesystemProjectDiscovery: true})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	hash, ok := database.GetFileHashByAgentPath(paths[0], string(parser.AgentCodex))
	require.True(t, ok)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(fixture.String()))), hash)
	_, hasCheckpoint, err := database.GetParserCheckpoint("codex:" + uuid)
	require.NoError(t, err)
	assert.False(t, hasCheckpoint)
	before, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotEmpty(t, before)
	appendText := testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "continuing", "2024-01-01T10:00:02Z"), testjsonl.CodexFunctionCallOutputJSON("call_9", "finished", "2024-01-01T10:00:03Z"))
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(appendText)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	info, err := os.Stat(paths[0])
	require.NoError(t, err)
	result := engine.processFile(t.Context(), parser.DiscoveredFile{Agent: parser.AgentCodex, Path: paths[0], SourceSize: info.Size()})
	defer result.releaseStaged()
	defer result.retentionLease.Release()
	require.NoError(t, result.err)
	require.NotNil(t, result.incremental, "missing checkpoint must preserve the normal append path")
	assert.False(t, result.forceReplace)
	require.NoError(t, engine.writeIncremental(result.incremental))
	after, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.Greater(t, len(after), len(before))
	assert.Equal(t, before[0].ID, after[0].ID, "the existing transcript was not replaced")
	_, hasCheckpoint, err = database.GetParserCheckpoint("codex:" + uuid)
	require.NoError(t, err)
	assert.False(t, hasCheckpoint, "nine remaining pending calls still exceed the persistent cursor limit")
}
