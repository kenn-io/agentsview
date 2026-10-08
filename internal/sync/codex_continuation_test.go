package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const codexThreadID = "019faa49-a61a-7282-8376-12dd025a5f0c"

// codexRollout renders a Codex rollout with a session_meta line carrying
// threadID and the requested number of alternating user/assistant messages.
func codexRollout(threadID string, messages int) string {
	builder := testjsonl.NewSessionBuilder().
		AddCodexMeta("2024-01-01T10:00:00Z", threadID, "/Users/alice/code", "user")
	for i := range messages {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		builder = builder.AddCodexMessage(
			"2024-01-01T10:00:0"+string(rune('1'+i))+"Z", role, "message "+string(rune('a'+i)),
		)
	}
	return builder.String()
}

// codexRootEnv returns an environment whose Codex roots are a live sessions
// directory and an archived_sessions directory.
func codexRootEnv(t *testing.T) (env *testEnv, liveDir, archivedDir string) {
	t.Helper()
	root := t.TempDir()
	liveDir = filepath.Join(root, "sessions")
	archivedDir = filepath.Join(root, "archived_sessions")
	require.NoError(t, os.MkdirAll(liveDir, 0o755))
	require.NoError(t, os.MkdirAll(archivedDir, 0o755))
	return setupTestEnv(t, WithCodexDirs([]string{liveDir, archivedDir})), liveDir, archivedDir
}

// A paginated continuation rollout carries the thread's session id in its
// session_meta payload. Syncing it after the base rollout must keep both files:
// the stored transcript must not shrink to the continuation's shorter content.
func TestCodexContinuationKeepsBothFiles(t *testing.T) {
	tests := []struct {
		name       string
		base, next int
	}{
		{name: "shorter continuation after the base", base: 5, next: 1},
		{name: "longer continuation after the base", base: 2, next: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupTestEnv(t)
			day := filepath.Join("2026", "07", "28")
			sessionID := "codex:" + codexThreadID

			basePath := env.writeCodexSession(
				t, day, "rollout-2026-07-28T14-53-01-"+codexThreadID+".jsonl",
				codexRollout(codexThreadID, tt.base),
			)
			env.engine.SyncAll(t.Context(), nil)
			assertSessionMessageCount(t, env.db, sessionID, tt.base)

			continuationPath := env.writeCodexSession(
				t, day, "rollout-2026-07-28T15-00-00-"+codexThreadID+"_abcdef.jsonl",
				codexRollout(codexThreadID, tt.next),
			)
			env.engine.SyncAll(t.Context(), nil)

			base := requireStoredSession(t, env.db, sessionID)
			require.NotNil(t, base.FilePath)
			assert.Equal(t, basePath, *base.FilePath)
			assertSessionMessageCount(t, env.db, sessionID, tt.base)

			altID := parser.AltSessionID(sessionID, continuationPath)
			alt := requireStoredSession(t, env.db, altID)
			require.NotNil(t, alt.FilePath)
			assert.Equal(t, continuationPath, *alt.FilePath)
			require.NotNil(t, alt.ParentSessionID)
			assert.Equal(t, sessionID, *alt.ParentSessionID)
			assert.Equal(t, string(parser.RelContinuation), alt.RelationshipType)
			assertSessionMessageCount(t, env.db, altID, tt.next)
		})
	}
}

// requireSessionGone asserts that a session id has no stored row.
func requireSessionGone(t *testing.T, env *testEnv, id string) {
	t.Helper()
	sess, err := env.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Nilf(t, sess, "session %s must stay deleted", id)
}

// requireNoStoredRecords asserts that neither the base id nor any derived id
// has a stored row, so a deleted thread cannot come back under a new id.
func requireNoStoredRecords(t *testing.T, env *testEnv, baseID string) {
	t.Helper()
	records, err := env.db.ListSessionPathRecords(t.Context(), baseID)
	require.NoError(t, err)
	for _, r := range records {
		assert.Truef(t, r.Excluded, "record %s at %q must stay excluded", r.ID, r.FilePath)
	}
}

// requireNoLiveDerivedRecords asserts that every derived id for baseID is
// excluded, so a deleted derived session cannot come back under a new id while
// the base session is still stored.
func requireNoLiveDerivedRecords(t *testing.T, env *testEnv, baseID string) {
	t.Helper()
	records, err := env.db.ListSessionPathRecords(t.Context(), baseID)
	require.NoError(t, err)
	for _, r := range records {
		if r.ID == baseID {
			continue
		}
		assert.Truef(t, r.Excluded, "derived record %s at %q must stay excluded", r.ID, r.FilePath)
	}
}

// A permanently deleted Codex session stays deleted when its rollout moves
// between the live and archived roots. Both paths name the same thread, so the
// move must not resurrect the session under the base id or a derived id.
func TestCodexDeletedSessionStaysDeletedAfterRootMove(t *testing.T) {
	for _, mode := range []string{"sync", "resync"} {
		t.Run(mode, func(t *testing.T) {
			env, _, archivedDir := codexRootEnv(t)
			sessionID := "codex:" + codexThreadID

			livePath := env.writeCodexSession(
				t, filepath.Join("2026", "07", "28"),
				"rollout-2026-07-28T14-53-01-"+codexThreadID+".jsonl",
				codexRollout(codexThreadID, 3),
			)
			env.engine.SyncAll(t.Context(), nil)
			assertSessionMessageCount(t, env.db, sessionID, 3)

			require.NoError(t, env.db.DeleteSession(t.Context(), sessionID))

			require.NoError(t, os.Remove(livePath))
			env.writeSession(
				t, archivedDir, "rollout-2026-07-28T15-00-00-"+codexThreadID+".jsonl",
				codexRollout(codexThreadID, 3),
			)

			if mode == "resync" {
				stats := env.engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "%v", stats.Warnings)
			} else {
				env.engine.SyncAll(t.Context(), nil)
			}

			requireSessionGone(t, env, sessionID)
			requireNoStoredRecords(t, env, sessionID)
		})
	}
}

// When the continuation is imported first it keeps the base id and the ordinary
// rollout is stored under a derived id. Permanently deleting that derived row
// and moving the ordinary rollout to the archive root must not bring it back.
func TestCodexDeletedDerivedRolloutStaysDeletedAfterRootMove(t *testing.T) {
	env, _, archivedDir := codexRootEnv(t)
	sessionID := "codex:" + codexThreadID
	day := filepath.Join("2026", "07", "28")

	env.writeCodexSession(
		t, day, "rollout-2026-07-28T15-00-00-"+codexThreadID+"_abcdef.jsonl",
		codexRollout(codexThreadID, 1),
	)
	env.engine.SyncAll(t.Context(), nil)
	requireStoredSession(t, env.db, sessionID)

	ordinaryPath := env.writeCodexSession(
		t, day, "rollout-2026-07-28T14-53-01-"+codexThreadID+".jsonl",
		codexRollout(codexThreadID, 3),
	)
	env.engine.SyncAll(t.Context(), nil)
	derivedID := parser.AltSessionID(sessionID, ordinaryPath)
	requireStoredSession(t, env.db, derivedID)

	require.NoError(t, env.db.DeleteSession(t.Context(), derivedID))

	require.NoError(t, os.Remove(ordinaryPath))
	env.writeSession(
		t, archivedDir, "rollout-2026-07-28T14-53-01-"+codexThreadID+".jsonl",
		codexRollout(codexThreadID, 3),
	)
	env.engine.SyncAll(t.Context(), nil)

	requireSessionGone(t, env, derivedID)
	requireNoLiveDerivedRecords(t, env, sessionID)
}

// Permanently deleting a continuation stored under a derived id and moving its
// rollout to the archive root must not resurrect the transcript under a new id.
func TestCodexDeletedContinuationStaysDeletedAfterRootMove(t *testing.T) {
	env, _, archivedDir := codexRootEnv(t)
	sessionID := "codex:" + codexThreadID
	day := filepath.Join("2026", "07", "28")

	env.writeCodexSession(
		t, day, "rollout-2026-07-28T14-53-01-"+codexThreadID+".jsonl",
		codexRollout(codexThreadID, 3),
	)
	env.engine.SyncAll(t.Context(), nil)

	continuationPath := env.writeCodexSession(
		t, day, "rollout-2026-07-28T15-00-00-"+codexThreadID+"_abcdef.jsonl",
		codexRollout(codexThreadID, 1),
	)
	env.engine.SyncAll(t.Context(), nil)
	altID := parser.AltSessionID(sessionID, continuationPath)
	requireStoredSession(t, env.db, altID)

	require.NoError(t, env.db.DeleteSession(t.Context(), altID))

	require.NoError(t, os.Remove(continuationPath))
	env.writeSession(
		t, archivedDir, "rollout-2026-07-28T15-00-00-"+codexThreadID+"_abcdef.jsonl",
		codexRollout(codexThreadID, 1),
	)
	env.engine.SyncAll(t.Context(), nil)

	requireSessionGone(t, env, altID)
	requireNoLiveDerivedRecords(t, env, sessionID)
}
