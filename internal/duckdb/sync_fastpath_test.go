//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncIncrementalMetadataChangeSkipsMessageRewrite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	rowIDBefore := duckMessageRowID(t, syncer.DB(), fixture.betaID, 0)

	time.Sleep(time.Millisecond)
	renameSessionOnly(t, local, fixture.betaID, "beta renamed")
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 0, second.MessagesPushed)
	assert.Equal(t,
		rowIDBefore,
		duckMessageRowID(t, syncer.DB(), fixture.betaID, 0),
		"metadata-only session pushes must not delete and recreate messages",
	)
}

func TestSyncIncrementalVolatileStatChangeSkipsMirrorRewrite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	firstWatermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	before := duckSessionMirrorStats(t, syncer.DB(), fixture.betaID)

	time.Sleep(time.Millisecond)
	volatileAt := time.Now().UTC()
	volatileMtime := volatileAt.UnixNano()
	mutateSessionVolatileStats(t, local, fixture.betaID, volatileMtime)
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Zero(t, second.SessionsPushed)
	assert.Zero(t, second.MessagesPushed)
	afterStatOnly := duckSessionMirrorStats(t, syncer.DB(), fixture.betaID)
	assert.Equal(t, before, afterStatOnly,
		"stat-only churn should not rewrite mirrored session columns")
	watermarkAfterStatOnly, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Equal(t, second.Diagnostics.Cutoff, watermarkAfterStatOnly)
	assert.Greater(t, watermarkAfterStatOnly, firstWatermark)

	time.Sleep(time.Millisecond)
	appendLocalMessage(t, local, fixture.betaID, 2, syncMessage(
		fixture.betaID, 1, "assistant", "beta content changed",
		time.Now().UTC().Format(localSyncTimestampLayout),
	))
	mutateSessionVolatileStats(t, local, fixture.betaID, volatileMtime)
	third, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, third.SessionsPushed)
	assert.Equal(t, 1, third.MessagesPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", fixture.betaID, 2)
	afterContent := duckSessionMirrorStats(t, syncer.DB(), fixture.betaID)
	require.True(t, afterContent.fileMtime.Valid)
	assert.Equal(t, volatileMtime, afterContent.fileMtime.Int64,
		"real content pushes should still write mirrored stat columns")
}

func TestSyncIncrementalAppendsOnlyNewSuffixMessages(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	const sessionID = "duck-sync-append"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "append first",
			"2026-01-19T00:00:00.000Z", 2,
		),
		Messages: []db.Message{
			syncMessage(sessionID, 0, "user", "append first", "2026-01-19T00:00:00.000Z"),
			syncMessage(sessionID, 1, "assistant", "append reply", "2026-01-19T00:01:00.000Z"),
		},
		UsageEvents: []db.UsageEvent{{
			Source:       "hermes",
			Model:        "claude-test",
			InputTokens:  1,
			OutputTokens: 2,
			OccurredAt:   "2026-01-19T00:02:00.000Z",
			DedupKey:     "append-usage",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	prefixRowID := duckMessageRowID(t, syncer.DB(), sessionID, 0)

	time.Sleep(time.Millisecond)
	appendLocalMessage(t, local, sessionID, 2, syncMessage(
		sessionID, 2, "assistant", "append suffix",
		time.Now().UTC().Format(localSyncTimestampLayout),
		db.ToolCall{
			ToolName:  "read",
			Category:  "filesystem",
			ToolUseID: "append-tool",
			InputJSON: `{"path":"append.txt"}`,
			ResultEvents: []db.ToolResultEvent{{
				Source:        "tool",
				Status:        "complete",
				Content:       "append result",
				ContentLength: len("append result"),
				Timestamp:     time.Now().UTC().Format(localSyncTimestampLayout),
				EventIndex:    0,
			}},
		},
	), db.UsageEvent{
		Source:       "hermes",
		Model:        "claude-test",
		InputTokens:  42,
		OutputTokens: 2,
		OccurredAt:   "2026-01-19T00:02:00.000Z",
		DedupKey:     "append-usage",
	})
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 1, second.MessagesPushed)
	assert.Equal(t,
		prefixRowID,
		duckMessageRowID(t, syncer.DB(), sessionID, 0),
		"suffix-only growth must preserve already mirrored prefix rows",
	)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", sessionID, 3)
	assertDuckDBCountWhere(t, syncer.DB(), "tool_calls", "session_id = ?", sessionID, 1)
	assertDuckDBCountWhere(t, syncer.DB(), "tool_result_events", "session_id = ?", sessionID, 1)
	var inputTokens int
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT input_tokens FROM usage_events WHERE session_id = ? AND dedup_key = ?`,
		sessionID, "append-usage",
	).Scan(&inputTokens))
	assert.Equal(t, 42, inputTokens)
}

func TestSyncIncrementalHistoricalMessageChangeFallsBackToRewrite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	rowIDBefore := duckMessageRowID(t, syncer.DB(), fixture.alphaID, 0)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha historical edit", modifiedAt)
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 1, second.MessagesPushed)
	assert.NotEqual(t,
		rowIDBefore,
		duckMessageRowID(t, syncer.DB(), fixture.alphaID, 0),
		"historical content changes must fall back to a full dependent rewrite",
	)
}

func TestSyncIncrementalMessageIDChangeFallsBackToRewrite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	rowIDBefore := duckMessageRowID(t, syncer.DB(), fixture.alphaID, 0)
	localBefore, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, localBefore, 2)
	require.Equal(t,
		localBefore[0].ID,
		duckMessageID(t, syncer.DB(), fixture.alphaID, 0),
	)

	time.Sleep(time.Millisecond)
	localAfter := rewriteLocalMessagesPreservingContent(t, local, fixture.alphaID)
	require.Len(t, localAfter, 2)
	require.NotEqual(t, localBefore[0].ID, localAfter[0].ID)
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, len(localAfter), second.MessagesPushed)
	assert.NotEqual(t,
		rowIDBefore,
		duckMessageRowID(t, syncer.DB(), fixture.alphaID, 0),
		"local message id changes must refresh DuckDB message rows",
	)
	assert.Equal(t,
		localAfter[0].ID,
		duckMessageID(t, syncer.DB(), fixture.alphaID, 0),
	)
	assert.Equal(t, 1, duckPinnedMessageMirrorJoinCount(
		t, syncer.DB(), fixture.alphaID,
	), "curation refresh must point pins at mirrored messages")
}

func TestSyncIncrementalNullMirrorMessageIDFallsBackToRewrite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	localMessages, err := local.GetAllMessages(ctx, fixture.betaID)
	require.NoError(t, err)
	require.Len(t, localMessages, 1)
	_, err = syncer.DB().ExecContext(ctx,
		`UPDATE messages SET id = NULL WHERE session_id = ? AND ordinal = ?`,
		fixture.betaID, 0,
	)
	require.NoError(t, err)

	time.Sleep(time.Millisecond)
	renameSessionOnly(t, local, fixture.betaID, "beta null id repair")
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 1, second.MessagesPushed)
	assert.Equal(t,
		localMessages[0].ID,
		duckMessageID(t, syncer.DB(), fixture.betaID, 0),
		"NULL mirrored message ids must force a repair rewrite",
	)
}

func TestPushSessionSkipFastPathRefreshesPinnedMessages(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	note := "skip fast path pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)
	sess, err := local.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, sess)

	pushedMessages, err := syncer.pushSingleSession(ctx, *sess, false)
	require.NoError(t, err)

	assert.Zero(t, pushedMessages)
	assert.Equal(t, note, duckPinnedMessageNote(
		t, syncer.DB(), fixture.alphaID, msgs[0].ID,
	))
}

func TestPushSessionAppendFastPathRefreshesPinnedMessages(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	note := "append fast path pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)
	appendLocalMessage(t, local, fixture.alphaID, 3, syncMessage(
		fixture.alphaID, 2, "assistant", "append after pin",
		time.Now().UTC().Format(localSyncTimestampLayout),
	))
	sess, err := local.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, sess)

	pushedMessages, err := syncer.pushSingleSession(ctx, *sess, false)
	require.NoError(t, err)

	assert.Equal(t, 1, pushedMessages)
	assert.Equal(t, note, duckPinnedMessageNote(
		t, syncer.DB(), fixture.alphaID, msgs[0].ID,
	))
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", fixture.alphaID, 3)
}

func TestSyncIncrementalToolResultEventChangeUpdatesMirror(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	const sessionID = "duck-sync-result-event"
	writeToolResultStatusSession(t, local, sessionID, "in_progress")
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)

	time.Sleep(time.Millisecond)
	writeToolResultStatusSession(t, local, sessionID, "complete")
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	var status string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT status FROM tool_result_events WHERE session_id = ?`,
		sessionID,
	).Scan(&status))
	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, "complete", status)
}

func TestSyncIncrementalFastPathLifecycle(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	const sessionID = "duck-sync-fastpath-lifecycle"
	writeFastPathLifecycleSession(t, local, sessionID, "in_progress", false, 0)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	require.Equal(t, 2, first.MessagesPushed)
	row0 := duckMessageRowID(t, syncer.DB(), sessionID, 0)
	row1 := duckMessageRowID(t, syncer.DB(), sessionID, 1)

	noOp, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, noOp.SessionsPushed)
	assert.Equal(t, row0, duckMessageRowID(t, syncer.DB(), sessionID, 0))
	assert.Equal(t, row1, duckMessageRowID(t, syncer.DB(), sessionID, 1))

	time.Sleep(time.Millisecond)
	renameSessionOnly(t, local, sessionID, "renamed lifecycle")
	metadataOnly, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, metadataOnly.SessionsPushed)
	assert.Zero(t, metadataOnly.MessagesPushed)
	assert.Equal(t, row0, duckMessageRowID(t, syncer.DB(), sessionID, 0))
	assert.Equal(t, row1, duckMessageRowID(t, syncer.DB(), sessionID, 1))

	time.Sleep(time.Millisecond)
	appendLocalMessage(t, local, sessionID, 3, lifecycleSuffixMessage(sessionID), db.UsageEvent{
		Source:       "hermes",
		Model:        "claude-test",
		InputTokens:  42,
		OutputTokens: 7,
		OccurredAt:   "2026-01-21T00:03:00.000Z",
		DedupKey:     "lifecycle-usage",
	})
	appended, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, appended.SessionsPushed)
	assert.Equal(t, 1, appended.MessagesPushed)
	assert.Equal(t, row0, duckMessageRowID(t, syncer.DB(), sessionID, 0))
	assert.Equal(t, row1, duckMessageRowID(t, syncer.DB(), sessionID, 1))
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", sessionID, 3)
	var inputTokens int
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT input_tokens FROM usage_events WHERE session_id = ? AND dedup_key = ?`,
		sessionID, "lifecycle-usage",
	).Scan(&inputTokens))
	assert.Equal(t, 42, inputTokens)

	time.Sleep(time.Millisecond)
	writeFastPathLifecycleSession(t, local, sessionID, "complete", true, 99)
	resultOnly, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, resultOnly.SessionsPushed)
	assert.Equal(t, 3, resultOnly.MessagesPushed)
	assert.NotEqual(t, row1, duckMessageRowID(t, syncer.DB(), sessionID, 1))
	var status string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT status FROM tool_result_events WHERE session_id = ?`,
		sessionID,
	).Scan(&status))
	assert.Equal(t, "complete", status)
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT input_tokens FROM usage_events WHERE session_id = ? AND dedup_key = ?`,
		sessionID, "lifecycle-usage",
	).Scan(&inputTokens))
	assert.Equal(t, 99, inputTokens)
}

func TestSyncCheckpointPolicyRunsOnlyAfterMutatingPush(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	policy := &checkpointSpy{}
	syncer.maintenance = policy

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	assert.Equal(t, 1, policy.calls)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, second.SessionsPushed)
	assert.Equal(t, 1, policy.calls, "no-op push must not checkpoint")
}

func TestSyncCheckpointFailureDoesNotAdvanceWatermark(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	policy := &checkpointSpy{err: errors.New("checkpoint failed")}
	syncer.maintenance = policy

	_, err := syncer.Push(ctx, true, nil)
	require.ErrorContains(t, err, "checkpoint failed")
	assert.Equal(t, 1, policy.calls)
	assertDuckDBCount(t, syncer.DB(), "sessions", 2)

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Empty(t, watermark)
}

func TestSyncCheckpointFailureAfterHardDeleteDoesNotKeepStaleFingerprint(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	firstWatermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	fingerprints, err := readSyncFingerprintsWithKey(local, lastPushBoundaryStateKey)
	require.NoError(t, err)
	require.Contains(t, fingerprints, fixture.betaID)

	require.NoError(t, local.SoftDeleteSession(fixture.betaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.betaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	policy := &checkpointSpy{err: errors.New("checkpoint failed")}
	syncer.maintenance = policy

	_, err = syncer.Push(ctx, false, nil)
	require.ErrorContains(t, err, "checkpoint failed")
	assert.Equal(t, 1, policy.calls)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 0)
	watermarkAfterFailure, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Equal(t, firstWatermark, watermarkAfterFailure)

	policy.err = nil
	retry, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, retry.SessionsPushed)
	assert.Equal(t, 1, policy.calls, "retry should only repair local sync state")
	fingerprints, err = readSyncFingerprintsWithKey(local, lastPushBoundaryStateKey)
	require.NoError(t, err)
	assert.NotContains(t, fingerprints, fixture.betaID)
}

func TestSyncMissingMirrorRowWithUnchangedLocalSessionIsRepaired(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	policy := &checkpointSpy{}
	syncer.maintenance = policy

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	assert.Equal(t, 1, policy.calls)
	require.NoError(t, syncer.withDuckTx(ctx, "test delete mirror session", func(tx *sql.Tx) error {
		return syncer.deleteMirrorSession(ctx, tx, fixture.betaID)
	}))
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 0)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 1, second.MessagesPushed)
	assert.Equal(t, 2, policy.calls)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 1)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", fixture.betaID, 1)
	fingerprints, err := readSyncFingerprintsWithKey(local, lastPushBoundaryStateKey)
	require.NoError(t, err)
	assert.Contains(t, fingerprints, fixture.betaID)
}

func TestDuckCheckpointDecisionRequiresFreeBlockThreshold(t *testing.T) {
	assert.False(t, shouldCheckpointDuckDB(duckDBSize{
		blockSize:  duckCheckpointMinFreeBytes,
		freeBlocks: 0,
	}))
	assert.False(t, shouldCheckpointDuckDB(duckDBSize{
		blockSize:  duckCheckpointMinFreeBytes - 1,
		freeBlocks: 1,
	}))
	assert.True(t, shouldCheckpointDuckDB(duckDBSize{
		blockSize:  duckCheckpointMinFreeBytes,
		freeBlocks: 1,
	}))
}

func renameSessionOnly(t *testing.T, local *db.DB, sessionID, displayName string) {
	t.Helper()
	require.NoError(t, local.RenameSession(sessionID, &displayName))
}

func mutateSessionVolatileStats(
	t *testing.T, local *db.DB, sessionID string, fileMtime int64,
) {
	t.Helper()
	ctx := context.Background()
	sess, err := local.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	sess.FileMtime = &fileMtime
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: *sess,
	}})
	require.NoError(t, err)
	require.NoError(t, local.BumpLocalModifiedAt(sessionID))
}

func appendLocalMessage(
	t *testing.T, local *db.DB, sessionID string, newCount int, msg db.Message,
	usageEvents ...db.UsageEvent,
) {
	t.Helper()
	ctx := context.Background()
	sess, err := local.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	sess.LocalModifiedAt = &modifiedAt
	sess.MessageCount = newCount
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:     *sess,
		Messages:    []db.Message{msg},
		UsageEvents: usageEvents,
	}})
	require.NoError(t, err)
}

func rewriteLocalMessagesPreservingContent(
	t *testing.T, local *db.DB, sessionID string,
) []db.Message {
	t.Helper()
	ctx := context.Background()
	sess, err := local.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := local.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	sess.MessageCount = len(msgs)
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         *sess,
		Messages:        msgs,
		DataVersion:     1, // stamps local_modified_at for Push(false) selection
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	rewritten, err := local.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	return rewritten
}

func writeToolResultStatusSession(
	t *testing.T, local *db.DB, sessionID, status string,
) {
	t.Helper()
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	msg := syncMessage(
		sessionID, 0, "assistant", "tool result event",
		"2026-01-20T00:00:00.000Z",
		db.ToolCall{
			ToolName:  "read",
			Category:  "filesystem",
			ToolUseID: "result-event-tool",
			InputJSON: `{"path":"event.txt"}`,
			ResultEvents: []db.ToolResultEvent{{
				Source:        "tool",
				Status:        status,
				Content:       "event result",
				ContentLength: len("event result"),
				Timestamp:     "2026-01-20T00:00:30.000Z",
				EventIndex:    0,
			}},
		},
	)
	sess := syncSession(
		sessionID, "alpha", "tool result event",
		"2026-01-20T00:00:00.000Z", 1,
	)
	sess.LocalModifiedAt = &modifiedAt
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{msg},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

func writeFastPathLifecycleSession(
	t *testing.T,
	local *db.DB,
	sessionID string,
	resultStatus string,
	includeSuffix bool,
	usageInputTokens int,
) {
	t.Helper()
	messages := []db.Message{
		syncMessage(
			sessionID, 0, "user", "lifecycle first",
			"2026-01-21T00:00:00.000Z",
		),
		lifecycleToolMessage(sessionID, resultStatus),
	}
	if includeSuffix {
		messages = append(messages, lifecycleSuffixMessage(sessionID))
	}
	sess := syncSession(
		sessionID, "alpha", "lifecycle first",
		"2026-01-21T00:00:00.000Z", len(messages),
	)
	write := db.SessionBatchWrite{
		Session:         sess,
		Messages:        messages,
		DataVersion:     1, // stamps local_modified_at for Push(false) selection
		ReplaceMessages: true,
	}
	if usageInputTokens > 0 {
		write.UsageEvents = []db.UsageEvent{{
			Source:       "hermes",
			Model:        "claude-test",
			InputTokens:  usageInputTokens,
			OutputTokens: 7,
			OccurredAt:   "2026-01-21T00:03:00.000Z",
			DedupKey:     "lifecycle-usage",
		}}
	}
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{write})
	require.NoError(t, err)
}

func lifecycleToolMessage(sessionID, resultStatus string) db.Message {
	return syncMessage(
		sessionID, 1, "assistant", "lifecycle tool",
		"2026-01-21T00:01:00.000Z",
		db.ToolCall{
			ToolName:  "read",
			Category:  "filesystem",
			ToolUseID: "lifecycle-tool",
			InputJSON: `{"path":"lifecycle.txt"}`,
			ResultEvents: []db.ToolResultEvent{{
				Source:        "tool",
				Status:        resultStatus,
				Content:       "lifecycle result",
				ContentLength: len("lifecycle result"),
				Timestamp:     "2026-01-21T00:01:30.000Z",
				EventIndex:    0,
			}},
		},
	)
}

func lifecycleSuffixMessage(sessionID string) db.Message {
	return syncMessage(
		sessionID, 2, "assistant", "lifecycle suffix",
		"2026-01-21T00:02:00.000Z",
	)
}

func duckMessageRowID(
	t *testing.T, conn *sql.DB, sessionID string, ordinal int,
) int64 {
	t.Helper()
	var rowID int64
	require.NoError(t, conn.QueryRow(
		`SELECT rowid FROM messages WHERE session_id = ? AND ordinal = ?`,
		sessionID, ordinal,
	).Scan(&rowID))
	return rowID
}

func duckMessageID(
	t *testing.T, conn *sql.DB, sessionID string, ordinal int,
) int64 {
	t.Helper()
	var id int64
	require.NoError(t, conn.QueryRow(
		`SELECT id FROM messages WHERE session_id = ? AND ordinal = ?`,
		sessionID, ordinal,
	).Scan(&id))
	return id
}

func duckPinnedMessageMirrorJoinCount(
	t *testing.T, conn *sql.DB, sessionID string,
) int {
	t.Helper()
	var count int
	require.NoError(t, conn.QueryRow(`
		SELECT COUNT(*)
		FROM pinned_messages p
		JOIN messages m
			ON m.session_id = p.session_id
			AND m.id = p.message_id
		WHERE p.session_id = ?`,
		sessionID,
	).Scan(&count))
	return count
}

func duckPinnedMessageNote(
	t *testing.T, conn *sql.DB, sessionID string, messageID int64,
) string {
	t.Helper()
	var note string
	require.NoError(t, conn.QueryRow(
		`SELECT note FROM pinned_messages WHERE session_id = ? AND message_id = ?`,
		sessionID, messageID,
	).Scan(&note))
	return note
}

type duckMirrorStats struct {
	fileMtime     sql.NullInt64
	localModified string
}

func duckSessionMirrorStats(t *testing.T, conn *sql.DB, sessionID string) duckMirrorStats {
	t.Helper()
	var stats duckMirrorStats
	require.NoError(t, conn.QueryRow(
		`SELECT file_mtime, COALESCE(CAST(local_modified_at AS VARCHAR), '')
		 FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&stats.fileMtime, &stats.localModified))
	return stats
}

type checkpointSpy struct {
	calls int
	err   error
}

func (s *checkpointSpy) checkpointAfterPush(context.Context, *sql.DB) error {
	s.calls++
	return s.err
}
