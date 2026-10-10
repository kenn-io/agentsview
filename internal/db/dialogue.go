package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/parser"
)

// ContentSearchSources lists every content search source, in default order.
var ContentSearchSources = []string{"messages", "thinking", "tool_input", "tool_result"}

// dialogueSourceDataVersion is the first data version whose rows carry
// derived dialogue and inline-only thinking text.
const dialogueSourceDataVersion = 129

var extraBlankLines = regexp.MustCompile(`\n{3,}`)

// MessageDialogueSQL is the text messages-only search reads: the stored
// dialogue when a message has one, otherwise its content.
func MessageDialogueSQL(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "COALESCE(" + alias + "dialogue_text, " + alias + "content)"
}

// DialogueContainsPredicate matches term in a message's dialogue. It tests
// each column on its own, not MessageDialogueSQL, so PostgreSQL can use the
// trigram indexes on dialogue_text and content.
func (b *QueryBuilder) DialogueContainsPredicate(alias, term string) string {
	if alias != "" {
		alias += "."
	}
	return "(" + b.ContainsPredicate(alias+"dialogue_text", term) +
		" OR (" + alias + "dialogue_text IS NULL AND " +
		b.ContainsPredicate(alias+"content", term) + "))"
}

// DeriveSearchText returns msgs with the fields messages-only search reads.
// The storage projections apply it on every archive write.
func DeriveSearchText(msgs []Message) []Message {
	out := slices.Clone(msgs)
	for i := range out {
		deriveMissingText(&out[i])
	}
	return out
}

// deriveMissingText keeps dialogue derived before a storage policy dropped
// the tool inputs that regenerate renderings.
func deriveMissingText(m *Message) {
	if m.DialogueText == nil || toolInputsRemain(m.ToolCalls) {
		deriveMessageText(m, "")
	}
}

// deriveMessageText sets the fields messages-only search reads. Reasoning
// that a parser kept only inline in content becomes ThinkingText, and
// DialogueText is the content without its thinking blocks and tool-call
// renderings, or nil when nothing was removed so search reads content.
// Thinking blocks are removed only when ThinkingText holds them, so
// reasoning stays searchable under the thinking source.
func deriveMessageText(m *Message, agent string) {
	if m.ThinkingText == "" && m.HasThinking {
		m.ThinkingText = friction.InlineThinking(m.Content)
	}
	text := m.Content
	if m.ThinkingText != "" {
		text = friction.StripThinking(text, m.ThinkingText)
	}
	var calls []friction.RawToolCall
	for _, call := range m.ToolCalls {
		// Archives that dropped tool inputs keep the target path, which is
		// all a shortened rendering shows.
		if call.InputJSON == "" && call.FilePath != "" {
			b, _ := json.Marshal(map[string]string{
				"file_path": call.FilePath, "path": call.FilePath,
			})
			call.InputJSON = string(b)
		}
		// The parse-time rendering is exact, in full or as a storage policy
		// shortened it; rows read back from storage only have the input to
		// regenerate it from.
		if cut, ok := cutRendering(text, call); ok {
			text = cut
			continue
		}
		calls = append(calls, friction.RawToolCall{
			ToolName: call.ToolName, Category: call.Category,
			InputJSON: call.InputJSON,
		})
	}
	text = friction.StripToolRenderings(agent, text, calls, false)
	m.DialogueText = nil
	if text != m.Content {
		text = strings.TrimSpace(extraBlankLines.ReplaceAllString(text, "\n\n"))
		m.DialogueText = &text
	}
}

func cutRendering(text string, call ToolCall) (string, bool) {
	if call.Rendering == "" {
		return text, false
	}
	if before, after, ok := strings.Cut(text, call.Rendering); ok {
		return before + after, true
	}
	r := parser.RedactToolUseRendering(
		call.Rendering, call.Category, call.ToolName, call.InputJSON,
	)
	if before, after, ok := strings.Cut(text, r); ok && r != "" {
		return before + after, true
	}
	return text, false
}

const copiedDialoguePage = 500

// fillCopiedDialogueTx derives search text for rows copied by SQL from
// another archive, which skip the write-time storage projection. It fills
// only rows without a stored dialogue.
func fillCopiedDialogueTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
) error {
	for after := int64(0); ; {
		page, err := copiedDialoguePageTx(ctx, tx, tempIDsTable, after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			after = row.id
			thinking := row.msg.ThinkingText
			// Claude.ai imports before data version 129 left has_thinking unset.
			if row.agent == string(parser.AgentClaudeAI) && !row.msg.HasThinking {
				row.msg.HasThinking = friction.InlineThinking(row.msg.Content) != ""
			}
			deriveMessageText(&row.msg, row.agent)
			if row.msg.DialogueText == nil && row.msg.ThinkingText == thinking {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE messages SET thinking_text = ?, dialogue_text = ?, has_thinking = ? WHERE id = ?",
				row.msg.ThinkingText, row.msg.DialogueText, row.msg.HasThinking, row.id,
			); err != nil {
				return fmt.Errorf("filling copied dialogue %d: %w", row.id, err)
			}
		}
	}
}

type copiedDialogueRow struct {
	id    int64
	agent string
	msg   Message
}

func copiedDialoguePageTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string, after int64,
) ([]copiedDialogueRow, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH page AS (
			SELECT id FROM messages
			WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)
			  AND dialogue_text IS NULL
			  AND (has_thinking = 1 OR has_tool_use = 1 OR (
				session_id IN (SELECT id FROM sessions WHERE agent = 'claude-ai')
				AND instr(content, '[Thinking]') > 0))
			  AND id > ?
			ORDER BY id LIMIT ?
		)
		SELECT m.id, s.agent, m.content, m.thinking_text, m.has_thinking,
			tc.tool_name, tc.category, COALESCE(tc.input_json, ''),
			COALESCE(tc.file_path, '')
		FROM page p
		JOIN messages m ON m.id = p.id
		JOIN sessions s ON s.id = m.session_id
		LEFT JOIN tool_calls tc ON tc.message_id = m.id
		ORDER BY m.id, tc.id`, after, copiedDialoguePage)
	if err != nil {
		return nil, fmt.Errorf("querying copied messages: %w", err)
	}
	defer rows.Close()
	page := make([]copiedDialogueRow, 0, copiedDialoguePage)
	for rows.Next() {
		var row copiedDialogueRow
		var name, category, input, path sql.NullString
		if err := rows.Scan(
			&row.id, &row.agent, &row.msg.Content, &row.msg.ThinkingText,
			&row.msg.HasThinking, &name, &category, &input, &path,
		); err != nil {
			return nil, fmt.Errorf("scanning copied message: %w", err)
		}
		if n := len(page); n == 0 || page[n-1].id != row.id {
			page = append(page, row)
		}
		if name.Valid {
			last := &page[len(page)-1].msg
			last.ToolCalls = append(last.ToolCalls, ToolCall{
				ToolName: name.String, Category: category.String,
				InputJSON: input.String, FilePath: path.String,
			})
		}
	}
	return page, rows.Err()
}
