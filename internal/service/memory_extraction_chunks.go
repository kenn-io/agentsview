package service

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const defaultMemoryExtractionChunkMaxChars = 12000

type MemoryExtractionChunkOptions struct {
	MaxChars int
}

type MemoryExtractionChunk struct {
	SessionID    string                         `json:"session_id"`
	Index        int                            `json:"index"`
	StartOrdinal int                            `json:"start_ordinal"`
	EndOrdinal   int                            `json:"end_ordinal"`
	CharCount    int                            `json:"char_count"`
	Messages     []MemoryExtractionChunkMessage `json:"messages"`
	Text         string                         `json:"text"`
}

type MemoryExtractionChunkMessage struct {
	Ordinal int    `json:"ordinal"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

func BuildMemoryExtractionChunks(
	sessionID string,
	messages []db.Message,
	opts MemoryExtractionChunkOptions,
) []MemoryExtractionChunk {
	maxChars := opts.MaxChars
	if maxChars <= 0 {
		maxChars = defaultMemoryExtractionChunkMaxChars
	}

	var chunks []MemoryExtractionChunk
	var current []MemoryExtractionChunkMessage
	currentLen := 0
	for _, msg := range messages {
		chunkMsg, ok := memoryExtractionChunkMessage(msg)
		if !ok {
			continue
		}
		rendered := renderMemoryExtractionChunkMessage(chunkMsg)
		addedLen := len(rendered)
		if len(current) > 0 {
			addedLen++
		}
		if len(current) > 0 && currentLen+addedLen > maxChars {
			chunks = append(chunks, newMemoryExtractionChunk(sessionID, len(chunks), current))
			current = nil
			currentLen = 0
			addedLen = len(rendered)
		}
		current = append(current, chunkMsg)
		currentLen += addedLen
	}
	if len(current) > 0 {
		chunks = append(chunks, newMemoryExtractionChunk(sessionID, len(chunks), current))
	}
	return chunks
}

func memoryExtractionChunkMessage(msg db.Message) (MemoryExtractionChunkMessage, bool) {
	role := strings.TrimSpace(msg.Role)
	if msg.IsSystem || (role != "user" && role != "assistant") {
		return MemoryExtractionChunkMessage{}, false
	}
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return MemoryExtractionChunkMessage{}, false
	}
	return MemoryExtractionChunkMessage{
		Ordinal: msg.Ordinal,
		Role:    role,
		Content: content,
	}, true
}

func newMemoryExtractionChunk(
	sessionID string,
	index int,
	messages []MemoryExtractionChunkMessage,
) MemoryExtractionChunk {
	copied := append([]MemoryExtractionChunkMessage(nil), messages...)
	text := renderMemoryExtractionChunkMessages(copied)
	return MemoryExtractionChunk{
		SessionID:    sessionID,
		Index:        index,
		StartOrdinal: copied[0].Ordinal,
		EndOrdinal:   copied[len(copied)-1].Ordinal,
		CharCount:    len(text),
		Messages:     copied,
		Text:         text,
	}
}

func renderMemoryExtractionChunkMessages(messages []MemoryExtractionChunkMessage) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		parts = append(parts, renderMemoryExtractionChunkMessage(msg))
	}
	return strings.Join(parts, "\n")
}

func renderMemoryExtractionChunkMessage(msg MemoryExtractionChunkMessage) string {
	return fmt.Sprintf("[%d %s] %s", msg.Ordinal, msg.Role, msg.Content)
}
