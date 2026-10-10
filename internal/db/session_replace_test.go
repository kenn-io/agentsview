package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedReplaceSession stores a renamed three-message session with a tool call, two pins, and a usage event.
func seedReplaceSession(t *testing.T, d *DB) []Message {
	t.Helper()
	ctx := t.Context()
	write := messageCountWrite("replace", 3)
	write.Session.Agent = "chatgpt"
	write.Session.FilePath = Ptr("/exports/replace.json")
	write.Messages[1].HasToolUse = true
	write.Messages[1].ToolCalls = []ToolCall{{
		ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-1",
		ResultContent: "build output", ResultContentLength: 12,
	}}
	_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{write})
	require.NoError(t, err)
	require.NoError(t, d.RenameSession(ctx, "replace", Ptr("Saved title")))
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "replace", []UsageEvent{{
		Source: "session", Model: "gpt-test", InputTokens: 10,
		OccurredAt: "2026-10-01T00:00:00Z", DedupKey: "usage-1",
	}}))
	stored, err := d.GetAllMessages(ctx, "replace")
	require.NoError(t, err)
	for _, m := range stored[:2] {
		_, err := d.PinMessage(ctx, "replace", m.ID, Ptr("note"))
		require.NoError(t, err)
	}
	return stored
}

func replaceWrite(contents ...string) SessionBatchWrite {
	write := messageCountWrite("replace", len(contents))
	write.Session.Agent = "chatgpt"
	for i, c := range contents {
		write.Messages[i].Content = c
		write.Messages[i].ContentLength = len(c)
	}
	return write
}

func trashedSessionIDs(t *testing.T, d *DB) []string {
	t.Helper()
	trashed, err := d.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	var ids []string
	for _, s := range trashed {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestReplaceSessionKeepingTrashedCopy(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	before := seedReplaceSession(t, d)

	copyID, err := d.ReplaceSessionKeepingTrashedCopy(ctx, replaceWrite("new first", "new second"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(copyID, "replace:replaced:"), copyID)
	assert.Equal(t, []string{copyID}, trashedSessionIDs(t, d))

	live, err := d.GetAllMessages(ctx, "replace")
	require.NoError(t, err)
	require.Len(t, live, 2)
	assert.Equal(t, "new second", live[1].Content)

	full, err := d.GetSessionFull(ctx, copyID)
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Equal(t, "Saved title", *full.DisplayName)
	assert.Nil(t, full.FilePath, "the copy must not share the source file identity")
	old, err := d.GetAllMessages(ctx, copyID)
	require.NoError(t, err)
	require.Len(t, old, 3)
	for i := range old {
		assert.Equal(t, before[i].Content, old[i].Content)
	}
	require.Len(t, old[1].ToolCalls, 1)
	assert.Equal(t, "build output", old[1].ToolCalls[0].ResultContent)
	pins, err := d.ListPinnedMessages(ctx, copyID, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	assert.ElementsMatch(t, []int{0, 1}, []int{pins[0].Ordinal, pins[1].Ordinal})
	assert.Equal(t, "note", *pins[0].Note)

	for _, id := range []string{"replace", copyID} {
		events, err := d.GetUsageEvents(ctx, id)
		require.NoError(t, err)
		require.Len(t, events, 1, id)
		assert.Equal(t, "usage-1", events[0].DedupKey, id)
	}

	restored, err := d.RestoreSession(ctx, copyID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), restored)
}

func TestReplaceSessionKeepingTrashedCopyRollsBack(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pinLoss bool
	}{
		{"invalid ordinals", false},
		{"pin loss after copy", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			before := seedReplaceSession(t, d)

			write := replaceWrite("first", "second")
			if tt.pinLoss {
				_, err := d.getWriter().Exec(ctx, `CREATE TRIGGER reject_replacement_copy BEFORE INSERT ON messages
			WHEN NEW.session_id LIKE 'replace:replaced:%' AND EXISTS(SELECT 1 FROM messages WHERE session_id = 'replace' AND content = 'Changed pinned turn')
			BEGIN SELECT RAISE(ABORT, 'replacement rejected'); END`)
				require.NoError(t, err)
				write = replaceWrite("Changed pinned turn", "Changed reply")
				write.KeepTrashedCopyOnlyOnPinLoss = true
			} else {
				write.Messages[1].Ordinal = 0
			}
			_, err := d.ReplaceSessionKeepingTrashedCopy(ctx, write)
			require.Error(t, err)
			if tt.pinLoss {
				require.ErrorContains(t, err, "replacement rejected")
			}

			assert.Empty(t, trashedSessionIDs(t, d))
			after, err := d.GetAllMessages(ctx, "replace")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			pins, err := d.ListPinnedMessages(ctx, "replace", "")
			require.NoError(t, err)
			require.Len(t, pins, 2)
			assert.Equal(t, Ptr("note"), pins[0].Note)
		})
	}
}

func TestReplaceSessionKeepingTrashedCopyOnlyOnPinLoss(t *testing.T) {
	d := testDB(t)
	before := seedReplaceSession(t, d)
	write := replaceWrite(before[0].Content, before[1].Content, "Changed unpinned turn")
	write.Messages[0] = before[0]
	write.Messages[1] = before[1]
	write.KeepTrashedCopyOnlyOnPinLoss = true
	copyID, err := d.ReplaceSessionKeepingTrashedCopy(t.Context(), write)
	require.NoError(t, err)
	assert.Empty(t, copyID)
	assert.Empty(t, trashedSessionIDs(t, d))
	pins, err := d.ListPinnedMessages(t.Context(), "replace", "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	assert.Equal(t, Ptr("note"), pins[0].Note)
	stored, err := d.GetAllMessages(t.Context(), "replace")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "Changed unpinned turn", stored[2].Content)
}

func TestReplaceSessionKeepingTrashedCopyRefusals(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		d := testDB(t)
		write := replaceWrite()
		write.Messages = seedReplaceSession(t, d)
		_, err := d.ReplaceSessionKeepingTrashedCopy(t.Context(), write)
		require.ErrorIs(t, err, ErrReplaceUnchanged)
		assert.Empty(t, trashedSessionIDs(t, d))
	})
	t.Run("trashed", func(t *testing.T) {
		d := testDB(t)
		seedReplaceSession(t, d)
		require.NoError(t, d.SoftDeleteSession(t.Context(), "replace"))
		_, err := d.ReplaceSessionKeepingTrashedCopy(t.Context(), replaceWrite("first"))
		require.ErrorIs(t, err, ErrSessionTrashed)
		assert.Equal(t, []string{"replace"}, trashedSessionIDs(t, d))
	})
}

func TestReplaceSessionKeepingTrashedCopyPinIdentities(t *testing.T) {
	msg := func(uuid, content string) Message {
		return Message{SourceUUID: uuid, Role: "user", Content: content, ContentLength: len(content)}
	}
	d := testDB(t)
	write := replaceWrite()
	write.Session.Agent = "claude-ai"
	write.Session.MessageCount = 2
	write.Messages = []Message{msg("a", "same"), msg("", "other")}
	for i := range write.Messages {
		write.Messages[i].SessionID = "replace"
		write.Messages[i].Ordinal = i
	}
	_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	stored, err := d.GetAllMessages(t.Context(), "replace")
	require.NoError(t, err)
	for _, i := range []int{0, 1} {
		_, err := d.PinMessage(t.Context(), "replace", stored[i].ID, Ptr("saved note"))
		require.NoError(t, err)
	}
	write.Messages = []Message{msg("a", "other")}
	write.Session.MessageCount = 1
	write.KeepTrashedCopyOnlyOnPinLoss = true
	for i := range write.Messages {
		write.Messages[i].SessionID = "replace"
		write.Messages[i].Ordinal = i
	}
	copyID, err := d.ReplaceSessionKeepingTrashedCopy(t.Context(), write)
	require.NoError(t, err)
	require.NotEmpty(t, copyID)
	pins, err := d.ListPinnedMessages(t.Context(), "replace", "")
	require.NoError(t, err)
	var ordinals []int
	for _, pin := range pins {
		ordinals = append(ordinals, pin.Ordinal)
		assert.Equal(t, Ptr("saved note"), pin.Note)
	}
	assert.Equal(t, []int{0}, ordinals)

	oldPins, err := d.ListPinnedMessages(t.Context(), copyID, "")
	require.NoError(t, err)
	require.Len(t, oldPins, 2)
	assert.ElementsMatch(t, []int{0, 1}, []int{oldPins[0].Ordinal, oldPins[1].Ordinal})
	assert.Equal(t, Ptr("saved note"), oldPins[0].Note)
}
