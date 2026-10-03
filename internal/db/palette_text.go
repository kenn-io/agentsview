package db

import (
	"database/sql"
	"encoding/json"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// ReadVisibleSearchOrdinals filters broad SQL candidates using the fields the
// viewer displays. A serialized input key or image metadata is not a find hit.
func ReadVisibleSearchOrdinals(rows *sql.Rows, query string) ([]int, error) {
	needle := strings.ToLower(query)
	var ordinals []int
	for rows.Next() {
		var ordinal int
		var content, thinking, output, toolName, input, rendering, result, event string
		if err := rows.Scan(&ordinal, &content, &thinking, &output, &toolName, &input, &rendering, &result, &event); err != nil {
			return nil, err
		}
		if rendering == "" {
			rendering = parser.ToolUseRendering(toolName, input)
		}
		for _, text := range []string{content, thinking, DisplayResultText(output), rendering, DisplayResultText(result), DisplayResultText(event)} {
			if strings.Contains(strings.ToLower(text), needle) {
				if len(ordinals) == 0 || ordinals[len(ordinals)-1] != ordinal {
					ordinals = append(ordinals, ordinal)
				}
				break
			}
		}
	}
	return ordinals, rows.Err()
}

// PaletteCorpusRecipe identifies the derived complete-transcript projection.
const PaletteCorpusRecipe = "native-v1"

// PaletteText is the internal complete-transcript corpus. It consumes saved
// display fields rather than serialized tool inputs, whose keys are invisible.
func PaletteText(m Message) string {
	parts := []string{m.Content}
	add := func(text string) {
		if text != "" && (m.ContentLayout != nil || !strings.Contains(m.Content, text)) {
			parts = append(parts, text)
		}
	}
	add(m.ThinkingText)
	add(DisplayResultText(m.ToolResultText))
	for _, call := range m.ToolCalls {
		rendering := call.Rendering
		if rendering == "" {
			rendering = parser.ToolUseRendering(call.ToolName, call.InputJSON)
		}
		add(rendering)
		add(DisplayResultText(DedupToolCallResultSummary(call.ResultContent, call.ResultEvents)))
		for _, event := range call.ResultEvents {
			add(DisplayResultText(event.Content))
		}
	}
	return strings.Join(parts, "\n\n")
}

// DisplayResultText mirrors the retained-image placeholder display projection.
// Unknown shapes retain their original text, as the transcript viewer does.
func DisplayResultText(content string) string {
	if !strings.Contains(content, `"agentsview_image"`) {
		return content
	}
	project := func(raw string) (string, bool) {
		var blocks []struct {
			Type    string  `json:"type"`
			Version int     `json:"version"`
			Text    *string `json:"text"`
		}
		if json.Unmarshal([]byte(raw), &blocks) != nil || blocks == nil {
			return raw, false
		}
		parts := make([]string, 0, len(blocks))
		hasImage := false
		for _, block := range blocks {
			if block.Text == nil {
				return raw, false
			}
			switch block.Type {
			case "agentsview_image":
				if block.Version != 1 {
					return raw, false
				}
				hasImage = true
			case "text", "input_text":
			default:
				return raw, false
			}
			parts = append(parts, *block.Text)
		}
		return strings.Join(parts, "\n\n"), hasImage
	}
	if text, ok := project(content); ok {
		return text
	}
	parts := strings.Split(content, "\n\n")
	for i, part := range parts {
		if text, ok := project(part); ok {
			parts[i] = text
			continue
		}
		label, body, ok := strings.Cut(part, "\n")
		if ok && strings.HasSuffix(strings.TrimRight(label, " \t"), ":") {
			if text, ok := project(body); ok {
				parts[i] = label + "\n" + text
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func sqlitePaletteText(content, thinking, output, layout, callsJSON string) (string, error) {
	var calls []ToolCall
	if err := json.Unmarshal([]byte(callsJSON), &calls); err != nil {
		return "", err
	}
	return PaletteText(Message{Content: content, ThinkingText: thinking, ToolResultText: output, ContentLayout: DecodeStoredContentLayout(layout), ToolCalls: calls}), nil
}
