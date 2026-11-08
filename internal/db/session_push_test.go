package db

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func sessionPushFixture(id string) (SessionPushReceipt, SessionBatchWrite) {
	return SessionPushReceipt{
		SessionID: id, SourceOrigin: "archive-a", PayloadHash: strings.Repeat("a", 64),
	}, SessionBatchWrite{
		Session:     Session{ID: id, Project: "project", Machine: "source-machine", Agent: "claude", MessageCount: 1, UserMessageCount: 1},
		Messages:    []Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "first snapshot", ContentLength: 14}},
		UsageEvents: []UsageEvent{{SessionID: id, Source: "session", Model: "model", InputTokens: 10, OutputTokens: 3}},
		DataVersion: CurrentDataVersion(), ReplaceMessages: true,
	}
}

func TestSessionPushRetryRequiresCurrentOwnership(t *testing.T) {
	d := testDB(t)
	receipt, write := sessionPushFixture("snapshot")
	_, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	// Ordinary upserts are fenced separately; simulate unexpected ownership
	// damage to exercise the retry's defensive current-row check.
	_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET machine='different-owner' WHERE id='snapshot'`)
	require.NoError(t, err)
	_, err = d.ApplySessionPush(t.Context(), receipt, write)
	require.ErrorIs(t, err, ErrSessionPushConflict)
}

func requireSessionPushReceipt(t *testing.T, d *DB, id string) {
	t.Helper()
	var origin, hash string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		`SELECT source_origin, payload_hash FROM session_push_receipts WHERE session_id = ?`, id,
	).Scan(&origin, &hash))
	assert.Equal(t, "archive-a", origin)
	assert.Equal(t, strings.Repeat("a", 64), hash)
}

func TestSessionPushCreatesAndRetryPreservesCuration(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	receipt, write := sessionPushFixture("snapshot")
	require.NoError(t, d.SetSyncState(ctx, "artifact_origin_id", "receiver-origin"))
	require.NoError(t, d.SetSyncState(ctx, "artifact_local_machine_name", receipt.SourceOrigin))
	created, err := d.ApplySessionPush(ctx, receipt, write)
	require.NoError(t, err)
	require.True(t, created)
	requireSessionPushReceipt(t, d, "snapshot")
	messages, err := d.GetAllMessages(ctx, "snapshot")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "first snapshot", messages[0].Content)
	require.NoError(t, d.RenameSession(ctx, "snapshot", Ptr("local title")))
	_, err = d.StarSession(ctx, "snapshot")
	require.NoError(t, err)
	_, err = d.PinMessage(ctx, "snapshot", messages[0].ID, Ptr("local note"))
	require.NoError(t, err)
	before, err := d.GetSessionFull(ctx, "snapshot")
	require.NoError(t, err)
	var generation int64
	require.NoError(t, d.getReader().QueryRowContext(ctx,
		`SELECT generation FROM artifact_export_queue WHERE session_id = 'snapshot'`).Scan(&generation))
	created, err = d.ApplySessionPush(ctx, receipt, write)
	require.NoError(t, err)
	assert.False(t, created)
	after, err := d.GetSessionFull(ctx, "snapshot")
	require.NoError(t, err)
	assert.Equal(t, before, after, "retry must not touch any session metadata")
	afterMessages, err := d.GetAllMessages(ctx, "snapshot")
	require.NoError(t, err)
	assert.Equal(t, messages, afterMessages, "retry must preserve message IDs")
	pins, err := d.ListPinnedMessages(ctx, "snapshot", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, Ptr("local note"), pins[0].Note)
	stars, err := d.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Contains(t, stars, "snapshot")
	var afterGeneration int64
	require.NoError(t, d.getReader().QueryRowContext(ctx,
		`SELECT generation FROM artifact_export_queue WHERE session_id = 'snapshot'`).Scan(&afterGeneration))
	assert.Equal(t, generation, afterGeneration, "retry must not enqueue publication")
}

func TestSessionPushConflictsDoNotOverwrite(t *testing.T) {
	for _, kind := range []string{"hash", "origin", "unowned", "trash", "deleted", "excluded"} {
		t.Run(kind, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			receipt, write := sessionPushFixture("snapshot")
			if kind == "unowned" || kind == "excluded" {
				_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
				require.NoError(t, err)
			} else {
				created, err := d.ApplySessionPush(ctx, receipt, write)
				require.NoError(t, err)
				require.True(t, created)
			}
			switch kind {
			case "hash":
				receipt.PayloadHash = strings.Repeat("b", 64)
			case "origin":
				receipt.SourceOrigin = "archive-b"
			case "trash":
				require.NoError(t, d.SoftDeleteSession(ctx, "snapshot"))
			case "deleted", "excluded":
				require.NoError(t, d.DeleteSession(ctx, "snapshot"))
			}
			before, err := d.GetArtifactExportSession(ctx, "snapshot")
			require.NoError(t, err)
			write.Messages[0].Content = "replacement must not land"
			created, err := d.ApplySessionPush(ctx, receipt, write)
			require.ErrorIs(t, err, ErrSessionPushConflict)
			assert.False(t, created)
			after, err := d.GetArtifactExportSession(ctx, "snapshot")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			messages, err := d.GetAllMessages(ctx, "snapshot")
			require.NoError(t, err)
			if kind == "deleted" || kind == "excluded" {
				assert.Empty(t, messages)
			} else {
				require.Len(t, messages, 1)
				assert.Equal(t, "first snapshot", messages[0].Content)
			}
		})
	}
}

func TestSessionPushMalformedWriteRollsBack(t *testing.T) {
	d := testDB(t)
	receipt, write := sessionPushFixture("snapshot")
	// Usage validation runs after the session insert inside the batch writer.
	write.UsageEvents[0].SessionID = "other-session"
	created, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.ErrorContains(t, err, "usage event session_id")
	assert.False(t, created)
	for _, table := range []string{"sessions", "messages", "usage_events", "session_push_receipts", "artifact_export_queue"} {
		var count int
		require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table)
	}
	write.UsageEvents[0].SessionID = "snapshot"
	created, err = d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	assert.True(t, created, "rollback must not reserve the receipt")
}

func TestSessionPushRejectsMismatchedReceiptIdentity(t *testing.T) {
	d := testDB(t)
	receipt, write := sessionPushFixture("snapshot")
	receipt.SessionID = "other-session"
	created, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.Error(t, err)
	assert.False(t, created)
	for _, id := range []string{"snapshot", "other-session"} {
		session, err := d.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.Nil(t, session)
	}
}

func TestSessionPushRebuildPreservesReceiptsAndCuration(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	for _, id := range []string{"live", "trashed", "purged"} {
		receipt, write := sessionPushFixture(id)
		created, err := source.ApplySessionPush(ctx, receipt, write)
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, source.RenameSession(ctx, id, Ptr("local title")))
		_, err = source.StarSession(ctx, id)
		require.NoError(t, err)
		messages, err := source.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		_, err = source.PinMessage(ctx, id, messages[0].ID, Ptr("saved note"))
		require.NoError(t, err)
	}
	require.NoError(t, source.SoftDeleteSession(ctx, "trashed"))
	require.NoError(t, source.DeleteSession(ctx, "purged"))
	path := source.Path()
	require.NoError(t, source.Close())
	destination := testDB(t)
	require.NoError(t, destination.CopySyncStateFrom(ctx, path))
	// Receipts precede session restoration and include purged IDs.
	for _, id := range []string{"live", "trashed", "purged"} {
		requireSessionPushReceipt(t, destination, id)
	}
	require.NoError(t, destination.CopyExcludedSessionsFrom(ctx, path))
	_, err := destination.CopyTrashedDataFrom(ctx, path)
	require.NoError(t, err)
	_, err = destination.CopyOrphanedDataFrom(ctx, path)
	require.NoError(t, err)
	require.NoError(t, destination.CopySessionMetadataFrom(ctx, path))
	for _, id := range []string{"live", "trashed"} {
		session, err := destination.GetArtifactExportSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, Ptr("local title"), session.DisplayName)
		pins, err := destination.ListPinnedMessages(ctx, id, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Equal(t, Ptr("saved note"), pins[0].Note)
	}
	stars, err := destination.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Contains(t, stars, "live")
	for _, id := range []string{"live", "trashed", "purged"} {
		receipt, write := sessionPushFixture(id)
		created, err := destination.ApplySessionPush(ctx, receipt, write)
		assert.False(t, created)
		if id == "live" {
			assert.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrSessionPushConflict)
		}
	}
}

func TestSessionPushStoragePolicies(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			d.SetToolResultImages(config.ToolResultImagesDrop)
			receipt, write := sessionPushFixture("snapshot")
			write.Messages = []Message{testImageMessage("snapshot")}
			write.Messages[0].Content = "answer\x00 retained"
			created, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			assert.True(t, created)
			messages, err := d.GetAllMessages(t.Context(), "snapshot")
			require.NoError(t, err)
			if policy == config.ArchiveContentUsage {
				require.Len(t, messages, 1)
				assert.Equal(t, "assistant", messages[0].Role)
				assert.Empty(t, messages[0].Content)
				assert.Empty(t, messages[0].ToolCalls)
				return
			}
			require.Len(t, messages, 1)
			assert.Equal(t, "answer retained", messages[0].Content)
			require.Len(t, messages[0].ToolCalls, 1)
			result := messages[0].ToolCalls[0].ResultContent
			if policy == config.ArchiveContentTranscripts {
				assert.Empty(t, result)
			} else {
				assert.Contains(t, result, `"type":"agentsview_image"`)
				assert.NotContains(t, result, "data:image/")
			}
		})
	}
}

func TestSessionPushUsesReceiptOrigin(t *testing.T) {
	d := testDB(t)
	receipt, write := sessionPushFixture("snapshot")
	write.Session.Machine = "local"
	created, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	assert.True(t, created)
	session, err := d.GetSessionFull(t.Context(), "snapshot")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "archive-a", session.Machine)
}

func TestSessionPushCanceledDoesNotPublish(t *testing.T) {
	d := testDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, write := sessionPushFixture("snapshot")
	created, err := d.ApplySessionPush(ctx, receipt, write)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, created)
	var count int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_push_receipts`).Scan(&count))
	assert.Zero(t, count)
}

func TestLoadSessionPushDataExactLiveSession(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"parent", "child", "trashed"} {
		_, write := sessionPushFixture(id)
		write.Session.FirstMessage = Ptr("first snapshot")
		write.Session.SessionName = Ptr("agent title")
		if id == "child" {
			write.Session.ParentSessionID = Ptr("parent")
		}
		_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
		require.NoError(t, err)
	}
	require.NoError(t, d.SoftDeleteSession(ctx, "trashed"))
	for _, id := range []string{"missing", "pare", "trashed"} {
		session, data, err := d.LoadSessionPushData(ctx, id, smallArtifactExportLoadLimits())
		require.NoError(t, err)
		assert.Nil(t, session)
		assert.Empty(t, data.Messages)
	}
	session, data, err := d.LoadSessionPushData(ctx, "parent", smallArtifactExportLoadLimits())
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "parent", session.ID)
	assert.Nil(t, session.DisplayName, "keep agent and local names separate for export")
	assert.Equal(t, Ptr("agent title"), session.SessionName)
	require.Len(t, data.Messages, 1)
	assert.Equal(t, "parent", data.Messages[0].SessionID)
	require.Len(t, data.UsageEvents, 1)
	assert.Equal(t, "parent", data.UsageEvents[0].SessionID)
	limits := smallArtifactExportLoadLimits()
	limits.MessageBytes = 1
	_, _, err = d.LoadSessionPushData(ctx, "parent", limits)
	require.ErrorIs(t, err, ErrArtifactExportLimit)
}

func TestLoadSessionPushDataCoherentSnapshotDuringWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, write := sessionPushFixture("snapshot")
	write.Session.FirstMessage = Ptr("first snapshot")
	_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
	require.NoError(t, err)
	start := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		<-start
		for i := range 24 {
			_, next := sessionPushFixture("snapshot")
			if i%2 == 0 {
				next.Messages[0].Content = "second snapshot"
			}
			next.Session.FirstMessage = Ptr(next.Messages[0].Content)
			if _, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{next}); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()
	close(start)
	for range 24 {
		session, data, err := d.LoadSessionPushData(ctx, "snapshot", smallArtifactExportLoadLimits())
		if !assert.NoError(t, err, "loading session push snapshot") {
			break
		}
		if !assert.NotNil(t, session, "loaded session push snapshot") {
			break
		}
		if !assert.Len(t, data.Messages, 1) {
			break
		}
		assert.Equal(t, Ptr(data.Messages[0].Content), session.FirstMessage)
	}
	require.NoError(t, <-writerDone)
}
