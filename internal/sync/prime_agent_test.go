package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEngineSyncPrimeAgentLateAttributionForceReplacesUsage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPrimeAgent: {root},
		},
		Machine: "local",
	})
	require.NoError(database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "late-attribution-model",
		InputPerMTok:         money.Money{Microdollars: 1_000_000},
		OutputPerMTok:        money.Money{Microdollars: 2_000_000},
		CacheCreationPerMTok: money.Money{Microdollars: 3_000_000},
		CacheReadPerMTok:     money.Money{Microdollars: 4_000_000},
	}}))

	path := filepath.Join(root, "transcript-file-id.jsonl")
	initial := `{"type":"session","version":3,"id":"session-header-id","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}
{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-06T12:00:01Z","message":{"role":"user","content":"hello"}}
{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-06T12:00:02Z","message":{"role":"assistant","content":"hi","model":"late-attribution-model","usage":{"input":10,"output":1}}}
`
	require.NoError(os.WriteFile(path, []byte(initial), 0o600))

	stats := engine.SyncAll(t.Context(), nil)
	require.False(stats.Aborted)
	messages, err := database.GetAllMessages(
		t.Context(), "prime-agent:session-header-id",
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal(10, messages[1].ContextTokens)
	assert.Equal(1, messages[1].OutputTokens)

	appended := `{"type":"child_usage_attributed","id":"usage-1","parentId":"assistant-1","timestamp":"2026-08-06T12:00:03Z","targetId":"assistant-1","aggregateUsage":{"input":30,"output":7,"cacheRead":5,"cacheWrite":2}}
`
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(err)
	_, err = file.WriteString(appended)
	require.NoError(err)
	require.NoError(file.Close())

	engine.SyncPaths([]string{path})
	messages, err = database.GetAllMessages(
		t.Context(), "prime-agent:session-header-id",
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal(37, messages[1].ContextTokens)
	assert.Equal(7, messages[1].OutputTokens)
	assert.JSONEq(`{
		"input_tokens": 30,
		"output_tokens": 7,
		"cache_read_input_tokens": 5,
		"cache_creation_input_tokens": 2
	}`, string(messages[1].TokenUsage))
	session, err := database.GetSessionFull(
		t.Context(), "prime-agent:session-header-id",
	)
	require.NoError(err)
	require.NotNil(session)
	assert.Equal(37, session.PeakContextTokens)
	assert.Equal(7, session.TotalOutputTokens)
	usage, err := database.GetSessionUsage(
		t.Context(), "prime-agent:session-header-id", true,
	)
	require.NoError(err)
	require.NotNil(usage)
	assert.True(usage.HasCost)
	assert.Equal(money.Money{Microdollars: 70}, usage.Cost)
}

func TestEngineSyncPrimeAgentStoresForkRelationship(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPrimeAgent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	parentPath := filepath.Join(root, "parent-file-id.jsonl")
	childPath := filepath.Join(root, "child-file-id.jsonl")
	require.NoError(os.WriteFile(parentPath, []byte(`{"type":"session","version":3,"id":"parent-header-id","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}
{"type":"message","id":"parent-user","parentId":null,"timestamp":"2026-08-06T12:00:01Z","message":{"role":"user","content":"parent"}}
`), 0o600))
	require.NoError(os.WriteFile(childPath, []byte(`{"type":"session","version":3,"id":"child-header-id","timestamp":"2026-08-06T12:01:00Z","cwd":"/work/project","parentSession":"/stale/source/parent-file-id.jsonl"}
{"type":"message","id":"child-user","parentId":null,"timestamp":"2026-08-06T12:01:01Z","message":{"role":"user","content":"child"}}
`), 0o600))

	stats := engine.SyncAll(t.Context(), nil)
	require.False(stats.Aborted)
	child, err := database.GetSessionFull(
		t.Context(), "prime-agent:child-header-id",
	)
	require.NoError(err)
	require.NotNil(child)
	require.NotNil(child.ParentSessionID)
	assert.Equal("prime-agent:parent-header-id", *child.ParentSessionID)
	assert.Equal(string(parser.RelFork), child.RelationshipType)
}

func TestEngineSyncPrimeAgentSingleSessionResyncsResolvedPath(t *testing.T) {
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPrimeAgent: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	path := filepath.Join(root, "transcript-file-id.jsonl")
	initial := `{"type":"session","version":3,"id":"session-header-id","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}
{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-06T12:00:01Z","message":{"role":"user","content":"first"}}
`
	require.NoError(os.WriteFile(path, []byte(initial), 0o600))
	stats := engine.SyncAll(t.Context(), nil)
	require.False(stats.Aborted)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(err)
	_, err = file.WriteString(
		`{"type":"message","id":"user-2","parentId":"user-1","timestamp":"2026-08-06T12:00:02Z","message":{"role":"user","content":"second"}}` + "\n",
	)
	require.NoError(err)
	require.NoError(file.Close())

	require.NoError(engine.SyncSingleSession("prime-agent:session-header-id"))
	messages, err := database.GetAllMessages(
		t.Context(), "prime-agent:session-header-id",
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal(t, "second", messages[1].Content)
}
