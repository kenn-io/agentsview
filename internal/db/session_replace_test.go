package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedReplaceSession(t *testing.T, d *DB) []Message {
	t.Helper()
	write := messageCountWrite("replace", 3)
	write.Session.Agent = "chatgpt"
	write.Session.FilePath = Ptr("/exports/replace.json")
	write.Messages[1].Role = "assistant"
	write.Messages[1].HasToolUse = true
	write.Messages[1].ToolCalls = []ToolCall{{
		ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-1",
		InputJSON:           `{"command":"make"}`,
		ResultContent:       "build output",
		ResultContentLength: 12,
		ResultEvents: []ToolResultEvent{{
			Source: "tool_result", Status: "ok",
			Content: "build output", ContentLength: 12,
		}},
	}}
	_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	name := "Saved title"
	require.NoError(t, d.RenameSession(t.Context(), "replace", &name))
	stored, err := d.GetAllMessages(t.Context(), "replace")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	for _, i := range []int{0, 1} {
		_, err := d.PinMessage(t.Context(), "replace", stored[i].ID, Ptr("note"))
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

func trashedReplaceCopies(t *testing.T, d *DB) []Session {
	t.Helper()
	trashed, err := d.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	var copies []Session
	for _, s := range trashed {
		if strings.HasPrefix(s.ID, "replace:replaced:") {
			copies = append(copies, s)
		}
	}
	return copies
}

func TestReplaceSessionKeepingTrashedCopy(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	before := seedReplaceSession(t, d)

	copyID, err := d.ReplaceSessionKeepingTrashedCopy(ctx, replaceWrite("new first", "new second"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(copyID, "replace:replaced:"), copyID)

	live, err := d.GetAllMessages(ctx, "replace")
	require.NoError(t, err)
	require.Len(t, live, 2)
	assert.Equal(t, "new first", live[0].Content)
	assert.Equal(t, "new second", live[1].Content)
	session, err := d.GetSession(ctx, "replace")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "Saved title", *session.DisplayName)

	copies := trashedReplaceCopies(t, d)
	require.Len(t, copies, 1)
	assert.Equal(t, copyID, copies[0].ID)
	require.NotNil(t, copies[0].DisplayName)
	assert.Equal(t, "Saved title", *copies[0].DisplayName)
	full, err := d.GetSessionFull(ctx, copyID)
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Nil(t, full.FilePath, "the copy must not share the source file identity")

	old, err := d.GetAllMessages(ctx, copyID)
	require.NoError(t, err)
	require.Len(t, old, 3)
	for i := range old {
		assert.Equal(t, before[i].Content, old[i].Content)
		assert.Equal(t, before[i].Ordinal, old[i].Ordinal)
	}
	require.Len(t, old[1].ToolCalls, 1)
	assert.Equal(t, "build output", old[1].ToolCalls[0].ResultContent)
	require.Len(t, old[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "build output", old[1].ToolCalls[0].ResultEvents[0].Content)

	pins, err := d.ListPinnedMessages(ctx, copyID, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	ordinals := map[int]bool{}
	for _, p := range pins {
		ordinals[p.Ordinal] = true
		require.NotNil(t, p.Note)
		assert.Equal(t, "note", *p.Note)
	}
	assert.Equal(t, map[int]bool{0: true, 1: true}, ordinals)

	restored, err := d.RestoreSession(ctx, copyID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), restored)
	got, err := d.GetSession(ctx, copyID)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestReplaceSessionKeepingTrashedCopyRollsBack(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	before := seedReplaceSession(t, d)
	pinsBefore, err := d.ListPinnedMessages(ctx, "replace", "")
	require.NoError(t, err)

	write := replaceWrite("first", "second")
	write.Messages[1].Ordinal = 0
	_, err = d.ReplaceSessionKeepingTrashedCopy(ctx, write)
	require.Error(t, err)

	assert.Empty(t, trashedReplaceCopies(t, d))
	after, err := d.GetAllMessages(ctx, "replace")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	pinsAfter, err := d.ListPinnedMessages(ctx, "replace", "")
	require.NoError(t, err)
	assert.Equal(t, pinsBefore, pinsAfter)
}

func TestReplaceSessionKeepingTrashedCopyUnchanged(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	stored := seedReplaceSession(t, d)

	write := replaceWrite("x", "y", "z")
	write.Messages = stored
	_, err := d.ReplaceSessionKeepingTrashedCopy(ctx, write)
	require.ErrorIs(t, err, ErrReplaceUnchanged)
	assert.Empty(t, trashedReplaceCopies(t, d))
}

func TestReplaceSessionKeepingTrashedCopyRejectsTrashed(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedReplaceSession(t, d)
	require.NoError(t, d.SoftDeleteSession(ctx, "replace"))

	_, err := d.ReplaceSessionKeepingTrashedCopy(ctx, replaceWrite("first"))
	require.ErrorIs(t, err, ErrSessionTrashed)
	assert.Empty(t, trashedReplaceCopies(t, d))
}

func TestReplaceSessionKeepingTrashedCopyKeepsUsageEvents(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedReplaceSession(t, d)
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "replace", []UsageEvent{{
		Source: "session", Model: "gpt-test", InputTokens: 10, OutputTokens: 5,
		OccurredAt: "2026-10-01T00:00:00Z", DedupKey: "usage-1",
	}}))

	copyID, err := d.ReplaceSessionKeepingTrashedCopy(ctx, replaceWrite("new first", "new second"))
	require.NoError(t, err)

	for _, id := range []string{"replace", copyID} {
		events, err := d.GetUsageEvents(ctx, id)
		require.NoError(t, err)
		require.Len(t, events, 1, id)
		assert.Equal(t, "usage-1", events[0].DedupKey, id)
		assert.Equal(t, 10, events[0].InputTokens, id)
	}
}
