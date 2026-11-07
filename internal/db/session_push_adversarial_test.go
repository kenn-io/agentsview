package db

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestSessionPushReservesOrdinaryWriterOwnership(t *testing.T) {
	d := testDB(t)
	receipt, first := sessionPushFixture("reserved")
	_, err := d.ApplySessionPush(t.Context(), receipt, first)
	require.NoError(t, err)
	_, replacement := sessionPushFixture("reserved")
	replacement.Messages[0].Content = "ordinary source replacement"
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{replacement})
	require.ErrorIs(t, err, ErrSessionPushConflict)
	messages, err := d.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "first snapshot", messages[0].Content)
	created, err := d.ApplySessionPush(t.Context(), receipt, first)
	require.NoError(t, err)
	assert.False(t, created)
}

func TestSessionPushRebuildReservesIDsBeforeParsing(t *testing.T) {
	source := testDB(t)
	receipt, first := sessionPushFixture("reserved")
	_, err := source.ApplySessionPush(t.Context(), receipt, first)
	require.NoError(t, err)
	path := source.Path()
	require.NoError(t, source.Close())
	replacement := testDB(t)
	// This is the existing engine's pre-parse ownership preparation sequence.
	require.NoError(t, replacement.CopyArchiveIdentityFrom(t.Context(), path))
	require.NoError(t, replacement.CopyAttachedArchiveStateFrom(t.Context(), path))
	_, parsed := sessionPushFixture("reserved")
	parsed.Messages[0].Content = "colliding provider source"
	_, err = replacement.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{parsed})
	require.ErrorIs(t, err, ErrSessionPushConflict)
	require.NoError(t, replacement.CopySyncStateFrom(t.Context(), path))
	_, err = replacement.CopyOrphanedDataFrom(t.Context(), path)
	require.NoError(t, err)
	messages, err := replacement.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "first snapshot", messages[0].Content)
	created, err := replacement.ApplySessionPush(t.Context(), receipt, first)
	require.NoError(t, err)
	assert.False(t, created)
}

func TestSessionPushDoesNotPromoteReferencesToOtherSessionWrites(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "unrelated", Agent: "claude", Machine: "local", Project: "example"}))
	before, err := d.GetSessionFull(t.Context(), "unrelated")
	require.NoError(t, err)
	receipt, write := sessionPushFixture("selected")
	write.Messages[0].ToolCalls = []ToolCall{{ToolName: "Task", SubagentSessionID: "unrelated"}}
	_, err = d.ApplySessionPush(t.Context(), receipt, write)
	require.NoError(t, err)
	after, err := d.GetSessionFull(t.Context(), "unrelated")
	require.NoError(t, err)
	assert.Equal(t, before, after, "ingest must not mutate a referenced session")
	_, err = d.LinkSubagentSessionsContext(t.Context())
	require.NoError(t, err)
	after, err = d.GetSessionFull(t.Context(), "unrelated")
	require.NoError(t, err)
	assert.Equal(t, before, after, "later hierarchy repair must keep pushed references passive")
	rows, err := d.GetAllMessages(t.Context(), "selected")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].ToolCalls, 1)
	assert.Equal(t, "unrelated", rows[0].ToolCalls[0].SubagentSessionID)
}

func TestSessionPushConflictAndRetryDoNotPublishAssets(t *testing.T) {
	for _, existing := range []string{"unowned", "pushed"} {
		t.Run(existing, func(t *testing.T) {
			d := testDB(t)
			dir := t.TempDir()
			d.SetAssetsDir(dir)
			receipt, write := sessionPushFixture("selected")
			write.Messages = []Message{testImageMessage("selected")}
			write.Session.UserMessageCount = 0
			if existing == "unowned" {
				_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
				require.NoError(t, err)
			} else {
				_, err := d.ApplySessionPush(t.Context(), receipt, write)
				require.NoError(t, err)
			}
			d.SetToolResultImages(config.ToolResultImagesOffload)
			created, err := d.ApplySessionPush(t.Context(), receipt, write)
			if existing == "unowned" {
				require.ErrorIs(t, err, ErrSessionPushConflict)
			} else {
				require.NoError(t, err)
			}
			assert.False(t, created)
			var files []string
			require.NoError(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					files = append(files, path)
				}
				return err
			}))
			assert.Empty(t, files, "a rejected or unchanged push must not publish image objects")
		})
	}
}

func TestSessionPushRejectsDirectTranscriptMutation(t *testing.T) {
	for _, mode := range []string{"append", "replace", "usage", "plan"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			receipt, write := sessionPushFixture("reserved")
			_, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			switch mode {
			case "append":
				err = d.InsertMessages(t.Context(), []Message{{SessionID: "reserved", Ordinal: 1, Role: "assistant", Content: "new content"}})
			case "replace":
				err = d.ReplaceSessionMessages(t.Context(), "reserved", []Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "replacement"}})
			case "usage":
				err = d.ReplaceSessionUsageEvents(t.Context(), "reserved", []UsageEvent{{SessionID: "reserved", Source: "session", Model: "model", InputTokens: 999}})
			case "plan":
				err = d.ReplaceSessionPlanUsage(t.Context(), "reserved", &SessionPlanUsage{HasPlanUsage: true, DataStatus: "available"})
			}
			require.ErrorIs(t, err, ErrSessionPushConflict)
		})
	}
}
