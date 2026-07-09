package service

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const defaultRecallExtractionChunkMaxChars = 12000

type RecallExtractionChunkOptions struct {
	MaxChars int
}

type RecallExtractionChunk struct {
	SessionID    string                         `json:"session_id"`
	Index        int                            `json:"index"`
	StartOrdinal int                            `json:"start_ordinal"`
	EndOrdinal   int                            `json:"end_ordinal"`
	CharCount    int                            `json:"char_count"`
	Messages     []RecallExtractionChunkMessage `json:"messages"`
	Text         string                         `json:"text"`
}

type RecallExtractionChunkMessage struct {
	Ordinal int    `json:"ordinal"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

func BuildRecallExtractionChunks(
	sessionID string,
	messages []db.Message,
	opts RecallExtractionChunkOptions,
) []RecallExtractionChunk {
	maxChars := opts.MaxChars
	if maxChars <= 0 {
		maxChars = defaultRecallExtractionChunkMaxChars
	}

	var chunks []RecallExtractionChunk
	var current []RecallExtractionChunkMessage
	currentLen := 0
	for _, msg := range messages {
		chunkMsg, ok := recallExtractionChunkMessage(msg)
		if !ok {
			continue
		}
		rendered := renderRecallExtractionChunkMessage(chunkMsg)
		addedLen := len(rendered)
		if len(current) > 0 {
			addedLen++
		}
		if len(current) > 0 && currentLen+addedLen > maxChars {
			chunks = append(chunks, newRecallExtractionChunk(sessionID, len(chunks), current))
			current = nil
			currentLen = 0
			addedLen = len(rendered)
		}
		current = append(current, chunkMsg)
		currentLen += addedLen
	}
	if len(current) > 0 {
		chunks = append(chunks, newRecallExtractionChunk(sessionID, len(chunks), current))
	}
	return chunks
}

func recallExtractionChunkMessage(msg db.Message) (RecallExtractionChunkMessage, bool) {
	role := strings.TrimSpace(msg.Role)
	if msg.IsSystem || (role != "user" && role != "assistant") {
		return RecallExtractionChunkMessage{}, false
	}
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return RecallExtractionChunkMessage{}, false
	}
	return RecallExtractionChunkMessage{
		Ordinal: msg.Ordinal,
		Role:    role,
		Content: content,
	}, true
}

func newRecallExtractionChunk(
	sessionID string,
	index int,
	messages []RecallExtractionChunkMessage,
) RecallExtractionChunk {
	copied := append([]RecallExtractionChunkMessage(nil), messages...)
	text := renderRecallExtractionChunkMessages(copied)
	return RecallExtractionChunk{
		SessionID:    sessionID,
		Index:        index,
		StartOrdinal: copied[0].Ordinal,
		EndOrdinal:   copied[len(copied)-1].Ordinal,
		CharCount:    len(text),
		Messages:     copied,
		Text:         text,
	}
}

func renderRecallExtractionChunkMessages(messages []RecallExtractionChunkMessage) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		parts = append(parts, renderRecallExtractionChunkMessage(msg))
	}
	return strings.Join(parts, "\n")
}

func renderRecallExtractionChunkMessage(msg RecallExtractionChunkMessage) string {
	return fmt.Sprintf("[%d %s] %s", msg.Ordinal, msg.Role, msg.Content)
}
