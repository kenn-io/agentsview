package db

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/parser"
)

// SetContentLayout records whether the writer has native body provenance.
// A nil layout from a parser or archive read must remain legacy when copied.
func (m *Message) SetContentLayout(layout *parser.ContentLayout) {
	m.ContentLayout = layout
	m.legacyBody = layout == nil
}

// UnmarshalJSON preserves unknown provenance in older serialized messages.
// New direct Go writers supply canonical fields or an explicit layout.
func (m *Message) UnmarshalJSON(data []byte) error {
	type messageAlias Message
	var decoded messageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Message(decoded)
	m.legacyBody = m.ContentLayout == nil
	return nil
}

func ContentLayoutJSON(layout *parser.ContentLayout) string {
	if layout == nil {
		return ""
	}
	encoded, _ := json.Marshal(layout)
	return string(encoded)
}

func storedContentLayout(layout *parser.ContentLayout) any {
	if layout == nil {
		return nil
	}
	return ContentLayoutJSON(layout)
}

func DecodeStoredContentLayout(raw string) *parser.ContentLayout {
	if raw == "" {
		return nil
	}
	var layout parser.ContentLayout
	if err := json.Unmarshal([]byte(raw), &layout); err != nil {
		return nil
	}
	return &layout
}

func (m *Message) prepareBodyLayout() {
	if m.ContentLayout != nil || m.legacyBody {
		return
	}
	layout := &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{}}
	if m.HasThinking || m.ThinkingText != "" {
		layout.Blocks = append(layout.Blocks, parser.ContentBlock{Kind: "thinking", End: len(m.ThinkingText)})
	}
	if m.Content != "" {
		layout.Blocks = append(layout.Blocks, parser.ContentBlock{Kind: "text", End: len(m.Content)})
	}
	for index := range m.ToolCalls {
		layout.Blocks = append(layout.Blocks, parser.ContentBlock{Kind: "tool_call", CallIndex: index})
	}
	if m.ToolResultText != "" {
		layout.Blocks = append(layout.Blocks, parser.ContentBlock{Kind: "tool_result", End: len(m.ToolResultText)})
	}
	m.ContentLayout = layout
}

// ValidateContentLayout rejects malformed native provenance before any body
// rewrite. Invalid input remains intact and never becomes a legacy record.
func (m *Message) ValidateContentLayout() error {
	if m.ContentLayout == nil {
		return nil
	}
	if m.ContentLayout.Version != 1 {
		return fmt.Errorf("unsupported content layout version %d", m.ContentLayout.Version)
	}
	ends := map[string]int{}
	calls := map[int]bool{}
	for _, block := range m.ContentLayout.Blocks {
		if block.Kind == "tool_call" {
			if block.CallIndex < 0 || block.CallIndex >= len(m.ToolCalls) || calls[block.CallIndex] {
				return fmt.Errorf("invalid content layout tool call index %d", block.CallIndex)
			}
			calls[block.CallIndex] = true
			continue
		}
		var body string
		switch block.Kind {
		case "text":
			body = m.Content
		case "thinking":
			body = m.ThinkingText
		case "tool_result":
			body = m.ToolResultText
		default:
			return fmt.Errorf("unknown content layout block kind %q", block.Kind)
		}
		if block.Start < ends[block.Kind] || block.End < block.Start || block.End > len(body) {
			return fmt.Errorf("invalid content layout %s range [%d,%d)", block.Kind, block.Start, block.End)
		}
		for _, offset := range []int{block.Start, block.End} {
			if !bodyRuneBoundary(body, offset) {
				return fmt.Errorf("content layout %s offset %d splits a UTF-8 rune", block.Kind, offset)
			}
		}
		ends[block.Kind] = block.End
	}
	return nil
}

func bodyRuneBoundary(body string, offset int) bool {
	if offset == 0 || offset == len(body) || utf8.RuneStart(body[offset]) {
		return true
	}
	// Invalid UTF-8 bytes are individually sanitizable. Reject only a boundary
	// inside a valid multibyte rune, rather than every continuation-shaped byte.
	for start := max(0, offset-3); start < offset; start++ {
		_, size := utf8.DecodeRuneInString(body[start:])
		if size > 1 && start+size > offset {
			return false
		}
	}
	return true
}

// ProjectStandaloneResultCategories clears blocked output in each native
// owner without deleting its source record or its non-payload category hint.
func (m *Message) ProjectStandaloneResultCategories(blocked map[string]bool) error {
	if len(blocked) == 0 || m.ContentLayout == nil {
		return nil
	}
	if err := m.ValidateContentLayout(); err != nil {
		return err
	}
	layout := *m.ContentLayout
	layout.Blocks = slices.Clone(layout.Blocks)
	var output strings.Builder
	cursor, resultIndex := 0, 0
	for i := range layout.Blocks {
		block := &layout.Blocks[i]
		if block.Kind != "tool_result" {
			continue
		}
		output.WriteString(m.ToolResultText[cursor:block.Start])
		start := output.Len()
		if !blocked[block.Category] {
			output.WriteString(m.ToolResultText[block.Start:block.End])
		} else if resultIndex < len(m.ToolResults) {
			m.ToolResults[resultIndex].ContentRaw = `""`
		}
		cursor = block.End
		block.Start, block.End = start, output.Len()
		resultIndex++
	}
	output.WriteString(m.ToolResultText[cursor:])
	m.ToolResultText = output.String()
	m.SetContentLayout(&layout)
	return nil
}

func validateMessageLayouts(messages []Message) error {
	for i := range messages {
		if err := messages[i].ValidateContentLayout(); err != nil {
			return fmt.Errorf("message ordinal %d: %w", messages[i].Ordinal, err)
		}
	}
	return nil
}

// TransformBody rewrites native parts before joining them and rebuilds their
// byte ranges. Unrecognized or malformed layouts return an error unchanged.
// The callback also receives separators and unreferenced field text, keeping
// every stored byte within the same transformation boundary.
func (m *Message) TransformBody(rewrite func(kind, text string) string) error {
	if err := m.ValidateContentLayout(); err != nil {
		return err
	}
	fields := []struct {
		kind string
		text *string
	}{{"text", &m.Content}, {"thinking", &m.ThinkingText}, {"tool_result", &m.ToolResultText}}
	layout := m.ContentLayout
	if layout != nil {
		cloned := *layout
		cloned.Blocks = slices.Clone(layout.Blocks)
		layout = &cloned
	}
	for _, field := range fields {
		if layout == nil {
			*field.text = rewrite(field.kind, *field.text)
			continue
		}
		var output strings.Builder
		cursor := 0
		for index, block := range layout.Blocks {
			if block.Kind != field.kind {
				continue
			}
			output.WriteString(rewrite(field.kind, (*field.text)[cursor:block.Start]))
			start := output.Len()
			output.WriteString(rewrite(field.kind, (*field.text)[block.Start:block.End]))
			layout.Blocks[index].Start, layout.Blocks[index].End = start, output.Len()
			cursor = block.End
		}
		output.WriteString(rewrite(field.kind, (*field.text)[cursor:]))
		*field.text = output.String()
	}
	m.SetContentLayout(layout)
	return nil
}

// ClearBodyFields removes payloads and their native references together.
func (m *Message) ClearBodyFields(kinds ...string) {
	for _, kind := range kinds {
		switch kind {
		case "text":
			m.Content = ""
		case "thinking":
			m.ThinkingText = ""
		case "tool_result":
			m.ToolResultText = ""
		}
	}
	if m.ContentLayout != nil {
		layout := *m.ContentLayout
		layout.Blocks = slices.DeleteFunc(slices.Clone(layout.Blocks), func(block parser.ContentBlock) bool {
			return slices.Contains(kinds, block.Kind)
		})
		m.SetContentLayout(&layout)
	}
}

// TransformBodySources scans each complete canonical field before rewriting
// it. The returned boundary mapper adjusts native ranges, including secrets
// whose detection or payload crosses a part boundary.
func (m *Message) TransformBodySources(rewrite func(kind, text string) (string, func(int) int)) error {
	// Clone and validate provenance using the same rules as part transforms.
	if err := m.TransformBody(func(_, text string) string { return text }); err != nil {
		return err
	}
	for _, field := range []struct {
		kind string
		text *string
	}{{"text", &m.Content}, {"thinking", &m.ThinkingText}, {"tool_result", &m.ToolResultText}} {
		transformed, offset := rewrite(field.kind, *field.text)
		*field.text = transformed
		if m.ContentLayout != nil {
			for i := range m.ContentLayout.Blocks {
				block := &m.ContentLayout.Blocks[i]
				if block.Kind == field.kind {
					block.Start, block.End = offset(block.Start), offset(block.End)
				}
			}
		}
	}
	return nil
}

// RemoveToolResultBlocks consumes matched native results. The parser records
// one result block per transient result, including empty results. Unknown
// provenance stays untouched rather than guessing which bytes belong to it.
func (m *Message) RemoveToolResultBlocks(remove []bool) bool {
	if m.ContentLayout == nil || m.ContentLayout.Version != 1 {
		return false
	}
	count, end := 0, 0
	for _, block := range m.ContentLayout.Blocks {
		if block.Kind != "tool_result" {
			continue
		}
		if block.Start < end || block.End < block.Start || block.End > len(m.ToolResultText) {
			return false
		}
		count++
		end = block.End
	}
	if count != len(remove) {
		return false
	}
	var output strings.Builder
	blocks := make([]parser.ContentBlock, 0, len(m.ContentLayout.Blocks))
	index, cursor := 0, 0
	for _, block := range m.ContentLayout.Blocks {
		if block.Kind != "tool_result" {
			blocks = append(blocks, block)
			continue
		}
		originalEnd := block.End
		if !remove[index] {
			if output.Len() > 0 {
				output.WriteString(m.ToolResultText[cursor:block.Start])
			}
			start := output.Len()
			output.WriteString(m.ToolResultText[block.Start:block.End])
			block.Start, block.End = start, output.Len()
			blocks = append(blocks, block)
		}
		cursor = originalEnd
		index++
	}
	m.ToolResultText = output.String()
	m.SetContentLayout(&parser.ContentLayout{Version: 1, Blocks: blocks})
	return true
}
