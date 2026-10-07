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
	Index       *int                 `json:"index"`
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
	if slices.ContainsFunc(conv.Messages, func(m claudeAIMessage) bool { return len(m.Parent) > 0 && string(m.Parent) != "null" }) {
		messages, err := selectedClaudeAIPath(*conv)
		if err != nil {
			return ParseResult{}, err
		}
		conv.Messages = messages
	}
	result, err := convertClaudeAIConversation(*conv)
	if err != nil {
		return ParseResult{}, err
	}
	if len(conv.CurrentLeaf) > 0 && string(conv.CurrentLeaf) != "null" {
		var leaf string
		if err := json.Unmarshal(conv.CurrentLeaf, &leaf); err != nil {
			return ParseResult{}, err
		}
		result.Session.LastEntryUUID = &leaf
	}
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

// selectedClaudeAIPath follows Claude's fo/uo/co/vo and restores the prompt chosen by po.
func selectedClaudeAIPath(conv claudeAIConversation) ([]claudeAIMessage, error) {
	const root = "00000000-0000-4000-8000-000000000000"
	byID := make(map[string]claudeAIMessage)
	parents := make(map[string]string)
	kept := make(map[string]bool)
	for _, m := range conv.Messages {
		if _, exists := byID[m.UUID]; exists {
			return nil, fmt.Errorf("duplicate message uuid %s", m.UUID)
		}
		byID[m.UUID] = m
		if len(m.Parent) > 0 && string(m.Parent) != "null" {
			var parentsValue string
			if err := json.Unmarshal(m.Parent, &parentsValue); err != nil {
				return nil, err
			}
			parents[m.UUID] = parentsValue
		}
		if m.Sender == "assistant" && parents[m.UUID] != "" {
			kept[parents[m.UUID]] = true
		}
		if m.Sender == "assistant" || len(m.Parent) == 0 {
			kept[m.UUID] = true
		}
	}
	for id := range kept {
		for parent := parents[id]; parent != "" && !kept[parent]; parent = parents[parent] {
			kept[parent] = true
		}
	}
	var messages, dangling []claudeAIMessage
	byIndex := make(map[int]string)
	for _, m := range conv.Messages {
		if kept[m.UUID] {
			messages = append(messages, m)
			if m.Index != nil {
				byIndex[*m.Index] = m.UUID
			}
		} else if m.Sender == "human" {
			dangling = append(dangling, m)
		}
	}
	selected := make(map[string]string)
	for _, m := range messages {
		parent := parents[m.UUID]
		if parent == "" && m.Index != nil {
			for i := *m.Index - 1; parent == "" && i >= 0; i-- {
				parent = byIndex[i]
			}
		}
		if parent == "" {
			parent = root
		}
		if _, exists := byID[parent]; parent != root && (!exists || !kept[parent]) {
			return nil, fmt.Errorf("message %s's parent %s is missing", m.UUID, parent)
		}
		parents[m.UUID] = parent
		if selected[parent] == "" {
			selected[parent] = m.UUID
		}
	}
	if len(messages) > 0 && selected[root] == "" {
		return nil, fmt.Errorf("no root message found")
	}
	var leaf string
	if len(conv.CurrentLeaf) > 0 && string(conv.CurrentLeaf) != "null" {
		if err := json.Unmarshal(conv.CurrentLeaf, &leaf); err != nil {
			return nil, err
		}
	}
	serverLeaf := leaf
	if leaf != "" && !slices.ContainsFunc(messages, func(m claudeAIMessage) bool { return m.UUID == leaf }) {
		leaf = ""
		if len(messages) > 0 {
			leaf = messages[len(messages)-1].UUID
		}
	}
	seen := make(map[string]bool)
	for id := leaf; id != "" && id != root; id = parents[id] {
		if seen[id] {
			return nil, fmt.Errorf("cycle at message %s", id)
		}
		seen[id] = true
		selected[parents[id]] = id
	}
	var path []claudeAIMessage
	clear(seen)
	for id := selected[root]; id != ""; id = selected[id] {
		if seen[id] {
			return nil, fmt.Errorf("cycle at message %s", id)
		}
		seen[id] = true
		path = append(path, byID[id])
	}
	last := root
	if len(path) > 0 {
		last = path[len(path)-1].UUID
	}
	var prompt *claudeAIMessage
	for i := range dangling {
		m := &dangling[i]
		if serverLeaf != "" && m.UUID == serverLeaf {
			prompt = m
			break
		}
		parent := parents[m.UUID]
		if len(m.Parent) == 0 || string(m.Parent) == "null" {
			parent = root
		}
		if parent == last {
			prompt = m
		}
	}
	if prompt != nil {
		path = append(path, *prompt)
	}
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
