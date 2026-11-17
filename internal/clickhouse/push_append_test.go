package clickhouse

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func appendTestMessages() []db.Message {
	return []db.Message{
		{ID: 1, SessionID: "s", Ordinal: 0, Role: "user", Content: "first", Timestamp: "2026-01-10T00:00:00Z"},
		{
			ID: 2, SessionID: "s", Ordinal: 1, Role: "assistant", Content: "second", Timestamp: "2026-01-10T00:01:00Z",
			ToolCalls: []db.ToolCall{{ToolName: "Bash", ToolUseID: "t1", InputJSON: `{}`, ResultEvents: []db.ToolResultEvent{{
				Source: "tool_execution", Status: "completed", Content: "ok", Timestamp: "2026-01-10T00:01:30Z",
			}}}},
		},
		{ID: 3, SessionID: "s", Ordinal: 3, Role: "user", Content: "third", Timestamp: "2026-01-10T00:02:00Z"},
	}
}

func appendTestImages() []db.ImageTextRow {
	return []db.ImageTextRow{
		{SessionID: "s", SourceKind: "tool_call", MessageOrdinal: 1, Text: "kept"},
		{SessionID: "s", SourceKind: "tool_call", MessageOrdinal: 3, Text: "appended"},
	}
}

// The hash a push stores for all of a session's rows equals the hash the next
// push computes for the same leading messages after the session grew, which
// is what lets it append.
func TestMessageRowsFingerprintsMatchStoredRowsAfterGrowth(t *testing.T) {
	msgs := appendTestMessages()
	images := appendTestImages()
	stored, err := messageRowsFingerprints(msgs[:2], images[:1], 2)
	require.NoError(t, err)
	require.NotEmpty(t, stored[0])

	grown, err := messageRowsFingerprints(msgs, images, 2, 3)
	require.NoError(t, err)
	assert.Equal(t, stored[0], grown[0], "image text after the kept messages is not part of their hash")
	assert.NotEqual(t, stored[0], grown[1])

	slices.Reverse(images)
	grown, err = messageRowsFingerprints(msgs, images, 2)
	require.NoError(t, err)
	assert.Equal(t, stored[0], grown[0], "image text is hashed with its message whatever order it is listed in")
}

// When a tool result arrives for the last stored message, the hash of the
// messages before it still matches, so the push can write only that message
// again and append the rest.
func TestMessageRowsFingerprintsLeadingRowsSurviveLateToolResult(t *testing.T) {
	msgs := appendTestMessages()[:2]
	msgs[1].ToolCalls[0].ResultEvents = nil
	stored, err := messageRowsFingerprints(msgs, nil, 2, 1)
	require.NoError(t, err)

	grown := appendTestMessages()
	got, err := messageRowsFingerprints(grown, nil, 2, 1)
	require.NoError(t, err)
	assert.NotEqual(t, stored[0], got[0], "the result changes the last stored message's rows")
	assert.Equal(t, stored[1], got[1])
}

// Any change a kept row would carry changes the hash of the kept messages, so
// the push does not keep them.
func TestMessageRowsFingerprintsTrackKeptRows(t *testing.T) {
	stored, err := messageRowsFingerprints(appendTestMessages(), appendTestImages(), 2)
	require.NoError(t, err)
	for name, change := range map[string]func(ms []db.Message, images []db.ImageTextRow){
		"message content":   func(ms []db.Message, _ []db.ImageTextRow) { ms[0].Content = "edited" },
		"message id":        func(ms []db.Message, _ []db.ImageTextRow) { ms[0].ID = 9 },
		"tool result":       func(ms []db.Message, _ []db.ImageTextRow) { ms[1].ToolCalls[0].ResultContent = "late result" },
		"result event":      func(ms []db.Message, _ []db.ImageTextRow) { ms[1].ToolCalls[0].ResultEvents[0].Status = "errored" },
		"kept image text":   func(_ []db.Message, images []db.ImageTextRow) { images[0].Text = "changed" },
		"tool call removed": func(ms []db.Message, _ []db.ImageTextRow) { ms[1].ToolCalls = nil },
	} {
		ms, images := appendTestMessages(), appendTestImages()
		change(ms, images)
		got, err := messageRowsFingerprints(ms, images, 2)
		require.NoError(t, err, name)
		require.NotEmpty(t, got[0], name)
		assert.NotEqual(t, stored[0], got[0], name)
	}
}

func TestMessageRowsFingerprintsRejectUnorderedOrdinals(t *testing.T) {
	ms := appendTestMessages()
	ms[2].Ordinal = 1
	got, err := messageRowsFingerprints(ms, nil, 1, 2, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"", "", ""}, got)
}
