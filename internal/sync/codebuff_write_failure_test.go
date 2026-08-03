package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// seedCodebuffSingleSession mirrors createCodebuffSingleSession (which lives
// in the external sync_test package and cannot be reused here) so this
// internal-package test can exercise the unexported writeBatchOverride seam.
// It creates exactly one Codebuff session at
// <root>/project-0/chats/2026-07-15T10-00-00.000Z/ and returns the archive
// root plus the canonical chat-messages.json path.
func seedCodebuffSingleSession(t *testing.T) (root, chatPath string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "project-0", "chats", "2026-07-15T10-00-00.000Z")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "chat-messages.json"),
		[]byte(`[
		{"id":"user-1","variant":"user","content":"Single source","timestamp":"03:04 PM"}
	]`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "run-state.json"),
		[]byte(`{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-deepseek"}
		}
	}`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "chat-meta.json"),
		[]byte(`{
		"messageCount": 1,
		"firstPrompt": "Single source",
		"messagesSize": 50
	}`), 0o644))
	return root, filepath.Join(dir, "chat-messages.json")
}

// TestSyncCodebuffWriteFailureDoesNotPersistStatHash pins the roborev-high
// freshness finding: the provider_freshness digest must never be persisted
// before an outcome the engine can trust. A transient archive-write failure
// after discovery and parse — but before any session row commits — must leave
// no digest behind; a matching digest stamped pre-write would make every
// later sync short-circuit the source and permanently suppress the retry.
// The retry must then succeed through the normal path and stamp the digest
// only via the successful-write flush gate.
//
// The seeded session is Freebuff-classified: seedCodebuffSingleSession writes
// run-state.json with agentType "base2-free-deepseek", which the parser maps
// to AgentFreebuff, so the session row is stored under agent=freebuff (as a
// probe verified). Freebuff shares the Codebuff provider and hasher, and the
// side-table digest is only ever stamped under the canonical AgentCodebuff
// key (canonicalProviderStatHashAgent), so this single test covers the
// write-failure invariant for both agent labels — a separate Freebuff test
// would exercise identical code paths with no additional coverage.
func TestSyncCodebuffWriteFailureDoesNotPersistStatHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	database := dbtest.OpenTestDB(t)
	root, chatPath := seedCodebuffSingleSession(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	// Inject a transient archive-write failure: discovery and parse run
	// normally, but every pending row fails to commit (written=0, failed=n),
	// so outcome.written[i] stays false and the flush digest gate skips the
	// recordProviderStatHash persist for every row in the batch.
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		return 0, 0, len(batch), 0
	}

	failed := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, failed.Failed,
		"the injected archive write must fail and be counted as failed")
	assert.Zero(t, failed.Synced,
		"no session may be reported synced when the write failed")

	_, has, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.False(t, has,
		"a failed write must not persist provider_freshness; a matching "+
			"digest stamped before a confirmed outcome would suppress "+
			"every later retry of this source")

	// Clear the injected failure and re-sync. The retry must not be
	// suppressed — the session must now parse and commit, and the
	// successful-write flush gate must stamp the digest.
	engine.writeBatchOverride = nil
	retry := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, retry.Synced,
		"the retry after a transient write failure must parse and store "+
			"the session; a skip here means a digest was stamped before "+
			"the write outcome was confirmed")

	_, hasAfter, err := database.GetProviderStatHash(
		context.Background(), parser.AgentCodebuff, chatPath,
	)
	require.NoError(t, err)
	require.True(t, hasAfter,
		"the successful retry write must persist provider_freshness")
}
