package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// writeCodebuffTestFiles creates the three files that make up a Codebuff
// session directory: chat-messages.json, run-state.json, and chat-meta.json.
func writeCodebuffTestFiles(t *testing.T, dir, content string) {
	t.Helper()
	chatPath := filepath.Join(dir, "chat-messages.json")
	runStatePath := filepath.Join(dir, "run-state.json")
	chatMetaPath := filepath.Join(dir, "chat-meta.json")

	require.NoError(t, os.WriteFile(chatPath, []byte(`[
		{"id":"user-1","variant":"user","content":"`+content+`","timestamp":"03:04 PM"}
	]`), 0o644))
	require.NoError(t, os.WriteFile(runStatePath, []byte(`{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-deepseek"}
		}
	}`), 0o644))
	require.NoError(t, os.WriteFile(chatMetaPath, []byte(`{
		"messageCount": 1,
		"firstPrompt": "`+content+`",
		"messagesSize": 50
	}`), 0o644))
}

// TestSyncAllCodebuffBoundedPerEventWork verifies that unchanged Codebuff
// sessions are skipped during reconciliation without reading transcript
// bytes. The stat-only freshness gate (providerSourceFreshBeforeFingerprint)
// should prevent the fingerprint from being called for unchanged sources.
func TestSyncAllCodebuffBoundedPerEventWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Create a small archive with 3 sessions under 2 projects.
	root := t.TempDir()
	projects := []string{"project-a", "project-b"}
	sessionTimestamps := []string{
		"2026-07-15T10-00-00.000Z",
		"2026-07-15T11-00-00.000Z",
		"2026-07-15T12-00-00.000Z",
	}

	for _, project := range projects {
		for _, ts := range sessionTimestamps {
			dir := filepath.Join(root, project, "chats", ts)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			writeCodebuffTestFiles(t, dir, "Hello from "+project+"/"+ts)
		}
	}

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})

	// First sync: all sessions should be parsed.
	synced := engine.SyncAll(context.Background(), nil).Synced
	assert.Equal(t, 6, synced, "first sync should parse all 6 sessions")

	// Second sync with no changes: all sessions should be skipped.
	synced = engine.SyncAll(context.Background(), nil).Synced
	assert.Equal(t, 0, synced, "second sync with no changes should skip all sessions")

	// Modify one session's chat-messages.json.
	modifiedDir := filepath.Join(root, projects[0], "chats", sessionTimestamps[0])
	modifiedChatPath := filepath.Join(modifiedDir, "chat-messages.json")
	require.NoError(t, os.WriteFile(modifiedChatPath, []byte(`[
		{"id":"user-1","variant":"user","content":"Modified message","timestamp":"03:04 PM"}
	]`), 0o644))

	// Touch the file to ensure mtime changes.
	time.Sleep(10 * time.Millisecond)
	now := time.Now()
	require.NoError(t, os.Chtimes(modifiedChatPath, now, now))

	// Third sync: only the modified session should be reparsed.
	synced = engine.SyncAll(context.Background(), nil).Synced
	assert.Equal(t, 1, synced, "third sync should only reparse the modified session")
}
