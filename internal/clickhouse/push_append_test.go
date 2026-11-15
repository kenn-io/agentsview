package clickhouse

import (
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

// The fingerprint a push stores for all of a session's rows equals the
// fingerprint the next push computes for the same leading messages after the
// session grew, which is what lets it append.
func TestMessageRowsFingerprintMatchesStoredPrefixAfterGrowth(t *testing.T) {
	full := appendTestMessages()
	images := appendTestImages()
	stored, err := messageRowsFingerprint(full[:2], images[:1], 2)
	require.NoError(t, err)
	require.NotEmpty(t, stored)

	prefix, err := messageRowsFingerprint(full, images, 2)
	require.NoError(t, err)
	assert.Equal(t, stored, prefix, "image text after the kept messages is not part of the prefix")

	all, err := messageRowsFingerprint(full, images, len(full))
	require.NoError(t, err)
	assert.NotEqual(t, stored, all)
}

// Any change a kept row would carry changes the prefix fingerprint, so the
// push rewrites the session instead of appending.
func TestMessageRowsFingerprintTracksKeptRows(t *testing.T) {
	stored, err := messageRowsFingerprint(appendTestMessages(), appendTestImages(), 2)
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
		got, err := messageRowsFingerprint(ms, images, 2)
		require.NoError(t, err, name)
		require.NotEmpty(t, got, name)
		assert.NotEqual(t, stored, got, name)
	}
}

func TestMessageRowsFingerprintRejectsUnorderedOrdinals(t *testing.T) {
	ms := appendTestMessages()
	ms[2].Ordinal = 1
	got, err := messageRowsFingerprint(ms, nil, 2)
	require.NoError(t, err)
	assert.Empty(t, got)
}
