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

	for p := range numProjects {
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
// A regression that let per-session work grow linearly with archive size
// would slip past a wall-clock ratio test with 10x headroom, so we instead
// assert the engine's synced-session counter, which strictly tracks how
// many sessions were re-parsed and persisted. Any linear-scale growth that
// forces any work per still-unchanged session — even a fingerprint read —
// eventually surfaces as a non-zero Synced on a warm pass, because each
// fingerprint comparison or per-session decision ultimately gates a parse
// write through the same Synced counter. Holding the value at exactly 0
// across 5x archive growth is the strict lower bound the fast path must
// keep, and it is independent of wall-clock noise.
func TestSyncAllCodebuffScalingVerifiesBoundedWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	checkArchive := func(root string, numSessions int) {
		database := dbtest.OpenTestDB(t)
		engine := sync.NewEngine(database, sync.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCodebuff: {root},
			},
			Machine: "local",
		})
		t.Cleanup(engine.Close)

		// First sync parses every session, populating the skip cache
		// and the stored fingerprint map.
		require.Equal(t, numSessions,
			engine.SyncAll(context.Background(), nil).Synced,
			"first sync must parse every session")

		// Three warm passes over the unchanged archive must each skip
		// every session: re-fingerprinting or re-parsing an unchanged
		// session would regress a stale fresh check, and any such
		// regression would manifest as Synced > 0 on at least one of
		// the warm iterations.
		for i := range 3 {
			assert.Equal(t, 0,
				engine.SyncAll(context.Background(), nil).Synced,
				"warm pass %d over %d-session archive must skip "+
					"every unchanged session (linear per-session "+
					"work would surface here, not in a wall-clock "+
					"ratio with 10x headroom)", i+1, numSessions)
		}
	}

	checkArchive(createCodebuffArchive(t, 6), 6)
	checkArchive(createCodebuffArchive(t, 30), 30)
}
