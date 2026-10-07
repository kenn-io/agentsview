package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

type claudeAIConversation struct {
	UUID        string            `json:"uuid"`
	Name        string            `json:"name"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Messages    []claudeAIMessage `json:"chat_messages"`
	CurrentLeaf jsontext.Value    `json:"current_leaf_message_uuid"`
}

type claudeAIMessage struct {
	UUID        string               `json:"uuid"`
	Parent      jsontext.Value       `json:"parent_message_uuid"`
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

// ParseClaudeAIDetail selects the visible branch of a browser-fetched chat.
func ParseClaudeAIDetail(data []byte) (ParseResult, error) {
	var conv *claudeAIConversation
	if err := json.Unmarshal(data, &conv); err != nil {
		return ParseResult{}, err
	}
	if conv == nil || conv.UUID == "" || len(conv.Messages) == 0 {
		return ParseResult{}, fmt.Errorf("expected conversation with chat_messages")
	}
	messages, err := selectedClaudeAIPath(*conv)
	if err != nil {
		return ParseResult{}, err
	}
	conv.Messages = messages
	result, err := convertClaudeAIConversation(*conv)
	if err != nil {
		return ParseResult{}, err
	}
	leaf := messages[len(messages)-1].UUID
	result.Session.LastEntryUUID = &leaf
	return result, nil
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

// selectedClaudeAIPath walks the server leaf's ancestors back to the root.
func selectedClaudeAIPath(conv claudeAIConversation) ([]claudeAIMessage, error) {
	const root = "00000000-0000-4000-8000-000000000000"
	var leaf string
	if conv.CurrentLeaf.Kind() != jsontext.KindString || json.Unmarshal(conv.CurrentLeaf, &leaf) != nil || leaf == "" || leaf == root {
		return nil, fmt.Errorf("expected current_leaf_message_uuid string naming a message")
	}
	byID := make(map[string]claudeAIMessage, len(conv.Messages))
	parents := make(map[string]string, len(conv.Messages))
	for _, m := range conv.Messages {
		if _, exists := byID[m.UUID]; exists {
			return nil, fmt.Errorf("duplicate message uuid %s", m.UUID)
		}
		var parent string
		if m.Parent.Kind() != jsontext.KindString || json.Unmarshal(m.Parent, &parent) != nil {
			return nil, fmt.Errorf("message %s's parent must be a string", m.UUID)
		}
		byID[m.UUID], parents[m.UUID] = m, parent
	}
	for id, parent := range parents {
		if _, exists := byID[parent]; parent != root && !exists {
			return nil, fmt.Errorf("message %s's parent %s is missing", id, parent)
		}
	}
	var path []claudeAIMessage
	seen := make(map[string]bool)
	for id := leaf; id != root; id = parents[id] {
		m, exists := byID[id]
		if !exists {
			return nil, fmt.Errorf("message %s is missing", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("cycle at message %s", id)
		}
		seen[id] = true
		path = append(path, m)
	}
	slices.Reverse(path)
	return path, nil
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
