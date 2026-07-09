package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestBuildRecallExtractionChunksIncludesOnlyUserAssistantText(t *testing.T) {
	chunks := BuildRecallExtractionChunks("session-1", []db.Message{
		{
			Ordinal: 0,
			Role:    "system",
			Content: "system instruction",
		},
		{
			Ordinal: 1,
			Role:    "user",
			Content: "We need a project recall that tracks decisions.",
		},
		{
			Ordinal: 2,
			Role:    "assistant",
			Content: "I propose extracting structured facts from sessions.",
		},
		{
			Ordinal:  3,
			Role:     "user",
			IsSystem: true,
			Content:  "promoted system marker",
		},
		{
			Ordinal: 4,
			Role:    "tool",
			Content: "tool output should not feed recall extraction",
		},
		{
			Ordinal: 5,
			Role:    "assistant",
			Content: "   ",
		},
	}, RecallExtractionChunkOptions{MaxChars: 1000})

	require.Len(t, chunks, 1)
	assert.Equal(t, "session-1", chunks[0].SessionID)
	assert.Equal(t, 0, chunks[0].Index)
	assert.Equal(t, 1, chunks[0].StartOrdinal)
	assert.Equal(t, 2, chunks[0].EndOrdinal)
	require.Len(t, chunks[0].Messages, 2)
	assert.Equal(t, "user", chunks[0].Messages[0].Role)
	assert.Equal(t, 1, chunks[0].Messages[0].Ordinal)
	assert.Equal(t, "assistant", chunks[0].Messages[1].Role)
	assert.Equal(t, 2, chunks[0].Messages[1].Ordinal)
	assert.Contains(t, chunks[0].Text, "[1 user] We need a project recall")
	assert.Contains(t, chunks[0].Text, "[2 assistant] I propose extracting")
	assert.NotContains(t, chunks[0].Text, "system instruction")
	assert.NotContains(t, chunks[0].Text, "tool output")
	assert.NotContains(t, chunks[0].Text, "promoted system marker")
}

func TestBuildRecallExtractionChunksBoundsChunksByTextSize(t *testing.T) {
	msgs := []db.Message{
		{Ordinal: 1, Role: "user", Content: strings.Repeat("a", 36)},
		{Ordinal: 2, Role: "assistant", Content: strings.Repeat("b", 36)},
		{Ordinal: 3, Role: "user", Content: strings.Repeat("c", 36)},
		{Ordinal: 4, Role: "assistant", Content: strings.Repeat("d", 36)},
	}

	chunks := BuildRecallExtractionChunks(
		"session-2", msgs, RecallExtractionChunkOptions{MaxChars: 90},
	)

	require.Len(t, chunks, 4)
	for i, chunk := range chunks {
		assert.Equal(t, i, chunk.Index)
		assert.Len(t, chunk.Messages, 1)
		assert.LessOrEqual(t, len(chunk.Text), 90)
		assert.Equal(t, chunk.Messages[0].Ordinal, chunk.StartOrdinal)
		assert.Equal(t, chunk.Messages[0].Ordinal, chunk.EndOrdinal)
	}
}
