package db

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionStream collects one session's records, with each message's text
// joined from its chunks.
type sessionStream struct {
	header   ConversationSessionHeader
	changes  []ConversationSessionChange
	text     map[string]string
	chunks   []ConversationSessionText
	trailer  *ConversationSessionTrailer
	sequence []string
}

func streamSession(t *testing.T, d *DB, opts ConversationSessionOptions) (sessionStream, error) {
	t.Helper()
	stream := sessionStream{text: map[string]string{}}
	err := d.ExportConversationSession(t.Context(), opts, func(record any) error {
		switch r := record.(type) {
		case ConversationSessionHeader:
			stream.header = r
		case ConversationSessionChange:
			stream.changes = append(stream.changes, r)
		case ConversationSessionText:
			stream.chunks = append(stream.chunks, r)
			stream.text[r.MessageID] += r.Text
		case ConversationSessionTrailer:
			stream.trailer = &r
		default:
			require.Failf(t, "unexpected record", "%T", record)
		}
		stream.sequence = append(stream.sequence, typeName(record))
		return nil
	})
	return stream, err
}

func typeName(record any) string {
	switch r := record.(type) {
	case ConversationSessionHeader:
		return "header"
	case ConversationSessionChange:
		return r.Type
	case ConversationSessionText:
		return "text"
	case ConversationSessionTrailer:
		return "trailer"
	}
	return "?"
}

func TestConversationSessionStreamIsTheWalkForOneSession(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Check the saved conversation.", SourceUUID: "ask"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "The saved reply.", SourceUUID: "reply"},
		{SessionID: "chat", Ordinal: 2, Role: "user", Content: "One more thing.", SourceUUID: "follow-up"},
	}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	// The first walk builds the index, so the edits below publish revisions.
	_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	msgs[1].Content = "The revised reply."
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs[:2]))
	// A later write elsewhere moves the archive revision past this session's.
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "other", Project: "sample", Machine: "local", Agent: "codex"}))
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "other", Ordinal: 0, Role: "user", Content: "Unrelated.", SourceUUID: "other-ask"}}))

	walk, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	var walked []ConversationChange
	head := ""
	for _, change := range walk.Changes {
		head = change.Revision
		if change.SessionID == "chat" {
			walked = append(walked, change)
		}
	}
	require.Len(t, walked, 3)

	stream, err := streamSession(t, d, ConversationSessionOptions{SessionID: "chat"})
	require.NoError(t, err)
	assert.Equal(t, ConversationSessionHeader{
		Type: "header", SchemaVersion: 1, ArchiveID: walk.ArchiveID, DatabaseID: walk.DatabaseID, SessionID: "chat",
		Revision: head, Session: &ConversationSession{Agent: "claude", Automation: "interactive"}, Project: stream.header.Project,
	}, stream.header, "the header carries the archive revision, not the session's newest")
	require.NotNil(t, stream.header.Project)
	assert.Equal(t, "sample", stream.header.Project.DisplayLabel)

	require.Len(t, stream.changes, 3)
	for i, change := range stream.changes {
		assert.Equal(t, walked[i].MessageID, change.MessageID, "change %d follows the walk's order", i)
		assert.Equal(t, walked[i].Revision, change.Revision)
		assert.Equal(t, walked[i].Deleted, change.Deleted)
		assert.Equal(t, walked[i].Digest, change.Digest)
	}
	byText := map[string]bool{}
	var tombstones int
	for _, change := range stream.changes {
		if change.Deleted {
			tombstones++
			assert.Empty(t, stream.text[change.MessageID], "a tombstone has no text")
			continue
		}
		byText[stream.text[change.MessageID]] = true
	}
	assert.Equal(t, map[string]bool{"Check the saved conversation.": true, "The revised reply.": true}, byText)
	assert.Equal(t, 1, tombstones, "the removed follow-up appears as its tombstone")
	require.NotNil(t, stream.trailer)
	assert.Equal(t, 3, stream.trailer.Messages)
	assert.Equal(t, "header", stream.sequence[0])
	assert.Equal(t, "trailer", stream.sequence[len(stream.sequence)-1])
}

func TestConversationSessionStreamMasksAndChunksText(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "leaky", Project: "sample", Machine: "local", Agent: "claude"}))
	stored := "use AKIA" + "7QHWN2DKR4FYPLJM for café"
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "leaky", Ordinal: 0, Role: "user", Content: stored, SourceUUID: "ask"}}))

	masked, err := streamSession(t, d, ConversationSessionOptions{SessionID: "leaky", MaxBytes: 4})
	require.NoError(t, err)
	require.Len(t, masked.changes, 1)
	message := masked.changes[0]
	want := "use AKIA…PLJM for café"
	assert.Equal(t, want, masked.text[message.MessageID])
	assert.True(t, message.Redacted)
	assert.EqualValues(t, len(want), message.TextBytes, "text_bytes counts the masked text the stream writes")
	var offset int64
	for _, chunk := range masked.chunks {
		assert.Equal(t, offset, chunk.Offset, "chunks are contiguous")
		assert.LessOrEqual(t, len(chunk.Text), 4)
		assert.True(t, utf8.ValidString(chunk.Text), "chunk %q splits a character", chunk.Text)
		offset += int64(len(chunk.Text))
	}

	revealed, err := streamSession(t, d, ConversationSessionOptions{SessionID: "leaky", RevealSecrets: true})
	require.NoError(t, err)
	assert.Equal(t, stored, revealed.text[message.MessageID])
	assert.False(t, revealed.changes[0].Redacted)
	assert.EqualValues(t, len(stored), revealed.changes[0].TextBytes)
}

func TestConversationSessionStreamVisibleTextLeavesOutReasoning(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(ctx, []Message{{
		SessionID: "chat", Ordinal: 0, Role: "assistant", Content: "[Thinking]\nweigh options\nThe answer.",
		ThinkingText: "weigh options", DialogueText: new("The answer."), SourceUUID: "reply",
	}}))
	stream, err := streamSession(t, d, ConversationSessionOptions{SessionID: "chat", TextMode: "visible"})
	require.NoError(t, err)
	assert.Equal(t, "visible", stream.header.TextMode)
	require.Len(t, stream.changes, 1)
	assert.Equal(t, "The answer.", stream.text[stream.changes[0].MessageID])
}

func TestConversationSessionStreamReportsTrashedSession(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Hello.", SourceUUID: "ask"}}))
	_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession(ctx, "chat"))

	stream, err := streamSession(t, d, ConversationSessionOptions{SessionID: "chat"})
	require.NoError(t, err)
	require.Len(t, stream.changes, 2)
	var session, message ConversationSessionChange
	for _, change := range stream.changes {
		if change.Type == "session" {
			session = change
		} else {
			message = change
		}
	}
	assert.True(t, session.Deleted, "the session change reports the trash")
	assert.True(t, message.Deleted)
	assert.Empty(t, stream.text, "a trashed session writes no text")
	assert.Equal(t, 1, stream.trailer.Messages)
}

func TestConversationSessionStreamCarriesToolSummaryOnRequest(t *testing.T) {
	d := testDB(t)
	toolSession(t, d, "chat",
		timedCall("Bash", "run-1", "2026-03-01T10:01:00Z", "2026-03-01T10:01:40Z"),
		ToolCall{ToolName: "Skill", Category: "Skill", ToolUseID: "skill-1", SkillName: "commit"},
	)
	walk, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{ToolSummaries: true})
	require.NoError(t, err)
	var listed ConversationChange
	for _, change := range walk.Changes {
		if change.Type == conversationToolChangeType {
			listed = change
		}
	}
	require.NotNil(t, listed.ToolSummary)

	stream, err := streamSession(t, d, ConversationSessionOptions{SessionID: "chat", ToolSummaries: true})
	require.NoError(t, err)
	last := stream.changes[len(stream.changes)-1]
	assert.Equal(t, conversationToolChangeType, last.Type, "the summary follows the messages")
	assert.Equal(t, listed.Revision, last.Revision)
	require.NotNil(t, last.ToolSummary)
	assert.Equal(t, ConversationToolSummary{
		DurationMs: 40_000,
		Categories: []ConversationToolCategory{
			{Category: "Bash", Calls: 1, Timed: 1, DurationMs: 40_000},
			{Category: "Skill", Calls: 1},
		},
		Skills: []ConversationSkillUse{{Name: "commit", Calls: 1}},
	}, *last.ToolSummary)
	assert.Equal(t, 2, stream.trailer.Messages, "the summary is not a message")

	without, err := streamSession(t, d, ConversationSessionOptions{SessionID: "chat"})
	require.NoError(t, err)
	for _, change := range without.changes {
		assert.NotEqual(t, conversationToolChangeType, change.Type)
	}
}

func TestConversationSessionStreamRejectsUnknownSession(t *testing.T) {
	d := testDB(t)
	wrote := false
	err := d.ExportConversationSession(t.Context(), ConversationSessionOptions{SessionID: "missing"}, func(any) error {
		wrote = true
		return nil
	})
	require.ErrorIs(t, err, ErrConversationSessionNotFound)
	assert.False(t, wrote, "an unknown session writes no header")
}
