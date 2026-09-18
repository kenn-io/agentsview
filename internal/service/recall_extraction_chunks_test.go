package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestBuildRecallExtractionChunksIncludesOnlyUserAssistantText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Len(chunks, 1)
	assert.Equal("session-1", chunks[0].SessionID)
	assert.Equal(0, chunks[0].Index)
	assert.Equal(1, chunks[0].StartOrdinal)
	assert.Equal(2, chunks[0].EndOrdinal)
	require.Len(chunks[0].Messages, 2)
	assert.Equal("user", chunks[0].Messages[0].Role)
	assert.Equal(1, chunks[0].Messages[0].Ordinal)
	assert.Equal("assistant", chunks[0].Messages[1].Role)
	assert.Equal(2, chunks[0].Messages[1].Ordinal)
	assert.Contains(chunks[0].Text, "[1 user] We need a project recall")
	assert.Contains(chunks[0].Text, "[2 assistant] I propose extracting")
	assert.NotContains(chunks[0].Text, "system instruction")
	assert.NotContains(chunks[0].Text, "tool output")
	assert.NotContains(chunks[0].Text, "promoted system marker")
}

func TestBuildRecallExtractionChunksBoundsChunksByTextSize(t *testing.T) {
	assert := assert.New(t)

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
		assert.Equal(i, chunk.Index)
		assert.Len(chunk.Messages, 1)
		assert.LessOrEqual(len(chunk.Text), 90)
		assert.Equal(chunk.Messages[0].Ordinal, chunk.StartOrdinal)
		assert.Equal(chunk.Messages[0].Ordinal, chunk.EndOrdinal)
	}
}
