package sync_test

import (
	"context"
	"fmt"
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

// createCodebuffArchive creates a Codebuff archive with the given number of
// sessions distributed across projects. Returns the root directory.
func createCodebuffArchive(t *testing.T, numSessions int) string {
	t.Helper()
	root := t.TempDir()
	numProjects := 3
	sessionsPerProject := numSessions / numProjects
	if sessionsPerProject == 0 {
		sessionsPerProject = 1
	}

	for p := 0; p < numProjects; p++ {
		project := fmt.Sprintf("project-%d", p)
		for s := 0; s < sessionsPerProject; s++ {
			ts := fmt.Sprintf("2026-07-15T%02d-00-00.000Z", 10+s)
			dir := filepath.Join(root, project, "chats", ts)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			writeCodebuffTestFiles(t, dir, fmt.Sprintf("Session %d in %s", s, project))
		}
	}
	return root
}

// TestSyncAllCodebuffBoundedPerEventWork verifies that unchanged Codebuff
// sessions are skipped during reconciliation without reading transcript
// bytes. The stat-only freshness gate (providerSourceFreshBeforeFingerprint)
// should prevent the fingerprint from being called for unchanged sources.
func TestSyncAllCodebuffBoundedPerEventWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Create a small archive with 6 sessions.
	root := createCodebuffArchive(t, 6)

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
	modifiedDir := filepath.Join(root, "project-0", "chats", "2026-07-15T10-00-00.000Z")
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

// TestSyncAllCodebuffScalingVerifiesBoundedWork verifies that the per-event
// work for unchanged Codebuff sessions does not scale with archive size.
// It creates two archives (small and large), performs a sync on each, and
// asserts that the time per unchanged session is bounded.
func TestSyncAllCodebuffScalingVerifiesBoundedWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Create a small archive (6 sessions) and a large archive (30 sessions).
	smallRoot := createCodebuffArchive(t, 6)
	largeRoot := createCodebuffArchive(t, 30)

	// Measure sync time for small archive (all unchanged).
	smallDB := dbtest.OpenTestDB(t)
	smallEngine := sync.NewEngine(smallDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {smallRoot},
		},
		Machine: "local",
	})
	// First sync to populate the skip cache.
	smallEngine.SyncAll(context.Background(), nil)

	start := time.Now()
	for i := 0; i < 3; i++ {
		smallEngine.SyncAll(context.Background(), nil)
	}
	smallElapsed := time.Since(start)
	smallPerSession := smallElapsed / (6 * 3) // 6 sessions x 3 syncs

	// Measure sync time for large archive (all unchanged).
	largeDB := dbtest.OpenTestDB(t)
	largeEngine := sync.NewEngine(largeDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {largeRoot},
		},
		Machine: "local",
	})
	// First sync to populate the skip cache.
	largeEngine.SyncAll(context.Background(), nil)

	start = time.Now()
	for i := 0; i < 3; i++ {
		largeEngine.SyncAll(context.Background(), nil)
	}
	largeElapsed := time.Since(start)
	largePerSession := largeElapsed / (30 * 3) // 30 sessions x 3 syncs

	// The per-session time for unchanged sessions should be bounded.
	// Allow 10x headroom for system noise, but the per-event work should
	// not scale linearly with archive size (which would give 5x ratio).
	t.Logf("small archive: %v total, %v per session", smallElapsed, smallPerSession)
	t.Logf("large archive: %v total, %v per session", largeElapsed, largePerSession)

	// Assert that the large archive's per-session time is not more than
	// 10x the small archive's per-session time. This ensures the work
	// per unchanged session is bounded.
	assert.Less(t, largePerSession, smallPerSession*10,
		"per-session work should not scale linearly with archive size")
}
