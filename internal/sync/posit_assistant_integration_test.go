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

func writeSyncPositAssistantFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// TestSyncPositAssistantSidecarAppendResyncsWithoutWatchEvents proves the
// non-watcher sync path picks up a usage-events.jsonl append when nothing
// else about the conversation changed. Every other source file is aged well
// past the first sync, so the session only resyncs if the sidecar drives the
// provider fingerprint — both its mtime (the incremental cutoff signal) and
// its content hash (the skip gate).
func TestSyncPositAssistantSidecarAppendResyncsWithoutWatchEvents(t *testing.T) {
	root := t.TempDir()
	convID := "11111111-1111-4111-8111-111111111111"
	convDir := filepath.Join(root, "ws1", convID)
	wsPath := filepath.Join(root, "ws1", "workspace.json")
	convPath := filepath.Join(convDir, "conversation.json")
	lmPath := filepath.Join(convDir, "lm-messages.jsonl")

	writeSyncPositAssistantFile(t, wsPath, `{"path":"/work/acme-app"}`)
	writeSyncPositAssistantFile(t, convPath, `{
		"schemaVersion": "3",
		"root": {"id": "`+convID+`",
			"timestamp": 1735689600000, "metadata": {"kind": "main"}},
		"messages": [
			{"id": "n1", "parentId": "`+convID+`", "isActive": true,
				"lmMessageIds": [0, 1], "timestamp": 1735689600000}
		],
		"files": []
	}`)
	writeSyncPositAssistantFile(t, lmPath,
		`{"id":0,"message":{"role":"user","content":"Inspect the auth flow."}}`+"\n"+
			`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"Looking."}],"providerOptions":{"providerMetadata":{"positai":{"timestamp":1735689601000,"modelId":"claude-sonnet-4-6","providerId":"anthropic","usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":100}}}}}}`+"\n")

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositAssistant: {root},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})
	sessionID := "posit-assistant:" + convID
	usage, err := database.GetUsageEvents(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, usage, "no sidecar yet, no usage events")

	// Sidecar-only change: no other file is touched, so the session resyncs
	// only if the sidecar participates in the provider fingerprint.
	writeSyncPositAssistantFile(t,
		filepath.Join(convDir, "usage-events.jsonl"),
		`{"type":"usage","kind":"keepalive","timestamp":1735693200000,"anchorMessageId":"n1","providerId":"anthropic","modelId":"claude-sonnet-4-6","inputTokens":2,"outputTokens":1,"totalTokens":24642,"cacheReadTokens":24639,"cacheWriteTokens":0}`+"\n")

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})
	usage, err = database.GetUsageEvents(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, usage, 1,
		"a sidecar-only append must resync the session without a watch event")
	assert.Equal(t, "posit-assistant-keepalive", usage[0].Source)
	assert.Equal(t, 24639, usage[0].CacheReadInputTokens)
	assert.Equal(t, 2, usage[0].InputTokens)
}
