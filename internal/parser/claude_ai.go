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
	UUID      string          `json:"uuid"`
	Text      string          `json:"text"`
	Content   []claudeAIBlock `json:"content"`
	Sender    string          `json:"sender"`
	CreatedAt string          `json:"created_at"`
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
	if len(m.Content) == 0 {
		return m.Text, false
	}

	var parts []string
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				hasThinking = true
				parts = append(parts,
					"[Thinking]\n"+b.Thinking+"\n[/Thinking]")
			}
			// tool_use, tool_result, voice_note, token_budget
			// are metadata blocks — skip for display content.
		}
	}

	if len(parts) == 0 {
		return m.Text, hasThinking
	}
	return strings.Join(parts, "\n\n"), hasThinking
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
