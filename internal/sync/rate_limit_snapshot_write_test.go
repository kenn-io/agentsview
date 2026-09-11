package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// codexRLTranscript builds a single Codex rollout with one rate-limit
// observation, shared by both tests in this file.
func codexRLTranscript(uuid, dir string, usedPercent float64) string {
	return testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, dir, "codex_cli_rs", "2026-06-11T12:44:06Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2026-06-11T12:44:07Z"),
		testjsonl.CodexMsgJSON("assistant", "hi", "2026-06-11T12:44:08Z"),
		testjsonl.CodexTokenCountWithRateLimitsJSON(
			"2026-06-11T12:44:09Z", 10000, 500, 6000, "codex", "pro",
			&testjsonl.CodexRateLimitWindow{UsedPercent: usedPercent, WindowMinutes: 10080, ResetsAt: 1789435448},
			nil, "100.0",
		),
	)
}

// TestCodexEngineRateLimitSnapshotIDPrefix pins the prefixed-session-id
// invariant: the parser stamps each snapshot with its own raw session id,
// but a remote sync's applyRemoteRewrites renames the session itself, so
// rateLimitSnapshotsForWrite must attach the snapshot to the final,
// prefixed id. Covers both the collecting and staged parse paths.
func TestCodexEngineRateLimitSnapshotIDPrefix(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b20"
	transcript := codexRLTranscript(uuid, "/workspace/project-a", 42)
	const wantSessionID = "remote-host~codex:" + uuid

	for _, stagedMin := range []int64{0, 1} {
		database := openTestDB(t)
		engine := NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCodex: {writeCodexTranscriptRoot(t, uuid, transcript)},
			},
			Machine: "remote", IDPrefix: "remote-host~", StagedCodexParseMinBytes: stagedMin,
		})
		t.Cleanup(engine.Close)
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, 1, stats.Synced)

		rows, err := database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{Machine: "remote"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, wantSessionID, rows[0].SessionID,
			"snapshot must carry the final prefixed session id, not the parser's native one")
	}
}

// writeCodexRolloutInto writes a single Codex rollout file for uuid under
// root, so a test can later remove it to simulate an orphaned session.
func writeCodexRolloutInto(t *testing.T, root, uuid, transcript string) string {
	t.Helper()
	day := filepath.Join(root, "2026", "06", "11")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2026-06-11T12-44-06-"+uuid+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(transcript), 0o644))
	return path
}

// TestResyncAllPreservesOrphanedSessionRateLimitSnapshots pins ResyncAll's
// copy ordering: CopyRateLimitSnapshotsFrom must run after
// CopyOrphanedDataFromExcluding, or an orphaned session's snapshot would
// trip the table's session_id foreign key and abort the whole copy.
func TestResyncAllPreservesOrphanedSessionRateLimitSnapshots(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b21"
	sessionID := "codex:" + uuid
	root := t.TempDir()
	rolloutPath := writeCodexRolloutInto(t, root, uuid, codexRLTranscript(uuid, "/repo", 55))

	// A second, still-present session, so removing the first below does
	// not trip ResyncAll's empty-discovery guard instead.
	const keptUUID = "019eb791-cf7d-75c1-8439-9ed74c122b22"
	writeCodexRolloutInto(t, root, keptUUID, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(keptUUID, "/repo", "codex_cli_rs", "2026-06-11T13:00:00Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2026-06-11T13:00:01Z"),
		testjsonl.CodexMsgJSON("assistant", "hi", "2026-06-11T13:00:02Z"),
	))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	require.NoError(t, os.Remove(rolloutPath), "remove orphan source")
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)
	sess, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess, "the orphaned session itself must survive the resync")

	rows, err := database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the orphaned session's rate-limit snapshot must survive the resync")
	assert.Equal(t, sessionID, rows[0].SessionID)
}

// TestResyncAllPreservesTrashedSessionRateLimitSnapshots: CopyRateLimitSnapshotsFrom must get copiedSessionIDs (trashed + orphaned), not orphaned alone.
func TestResyncAllPreservesTrashedSessionRateLimitSnapshots(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b25"
	sessionID := "codex:" + uuid
	root := t.TempDir()
	writeCodexRolloutInto(t, root, uuid, codexRLTranscript(uuid, "/repo", 65))
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, database.SoftDeleteSession(sessionID), "trash the session")
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)
	rows, err := database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the trashed session's rate-limit snapshot must survive the resync")
}

// TestCodexEngineRateLimitSnapshotSameTimestampOrdinal pins the parser's
// per-event ordinal (see parser.ParsedRateLimitSnapshot.Ordinal): two
// token_count events landing on the same observed_at second must both
// persist instead of the second colliding with, and being dropped
// against, the first's dedup_key. Covers both the collecting and staged
// parse paths, which share the same builder and so must assign the same
// ordinals.
func TestCodexEngineRateLimitSnapshotSameTimestampOrdinal(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b23"
	const at = "2026-06-11T12:44:09Z"
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project-a", "codex_cli_rs", "2026-06-11T12:44:06Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2026-06-11T12:44:07Z"),
		testjsonl.CodexMsgJSON("assistant", "hi", "2026-06-11T12:44:08Z"),
		testjsonl.CodexTokenCountWithRateLimitsJSON(
			at, 10000, 500, 6000, "codex", "pro",
			&testjsonl.CodexRateLimitWindow{UsedPercent: 40, WindowMinutes: 10080, ResetsAt: 1789435448},
			nil, "100.0",
		),
		testjsonl.CodexTokenCountWithRateLimitsJSON(
			at, 10200, 520, 6100, "codex", "pro",
			&testjsonl.CodexRateLimitWindow{UsedPercent: 60, WindowMinutes: 10080, ResetsAt: 1789435448},
			nil, "100.0",
		),
	)

	for _, stagedMin := range []int64{0, 1} {
		database := openTestDB(t)
		engine := NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCodex: {writeCodexTranscriptRoot(t, uuid, transcript)},
			},
			Machine: "local", StagedCodexParseMinBytes: stagedMin,
		})
		t.Cleanup(engine.Close)
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, 1, stats.Synced)

		rows, err := database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{Machine: "local"})
		require.NoError(t, err)
		require.Len(t, rows, 2, "two token_count events sharing a timestamp must both persist")
	}
}

// TestResyncAllRateLimitSnapshotReparseAddsZeroRows pins ordinal
// reparse-stability across an incremental append: the second
// token_count event's ordinal, assigned during an incremental tail
// parse from a cached (or reseeded) cursor, must match what a later
// full reparse of the whole file assigns it. ResyncAll both reparses
// every still-present rollout from scratch and copies the old archive's
// rows via CopyRateLimitSnapshotsFrom, so a drifted ordinal would
// duplicate the second event's row under a new dedup_key instead of
// converging on the row already there.
func TestResyncAllRateLimitSnapshotReparseAddsZeroRows(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b25"
	sessionID := "codex:" + uuid
	root := t.TempDir()
	path := writeCodexRolloutInto(t, root, uuid, codexRLTranscript(uuid, "/repo", 42))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	rows, err := database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Append a second turn (its own token_count event) and sync it
	// incrementally (not via a fresh full parse), so the new event's
	// ordinal comes from the builder's cached/reseeded cursor rather
	// than a from-scratch count. The appended assistant message gives
	// the token_count event's usage somewhere to attach, so the
	// incremental parser does not itself fall back to a full reparse.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("user", "again", "2026-06-11T12:44:10Z"),
		testjsonl.CodexMsgJSON("assistant", "sure", "2026-06-11T12:44:11Z"),
		testjsonl.CodexTokenCountWithRateLimitsJSON(
			"2026-06-11T12:44:12Z", 10200, 520, 6100, "codex", "pro",
			&testjsonl.CodexRateLimitWindow{UsedPercent: 44, WindowMinutes: 10080, ResetsAt: 1789435448},
			nil, "100.0",
		),
	) + "\n")
	require.NoError(t, f.Close())
	require.NoError(t, err)
	engine.SyncPaths([]string{path})

	sess, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.LastWriteIncremental, "the append must take the incremental parse path, not a full reparse")

	rows, err = database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 2, "the incrementally-parsed second event must persist its own row")

	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %+v", stats)

	rows, err = database.RateLimitSnapshotHistory(t.Context(), db.RateLimitHistoryFilter{})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "reparsing the file from scratch must not duplicate the incrementally-parsed row")
}
