package db

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func sessionPushFixture(id string) (SessionPushReceipt, SessionBatchWrite) {
	return SessionPushReceipt{
		SessionID: id, SourceOrigin: "push-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadHash: strings.Repeat("a", 64),
	}, SessionBatchWrite{
		Session:     Session{ID: id, Project: "project", Machine: "source-machine", Agent: "claude", MessageCount: 1, UserMessageCount: 1},
		Messages:    []Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "first snapshot", ContentLength: 14}},
		UsageEvents: []UsageEvent{{SessionID: id, Source: "session", Model: "model", InputTokens: 10, OutputTokens: 3}},
		DataVersion: CurrentDataVersion(), ReplaceMessages: true,
	}
}

func TestSessionPushRejectsArchiveAwaitingRebuild(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().ExecContext(t.Context(), "PRAGMA user_version = 175")
	require.NoError(t, err)
	receipt, write := sessionPushFixture("snapshot")
	_, err = d.ApplySessionPush(t.Context(), receipt, write)
	require.ErrorIs(t, err, ErrSessionPushNeedsUpgrade)
	stored, err := d.GetSession(t.Context(), "snapshot")
	require.NoError(t, err)
	assert.Nil(t, stored)
	_, err = d.getWriter().ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", SessionPushDataVersion))
	require.NoError(t, err)
	created, err := d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	assert.True(t, created)
}

func requireSessionPushReceipt(t *testing.T, d *DB, id string) {
	t.Helper()
	var origin, hash string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		`SELECT source_origin, payload_hash FROM session_push_receipts WHERE session_id = ?`, id,
	).Scan(&origin, &hash))
	assert.Equal(t, "push-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", origin)
	assert.Equal(t, strings.Repeat("a", 64), hash)
}

func TestSessionPushCreatesAndRetryPreservesCuration(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	receipt, write := sessionPushFixture("snapshot")
	started := "2026-08-10T10:00:00Z"
	write.Session.StartedAt = &started
	write.Messages = []Message{testImageMessage("snapshot")}
	write.Messages[0].Content = "first snapshot"
	write.Session.UserMessageCount = 0
	write.Messages[0].Timestamp = started
	write.Messages[0].Model = "model"
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	usage, err := d.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Zero(t, usage.Totals.InputTokens)
	created, err := d.ApplySessionPush(ctx, receipt, write)
	require.NoError(t, err)
	require.True(t, created)
	usage, err = d.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 10, usage.Totals.InputTokens)
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
	require.NotNil(t, before)
	assert.Equal(t, "push-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", before.Machine)
	dir := t.TempDir()
	d.SetAssetsDir(dir)
	d.SetToolResultImages(config.ToolResultImagesOffload)
	created, err = d.ApplySessionPush(ctx, receipt, write)
	require.NoError(t, err)
	assert.False(t, created)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
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
}

func TestSessionPushConflictsDoNotOverwrite(t *testing.T) {
	for _, kind := range []string{"hash", "origin", "unowned", "trash", "excluded", "inherited-trash", "machine-adopted"} {
		t.Run(kind, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			id := "snapshot"
			if kind == "inherited-trash" {
				d = upgradedCodexTrash(t)
				id = legacyTrashMissingPage
			}
			receipt, write := sessionPushFixture(id)
			write.Messages = []Message{testImageMessage(id)}
			write.Messages[0].Content = "first snapshot"
			write.Session.UserMessageCount = 0
			switch kind {
			case "inherited-trash":
				write.Session.Agent = "codex"
			case "unowned", "excluded":
				_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
				require.NoError(t, err)
			default:
				created, err := d.ApplySessionPush(ctx, receipt, write)
				require.NoError(t, err)
				require.True(t, created)
			}
			switch kind {
			case "machine-adopted":
				_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET machine='different-owner' WHERE id=?`, id)
				require.NoError(t, err)
			case "hash":
				receipt.PayloadHash = strings.Repeat("b", 64)
				write.Messages[0].Content = "changed snapshot"
			case "origin":
				receipt.SourceOrigin = "archive-b"
			case "trash":
				require.NoError(t, d.SoftDeleteSession(ctx, id))
			case "excluded":
				require.NoError(t, d.DeleteSession(ctx, id))
			}
			before, err := d.GetArtifactExportSession(ctx, id)
			require.NoError(t, err)
			dir := t.TempDir()
			d.SetAssetsDir(dir)
			d.SetToolResultImages(config.ToolResultImagesOffload)
			created, err := d.ApplySessionPush(ctx, receipt, write)
			require.ErrorIs(t, err, ErrSessionPushConflict)
			assert.False(t, created)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Empty(t, entries, "a rejected or unchanged push must not publish image objects")
			after, err := d.GetArtifactExportSession(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			messages, err := d.GetAllMessages(ctx, id)
			require.NoError(t, err)
			if kind == "excluded" || kind == "inherited-trash" {
				assert.Empty(t, messages)
			} else {
				require.Len(t, messages, 1)
				assert.Equal(t, "first snapshot", messages[0].Content)
			}
			if kind == "inherited-trash" || kind == "excluded" {
				var receipts int
				require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count(*) FROM session_push_receipts WHERE session_id=?`, id).Scan(&receipts))
				assert.Zero(t, receipts)
			}
		})
	}
}

func TestSessionPushWriteFailuresRollBack(t *testing.T) {
	for _, tc := range []struct {
		name, table, message string
	}{
		{"usage", "usage_events", "usage write failed"},
		{"parent", "session_external_parents", "parent write failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			receipt, write := sessionPushFixture("snapshot")
			if tc.name == "parent" {
				receipt.ExternalParentSessionID = new("parent")
			}
			require.NoError(t, d.SetSyncState(t.Context(), "artifact_origin_id", "receiver-origin"))
			require.NoError(t, d.SetSyncState(t.Context(), "artifact_local_machine_name", "local"))
			_, err := d.getWriter().ExecContext(t.Context(), `CREATE TRIGGER fail_pushed_write BEFORE INSERT ON `+tc.table+` BEGIN SELECT RAISE(ABORT, '`+tc.message+`'); END`)
			require.NoError(t, err)
			created, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.ErrorContains(t, err, tc.message)
			assert.False(t, created)
			for _, table := range []string{"sessions", "messages", "usage_events", "session_external_parents", "session_push_receipts", "artifact_export_queue"} {
				var count int
				require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
				assert.Zero(t, count, table)
			}
			_, err = d.getWriter().ExecContext(t.Context(), "DROP TRIGGER fail_pushed_write")
			require.NoError(t, err)
			created, err = d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			assert.True(t, created, "rollback must not reserve the receipt")
		})
	}
}

func TestSessionPushParentWithExistingLinks(t *testing.T) {
	for _, mode := range []string{"sender", "receiver", "receiver wins"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "local-spawner", "project")
			insertSession(t, d, "launcher", "project")
			require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "local-spawner", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Task", SubagentSessionID: "snapshot"}}}}))
			receipt, write := sessionPushFixture("snapshot")
			if mode != "sender" {
				_, err := d.SetSessionExternalParent(ctx, "snapshot", "launcher")
				require.NoError(t, err)
			}
			if mode == "sender" {
				receipt.ExternalParentSessionID = new("launcher")
			} else if mode == "receiver wins" {
				insertSession(t, d, "source-parent", "project")
				_, err := d.SetSessionExternalParent(ctx, "source-parent", "snapshot")
				require.NoError(t, err)
				receipt.ExternalParentSessionID = new("source-parent")
			}
			created, err := d.ApplySessionPush(ctx, receipt, write)
			require.NoError(t, err)
			require.True(t, created)
			link, err := d.GetSessionExternalParent(ctx, "snapshot")
			require.NoError(t, err)
			assert.Equal(t, "launcher", link.ParentSessionID)
			assert.True(t, link.Applied)
			session, err := d.GetSession(ctx, "snapshot")
			require.NoError(t, err)
			assert.Equal(t, Ptr("launcher"), session.ParentSessionID)
		})
	}
}

func TestSessionPushRejectsParentCycleWithoutSaving(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	d.SetAssetsDir(dir)
	d.SetToolResultImages(config.ToolResultImagesOffload)
	insertSession(t, d, "parent", "proj")
	_, err := d.SetSessionExternalParent(ctx, "parent", "snapshot")
	require.NoError(t, err)
	receipt, write := sessionPushFixture("snapshot")
	write.Messages = []Message{testImageMessage("snapshot")}
	receipt.ExternalParentSessionID = new("parent")
	created, err := d.ApplySessionPush(ctx, receipt, write)
	require.ErrorIs(t, err, ErrSessionPushConflict)
	assert.False(t, created)
	session, err := d.GetSession(ctx, "snapshot")
	require.NoError(t, err)
	assert.Nil(t, session)
	reserved, err := d.SessionPushReservations(ctx, []string{"snapshot"})
	require.NoError(t, err)
	assert.False(t, reserved["snapshot"])
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestSessionPushRebuildPreservesReceiptsAndCuration(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	for _, id := range []string{"live", "trashed", "purged"} {
		receipt, write := sessionPushFixture(id)
		if id == "live" {
			receipt.ExternalParentSessionID = new("launcher")
		}
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
	link, err := destination.GetSessionExternalParent(ctx, "live")
	require.NoError(t, err)
	assert.Equal(t, "launcher", link.ParentSessionID)
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
	for _, id := range []string{"trashed", "purged"} {
		receipt, write := sessionPushFixture(id)
		created, err := destination.ApplySessionPush(ctx, receipt, write)
		assert.False(t, created)
		require.ErrorIs(t, err, ErrSessionPushConflict)
	}
}

func TestSessionPushStoragePolicies(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
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

func TestLoadSessionPushDataExactLiveSession(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"parent", "trashed"} {
		_, write := sessionPushFixture(id)
		write.Session.FirstMessage = Ptr("first snapshot")
		write.Session.SessionName = Ptr("agent title")
		_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
		require.NoError(t, err)
	}
	require.NoError(t, d.RenameSession(ctx, "parent", Ptr("local title")))
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
	assert.Equal(t, Ptr("local title"), session.DisplayName, "keep agent and local names separate for export")
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

func TestSessionPushQueuedCleanupPreservesSnapshot(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		name := "immediate"
		if rebuild {
			name = "rebuilt-queue"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			receipt, write := sessionPushFixture("child")
			write.Session.ParentSessionID = Ptr("historical-absent-parent")
			write.Session.RelationshipType = "subagent"
			_, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "native-parent", Agent: "claude", Machine: "local"}))
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "native-parent", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Task", SubagentSessionID: "child"}}}}))
			captured, err := d.SubagentChildSessionIDs(t.Context(), []string{"native-parent"})
			require.NoError(t, err)
			assert.Empty(t, captured, "receipt-owned children never become cleanup targets")
			// A persisted queue may predate the receipt fence; it must also be safe.
			require.NoError(t, d.QueueSubagentParentCleanupRepairs(t.Context(), []string{"child"}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "native-parent", nil))
			if rebuild {
				path := d.Path()
				require.NoError(t, d.Close())
				d = testDB(t)
				require.NoError(t, d.CopyArchiveIdentityFrom(t.Context(), path))
				require.NoError(t, d.CopyAttachedArchiveStateFrom(t.Context(), path))
				require.NoError(t, d.CopySyncStateFrom(t.Context(), path))
				_, err = d.CopyOrphanedDataFrom(t.Context(), path)
				require.NoError(t, err)
			}
			before, err := d.GetSessionFull(t.Context(), "child")
			require.NoError(t, err)
			require.NotNil(t, before)
			require.Equal(t, Ptr("historical-absent-parent"), before.ParentSessionID)
			_, err = d.RepairQueuedSubagentParentsContext(t.Context(), nil)
			require.NoError(t, err)
			after, err := d.GetSessionFull(t.Context(), "child")
			require.NoError(t, err)
			assert.Equal(t, before, after, "repair must preserve the entire accepted row")
			requireSessionPushReceipt(t, d, "child")
		})
	}
}

func TestSessionPushRejectsDirectTranscriptMutation(t *testing.T) {
	for _, mode := range []string{"batch", "append", "replace", "replace content", "incremental", "usage", "plan", "staged"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			receipt, write := sessionPushFixture("reserved")
			_, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			if strings.HasPrefix(mode, "staged") {
				require.NoError(t, d.UpsertParserCheckpoint(t.Context(), ParserCheckpoint{SessionID: "reserved", Agent: "claude", FilePath: "fixture", Offset: 10}, ParserCheckpointBlobs{Cursor: []byte("cursor"), HashState: []byte("hash")}))
			}
			before, err := d.GetSessionFull(t.Context(), "reserved")
			require.NoError(t, err)
			checkpoint, hasCheckpoint, err := d.GetParserCheckpoint(t.Context(), "reserved")
			require.NoError(t, err)
			blobs, hasBlobs, err := d.GetParserCheckpointBlobs(t.Context(), "reserved")
			require.NoError(t, err)
			switch mode {
			case "batch":
				_, replacement := sessionPushFixture("reserved")
				replacement.Messages[0].Content = "ordinary source replacement"
				_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{replacement})
			case "append":
				err = d.InsertMessages(t.Context(), []Message{{SessionID: "reserved", Ordinal: 1, Role: "assistant", Content: "new content"}})
			case "replace":
				err = d.ReplaceSessionMessages(t.Context(), "reserved", []Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "replacement"}})
			case "replace content":
				err = d.ReplaceSessionContent(t.Context(), "reserved", []Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "replacement"}}, SessionSignalUpdate{}, nil)
			case "incremental":
				err = d.UpdateSessionIncremental(t.Context(), "reserved", IncrementalSessionUpdate{MsgCount: 99})
			case "usage":
				err = d.ReplaceSessionUsageEvents(t.Context(), "reserved", []UsageEvent{{SessionID: "reserved", Source: "session", Model: "model", InputTokens: 999}})
			case "plan":
				err = d.ReplaceSessionPlanUsage(t.Context(), "reserved", &SessionPlanUsage{HasPlanUsage: true, DataStatus: "available"})
			case "staged":
				messages := []Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "replacement"}}
				err = d.ReplaceSessionContentStaged(t.Context(), "reserved", messages, newScratchStagedResults(t), nil, nil)
			}
			require.ErrorIs(t, err, ErrSessionPushConflict)
			messages, err := d.GetAllMessages(t.Context(), "reserved")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "first snapshot", messages[0].Content)
			requireSessionPushReceipt(t, d, "reserved")
			after, err := d.GetSessionFull(t.Context(), "reserved")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			afterCheckpoint, afterHasCheckpoint, err := d.GetParserCheckpoint(t.Context(), "reserved")
			require.NoError(t, err)
			assert.Equal(t, hasCheckpoint, afterHasCheckpoint)
			assert.Equal(t, checkpoint, afterCheckpoint)
			afterBlobs, afterHasBlobs, err := d.GetParserCheckpointBlobs(t.Context(), "reserved")
			require.NoError(t, err)
			assert.Equal(t, hasBlobs, afterHasBlobs)
			assert.Equal(t, blobs, afterBlobs)
		})
	}
}

func TestSessionPushLauncherParentEditableAfterNativeSpawnReference(t *testing.T) {
	for _, action := range []string{"clear", "move"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "first-parent", "proj")
			insertSession(t, d, "second-parent", "proj")
			receipt, write := sessionPushFixture("child")
			receipt.ExternalParentSessionID = new("first-parent")
			_, err := d.ApplySessionPush(ctx, receipt, write)
			require.NoError(t, err)
			link, err := d.GetSessionExternalParent(ctx, "child")
			require.NoError(t, err)
			require.True(t, link.Applied)
			_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: Session{ID: "spawner", Agent: "claude", Machine: "local", MessageCount: 1}, Messages: []Message{{SessionID: "spawner", Role: "assistant", HasToolUse: true, ToolCalls: []ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "child"}}}}, ReplaceMessages: true}})
			require.NoError(t, err)
			if action == "clear" {
				_, err = d.ClearSessionExternalParent(ctx, "child")
			} else {
				_, err = d.SetSessionExternalParent(ctx, "child", "second-parent")
			}
			require.NoError(t, err)
			child, err := d.GetSessionFull(ctx, "child")
			require.NoError(t, err)
			require.NotNil(t, child)
			if action == "clear" {
				assert.Nil(t, child.ParentSessionID)
			} else {
				assert.Equal(t, new("second-parent"), child.ParentSessionID)
			}
		})
	}
}
