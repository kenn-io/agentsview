package sync_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	codexThreadID        = "019faa49-a61a-7282-8376-12dd025a5f0c"
	codexRevertRolloutID = "019faa50-0000-7000-8000-000000000001"
	codexOrdinaryName    = "rollout-2026-07-28T14-53-01-" + codexThreadID + ".jsonl"
	// codexRevertedName is the file Codex writes when it reverts the thread:
	// the thread id stays in the name and the new rollout id follows it.
	codexRevertedName = "rollout-2026-07-28T15-00-00-" + codexThreadID + "_" +
		codexRevertRolloutID + ".jsonl"
)

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
			fmt.Sprintf("2024-01-01T10:%02d:00Z", i+1), role, fmt.Sprintf("message %d", i),
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

// A reverted Codex thread gets a second rollout whose session_meta carries the
// thread's id. Syncing it after the ordinary rollout must keep both files, on
// sync and on resync: the stored transcript must not shrink to the other
// file's content.
func TestCodexRevertedRolloutKeepsBothFiles(t *testing.T) {
	tests := []struct {
		name           string
		base, reverted int
	}{
		{name: "shorter reverted rollout", base: 5, reverted: 1},
		{name: "longer reverted rollout", base: 2, reverted: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupTestEnv(t)
			day := filepath.Join("2026", "07", "28")
			sessionID := "codex:" + codexThreadID

			basePath := env.writeCodexSession(
				t, day, codexOrdinaryName, codexRollout(codexThreadID, tt.base),
			)
			env.engine.SyncAll(t.Context(), nil)
			assertSessionMessageCount(t, env.db, sessionID, tt.base)

			revertedPath := env.writeCodexSession(
				t, day, codexRevertedName, codexRollout(codexThreadID, tt.reverted),
			)
			env.engine.SyncAll(t.Context(), nil)
			altID := parser.AltSessionID(sessionID, revertedPath)
			assertBothRollouts := func(t *testing.T) {
				t.Helper()
				base := requireStoredSession(t, env.db, sessionID)
				require.NotNil(t, base.FilePath)
				assert.Equal(t, basePath, *base.FilePath)
				assertSessionMessageCount(t, env.db, sessionID, tt.base)

				alt := requireStoredSession(t, env.db, altID)
				require.NotNil(t, alt.FilePath)
				assert.Equal(t, revertedPath, *alt.FilePath)
				require.NotNil(t, alt.ParentSessionID)
				assert.Equal(t, sessionID, *alt.ParentSessionID)
				assert.Equal(t, string(parser.RelContinuation), alt.RelationshipType)
				assertSessionMessageCount(t, env.db, altID, tt.reverted)
			}
			assertBothRollouts(t)

			stats := env.engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "%v", stats.Warnings)
			assertBothRollouts(t)
		})
	}
}

// writeCodexThreadName appends a rename to the session index Codex keeps
// beside its sessions root.
func writeCodexThreadName(t *testing.T, sessionsDir, name string, at time.Time) {
	t.Helper()
	path := filepath.Join(filepath.Dir(sessionsDir), parser.CodexSessionIndexFilename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = fmt.Fprintf(f,
		`{"id":%q,"thread_name":%q,"updated_at":"2026-07-28T17:34:20Z"}`+"\n",
		codexThreadID, name)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Chtimes(path, at, at))
}

// requireSessionName asserts the stored thread name of a session.
func requireSessionName(t *testing.T, env *testEnv, id, want string) {
	t.Helper()
	sess := requireStoredSession(t, env.db, id)
	require.NotNilf(t, sess.SessionName, "session %s name", id)
	assert.Equalf(t, want, *sess.SessionName, "session %s name", id)
}

// Renaming a reverted thread refreshes both of its rollouts' rows, whichever
// rollout synced first and so holds the thread's id. A full sync and a
// watcher event that delivers only the index both refresh them.
func TestCodexRevertedRolloutRefreshesThreadName(t *testing.T) {
	for _, tc := range []struct {
		revertedFirst bool
		indexEvent    bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		revertedFirst := tc.revertedFirst
		name := fmt.Sprintf("reverted first %t index event %t", revertedFirst, tc.indexEvent)
		t.Run(name, func(t *testing.T) {
			env, liveDir, _ := codexRootEnv(t)
			sessionID := "codex:" + codexThreadID
			day := filepath.Join("2026", "07", "28")
			writeCodexThreadName(t, liveDir, "First", time.Now().Add(-time.Hour))

			names := []string{codexOrdinaryName, codexRevertedName}
			if revertedFirst {
				names[0], names[1] = names[1], names[0]
			}
			env.writeCodexSession(t, day, names[0], codexRollout(codexThreadID, 3))
			env.engine.SyncAll(t.Context(), nil)
			secondPath := env.writeCodexSession(t, day, names[1], codexRollout(codexThreadID, 1))
			env.engine.SyncAll(t.Context(), nil)
			altID := parser.AltSessionID(sessionID, secondPath)
			requireSessionName(t, env, sessionID, "First")
			requireSessionName(t, env, altID, "First")

			writeCodexThreadName(t, liveDir, "Renamed", time.Now())
			if tc.indexEvent {
				env.engine.SyncPaths([]string{
					filepath.Join(filepath.Dir(liveDir), parser.CodexSessionIndexFilename),
				})
			} else {
				env.engine.SyncAll(t.Context(), nil)
			}

			requireSessionName(t, env, sessionID, "Renamed")
			requireSessionName(t, env, altID, "Renamed")
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
				t, filepath.Join("2026", "07", "28"), codexOrdinaryName,
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

// When the reverted rollout is imported first it keeps the base id and the
// ordinary rollout is stored under a derived id. Permanently deleting that
// derived row and moving the ordinary rollout to the archive root must not
// bring it back.
func TestCodexDeletedDerivedRolloutStaysDeletedAfterRootMove(t *testing.T) {
	env, _, archivedDir := codexRootEnv(t)
	sessionID := "codex:" + codexThreadID
	day := filepath.Join("2026", "07", "28")

	env.writeCodexSession(t, day, codexRevertedName, codexRollout(codexThreadID, 1))
	env.engine.SyncAll(t.Context(), nil)
	requireStoredSession(t, env.db, sessionID)

	ordinaryPath := env.writeCodexSession(
		t, day, codexOrdinaryName, codexRollout(codexThreadID, 3),
	)
	env.engine.SyncAll(t.Context(), nil)
	derivedID := parser.AltSessionID(sessionID, ordinaryPath)
	requireStoredSession(t, env.db, derivedID)

	require.NoError(t, env.db.DeleteSession(t.Context(), derivedID))

	require.NoError(t, os.Remove(ordinaryPath))
	env.writeSession(t, archivedDir, codexOrdinaryName, codexRollout(codexThreadID, 3))
	env.engine.SyncAll(t.Context(), nil)

	requireSessionGone(t, env, derivedID)
	requireNoLiveDerivedRecords(t, env, sessionID)
}

// Permanently deleting a reverted rollout stored under a derived id and moving
// it to the archive root must not resurrect the transcript under a new id.
func TestCodexDeletedRevertedRolloutStaysDeletedAfterRootMove(t *testing.T) {
	env, _, archivedDir := codexRootEnv(t)
	sessionID := "codex:" + codexThreadID
	day := filepath.Join("2026", "07", "28")

	env.writeCodexSession(t, day, codexOrdinaryName, codexRollout(codexThreadID, 3))
	env.engine.SyncAll(t.Context(), nil)

	revertedPath := env.writeCodexSession(
		t, day, codexRevertedName, codexRollout(codexThreadID, 1),
	)
	env.engine.SyncAll(t.Context(), nil)
	altID := parser.AltSessionID(sessionID, revertedPath)
	requireStoredSession(t, env.db, altID)

	require.NoError(t, env.db.DeleteSession(t.Context(), altID))

	require.NoError(t, os.Remove(revertedPath))
	env.writeSession(t, archivedDir, codexRevertedName, codexRollout(codexThreadID, 1))
	env.engine.SyncAll(t.Context(), nil)

	requireSessionGone(t, env, altID)
	requireNoLiveDerivedRecords(t, env, sessionID)
}

// A reverted rollout is a different file from the thread's stored rollout,
// never its replacement. When the stored rollout's file is gone, a longer
// reverted rollout must still be stored under a derived id so the archived
// conversation keeps its messages.
func TestCodexRevertedRolloutDoesNotClaimMissingRollout(t *testing.T) {
	for _, mode := range []string{"sync", "resync"} {
		t.Run(mode, func(t *testing.T) {
			env := setupTestEnv(t)
			day := filepath.Join("2026", "07", "28")
			sessionID := "codex:" + codexThreadID

			basePath := env.writeCodexSession(t, day, codexOrdinaryName, codexRollout(codexThreadID, 2))
			env.engine.SyncAll(t.Context(), nil)
			require.NoError(t, os.Remove(basePath))
			revertedPath := env.writeCodexSession(
				t, day, codexRevertedName, codexRollout(codexThreadID, 4),
			)
			if mode == "resync" {
				stats := env.engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "%v", stats.Warnings)
			} else {
				env.engine.SyncAll(t.Context(), nil)
			}

			base := requireStoredSession(t, env.db, sessionID)
			require.NotNil(t, base.FilePath)
			assert.Equal(t, basePath, *base.FilePath)
			assertSessionMessageCount(t, env.db, sessionID, 2)
			assertSessionMessageCount(t, env.db, parser.AltSessionID(sessionID, revertedPath), 4)
		})
	}
}

// Codex can hold the same reverted rollout in the live and archived roots.
// Both copies are one source, so they must produce one derived session that
// stays put across syncs.
func TestCodexRevertedRolloutCopiesInBothRootsAreOneSession(t *testing.T) {
	env, _, archivedDir := codexRootEnv(t)
	day := filepath.Join("2026", "07", "28")
	sessionID := "codex:" + codexThreadID

	env.writeCodexSession(t, day, codexOrdinaryName, codexRollout(codexThreadID, 3))
	env.engine.SyncAll(t.Context(), nil)
	livePath := env.writeCodexSession(t, day, codexRevertedName, codexRollout(codexThreadID, 1))
	env.writeSession(t, archivedDir, codexRevertedName, codexRollout(codexThreadID, 1))

	for range 2 {
		env.engine.SyncAll(t.Context(), nil)
		records, err := env.db.ListSessionPathRecords(t.Context(), sessionID)
		require.NoError(t, err)
		var derived []string
		for _, r := range records {
			if r.ID != sessionID && !r.Excluded {
				derived = append(derived, r.ID+" "+r.FilePath)
			}
		}
		assert.Equal(t, []string{parser.AltSessionID(sessionID, livePath) + " " + livePath}, derived)
	}
}
