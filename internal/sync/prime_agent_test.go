package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEngineSyncPrimeAgentLateAttributionForceReplacesUsage(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPrimeAgent: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "transcript-file-id.jsonl")
	initial := `{"type":"session","version":3,"id":"session-header-id","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}
{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-06T12:00:01Z","message":{"role":"user","content":"hello"}}
{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-06T12:00:02Z","message":{"role":"assistant","content":"hi","model":"gpt-5.4-mini","usage":{"input":10,"output":1}}}
`
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))

	stats := engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted)
	messages, err := database.GetAllMessages(
		context.Background(), "prime-agent:session-header-id",
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, 10, messages[1].ContextTokens)
	assert.Equal(t, 1, messages[1].OutputTokens)

	appended := `{"type":"child_usage_attributed","id":"usage-1","parentId":"assistant-1","timestamp":"2026-08-06T12:00:03Z","targetId":"assistant-1","aggregateUsage":{"input":30,"output":7,"cacheRead":5,"cacheWrite":2}}
`
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	engine.SyncPaths([]string{path})
	messages, err = database.GetAllMessages(
		context.Background(), "prime-agent:session-header-id",
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, 37, messages[1].ContextTokens)
	assert.Equal(t, 7, messages[1].OutputTokens)
	assert.JSONEq(t, `{
		"input_tokens": 30,
		"output_tokens": 7,
		"cache_read_input_tokens": 5,
		"cache_creation_input_tokens": 2
	}`, string(messages[1].TokenUsage))
}
