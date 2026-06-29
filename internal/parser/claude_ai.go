package parser

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type claudeAIConversation struct {
	UUID      string            `json:"uuid"`
	Name      string            `json:"name"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Messages  []claudeAIMessage `json:"chat_messages"`
}

type claudeAIMessage struct {
	UUID        string               `json:"uuid"`
	Text        string               `json:"text"`
	Content     []claudeAIBlock      `json:"content"`
	Sender      string               `json:"sender"`
	CreatedAt   string               `json:"created_at"`
	Attachments []claudeAIAttachment `json:"attachments"`
}

// claudeAIBlock represents a content block within a message.
// Block types: text, thinking, tool_use, tool_result,
// voice_note, token_budget.
type claudeAIBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

// ClaudeAIExportParser is implemented by the Claude.ai import-only provider to
// stream a Claude.ai conversations export. Claude.ai sessions are never
// discovered or synced from disk; they only enter the archive through a
// one-shot import, so this entry point lives on the provider rather than the
// Discover/Parse path. Callers obtain it via NewProvider(AgentClaudeAI, ...)
// and a type assertion.
type ClaudeAIExportParser interface {
	// ParseClaudeAIExport streams a Claude.ai conversations.json export and
	// calls onConversation for each non-empty conversation.
	ParseClaudeAIExport(
		r io.Reader,
		onConversation func(ParseResult) error,
	) error
}

// claudeAIAttachment holds content emitted by Claude attachments.
type claudeAIAttachment struct {
	FileName         string `json:"file_name"`
	ExtractedContent string `json:"extracted_content"`
}

// ParseClaudeAIExport streams a Claude.ai conversations.json
// export and calls onConversation for each non-empty
// conversation.
func (p *claudeAIImportOnlyProvider) ParseClaudeAIExport(
	r io.Reader,
	onConversation func(ParseResult) error,
) error {
	dec := json.NewDecoder(r)

	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("reading opening token: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("expected JSON array, got %v", tok)
	}

	for dec.More() {
		var conv claudeAIConversation
		if err := dec.Decode(&conv); err != nil {
			return fmt.Errorf("decoding conversation: %w", err)
		}

		if len(conv.Messages) == 0 {
			continue
		}

		result, err := convertClaudeAIConversation(conv)
		if err != nil {
			return fmt.Errorf(
				"converting conversation %s: %w",
				conv.UUID, err,
			)
		}

		if err := onConversation(result); err != nil {
			return err
		}
	}

	return nil
}

// assembleClaudeAIContent builds message content from content
// blocks. Falls back to the top-level text field when no
// content blocks have usable text.
func assembleClaudeAIContent(
	m claudeAIMessage,
) (content string, hasThinking bool) {
	attachmentParts := buildClaudeAttachmentText(m.Attachments)

	if len(m.Content) == 0 {
		if len(attachmentParts) == 0 {
			return m.Text, false
		}

		contentParts := make([]string, 0, 1+len(attachmentParts))
		if m.Text != "" {
			contentParts = append(contentParts, m.Text)
		}
		contentParts = append(contentParts, attachmentParts...)
		return strings.Join(contentParts, "\n\n"), false
	}

	var contentParts []string
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				contentParts = append(contentParts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				hasThinking = true
				contentParts = append(contentParts,
					"[Thinking]\n"+b.Thinking+"\n[/Thinking]")
			}
			// tool_use, tool_result, voice_note, token_budget
			// are metadata blocks — skip for display content.
		}
	}

	if len(contentParts) == 0 {
		if len(attachmentParts) == 0 {
			return m.Text, hasThinking
		}
		if m.Text != "" {
			contentParts = append(contentParts, m.Text)
		}
	}

	contentParts = append(contentParts, attachmentParts...)

	return strings.Join(contentParts, "\n\n"), hasThinking
}

func buildClaudeAttachmentText(
	attachments []claudeAIAttachment,
) []string {
	parts := make([]string, 0, len(attachments))
	for _, a := range attachments {
		if strings.TrimSpace(a.ExtractedContent) == "" {
			continue
		}
		if a.FileName == "" {
			parts = append(parts, a.ExtractedContent)
			continue
		}
		parts = append(parts, "[Attachment: "+a.FileName+"]\n"+a.ExtractedContent)
	}
	return parts
}

func convertClaudeAIConversation(
	conv claudeAIConversation,
) (ParseResult, error) {
	startedAt, err := time.Parse(time.RFC3339Nano, conv.CreatedAt)
	if err != nil {
		return ParseResult{},
			fmt.Errorf("parsing created_at: %w", err)
	}

	endedAt, err := time.Parse(time.RFC3339Nano, conv.UpdatedAt)
	if err != nil {
		return ParseResult{},
			fmt.Errorf("parsing updated_at: %w", err)
	}

	var (
		msgs             []ParsedMessage
		userCount        int
		firstUserMessage string
	)

	for i, m := range conv.Messages {
		content, hasThinking := assembleClaudeAIContent(m)

		role := RoleAssistant
		if m.Sender == "human" {
			role = RoleUser
			userCount++
			if firstUserMessage == "" {
				firstUserMessage = content
			}
		}

		ts, _ := time.Parse(time.RFC3339Nano, m.CreatedAt)

		msgs = append(msgs, ParsedMessage{
			Ordinal:       i,
			Role:          role,
			Content:       content,
			Timestamp:     ts,
			HasThinking:   hasThinking,
			ContentLength: len(content),
		})
	}

	return ParseResult{
		Session: ParsedSession{
			ID:               "claude-ai:" + conv.UUID,
			Project:          "claude.ai",
			Machine:          "local",
			Agent:            AgentClaudeAI,
			FirstMessage:     firstUserMessage,
			SessionName:      conv.Name,
			StartedAt:        startedAt,
			EndedAt:          endedAt,
			MessageCount:     len(conv.Messages),
			UserMessageCount: userCount,
		},
		Messages: msgs,
	}, nil
}
