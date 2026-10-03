package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	dec := jsontext.NewDecoder(r)

	tok, err := dec.ReadToken()
	if err != nil {
		return fmt.Errorf("reading opening token: %w", err)
	}
	if tok.Kind() != jsontext.KindBeginArray {
		return fmt.Errorf("expected JSON array, got %v", tok)
	}

	for dec.PeekKind() != jsontext.KindEndArray {
		var conv claudeAIConversation
		if err := json.UnmarshalDecode(dec, &conv); err != nil {
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
	_, err = dec.ReadToken()
	return err
}

// assembleClaudeAIBody uses export block types; literal marker text stays prose.
// Attachments and the legacy text fallback remain dialogue.
func assembleClaudeAIBody(m claudeAIMessage) ParsedMessage {
	var b MessageContentBuilder
	workLength, workParts := 0, 0
	addWork := func(length int) {
		if workParts > 0 {
			workLength += 2
		}
		workParts++
		workLength += length
	}
	addText := func(text string) {
		if text != "" {
			b.addText(text, "\n\n")
			addWork(sanitizedWorkLength(text))
		}
	}
	for _, block := range m.Content {
		switch block.Type {
		case "text":
			addText(block.Text)
		case "thinking":
			b.AddThinking(block.Thinking)
			if block.Thinking != "" {
				addWork(sanitizedWorkLength(block.Thinking) + len("[Thinking]\n\n[/Thinking]"))
			}
		}
	}
	if workParts == 0 {
		addText(m.Text)
	}
	for _, attachment := range buildClaudeAttachmentText(m.Attachments) {
		addText(attachment)
	}
	body := b.Message()
	body.ContentLength = workLength
	return body
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
		body := assembleClaudeAIBody(m)

		role := RoleAssistant
		if m.Sender == "human" {
			role = RoleUser
			userCount++
			if firstUserMessage == "" {
				firstUserMessage = body.Content
			}
		}

		ts, _ := time.Parse(time.RFC3339Nano, m.CreatedAt)

		body.Ordinal = i
		body.Role = role
		body.Timestamp = ts
		body.SourceUUID = m.UUID
		msgs = append(msgs, body)
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
