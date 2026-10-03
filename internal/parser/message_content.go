package parser

import (
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/stringutil"
)

// ContentLayout identifies native body types without interpreting text markers.
// Ranges are UTF-8 byte offsets into the field named by Kind; tool_call blocks
// instead identify the corresponding ToolCalls entry. Nil means legacy content.
type ContentLayout struct {
	Version int            `json:"version"`
	Blocks  []ContentBlock `json:"blocks"`
}

type ContentBlock struct {
	Kind      string `json:"kind"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	CallIndex int    `json:"call_index"`
	Category  string `json:"category,omitempty"`
}

// MessageContentBuilder keeps searchable dialogue separate from native work
// while preserving block order and the historical rendered work length.
type MessageContentBuilder struct {
	message    ParsedMessage
	workLength int
	workParts  int
}

func (b *MessageContentBuilder) addBlock(block ContentBlock) {
	if b.message.ContentLayout == nil {
		b.message.ContentLayout = &ContentLayout{Version: 1, Blocks: []ContentBlock{}}
	}
	b.message.ContentLayout.Blocks = append(b.message.ContentLayout.Blocks, block)
}

func (b *MessageContentBuilder) addWork(length int) {
	if b.workParts > 0 {
		b.workLength++ // historical newline between rendered blocks
	}
	b.workParts++
	b.workLength += length
}

func (b *MessageContentBuilder) appendBody(kind, text, separator string, body *string) {
	if *body != "" && text != "" {
		*body += separator
	}
	start := len(*body)
	*body += text
	b.addBlock(ContentBlock{
		Kind: kind, Start: start, End: len(*body),
	})
}

func (b *MessageContentBuilder) AddText(text string) {
	b.addText(text, "\n")
}

func (b *MessageContentBuilder) addText(text, separator string) {
	if text == "" {
		return
	}
	text = stringutil.SanitizeUTF8(text)
	b.addWork(len(text))
	b.appendBody("text", text, separator, &b.message.Content)
}

func (b *MessageContentBuilder) AddThinking(text string) {
	b.addThinking(text, "\n\n")
}

func (b *MessageContentBuilder) addThinking(text, separator string) {
	b.message.HasThinking = true
	if text != "" {
		b.addWork(sanitizedWorkLength(text) + len("[Thinking]\n\n[/Thinking]"))
	}
	b.appendBody("thinking", stringutil.SanitizeUTF8(text), separator, &b.message.ThinkingText)
}

func (b *MessageContentBuilder) AddToolCall(call ParsedToolCall) {
	b.message.HasToolUse = true
	b.addBlock(ContentBlock{Kind: "tool_call", CallIndex: len(b.message.ToolCalls)})
	rendered := call.Rendering != ""
	call.Rendering = stringutil.SanitizeUTF8(call.Rendering)
	b.message.ToolCalls = append(b.message.ToolCalls, call)
	if rendered {
		b.addWork(len(call.Rendering))
	}
}

func (b *MessageContentBuilder) AddToolResult(result ParsedToolResult) {
	b.addToolResult(result, DecodeContent(result.ContentRaw))
}

// addToolResult retains raw metadata when a provider has its own text decoder.
func (b *MessageContentBuilder) addToolResult(result ParsedToolResult, text string) {
	b.message.ToolResults = append(b.message.ToolResults, result)
	b.appendBody("tool_result", stringutil.SanitizeUTF8(text), "\n", &b.message.ToolResultText)
	if result.ToolName != "" {
		blocks := b.message.ContentLayout.Blocks
		blocks[len(blocks)-1].Category = NormalizeToolCategory(result.ToolName)
	}
}

func (b *MessageContentBuilder) Message() ParsedMessage {
	if b.message.ContentLayout == nil {
		b.message.ContentLayout = &ContentLayout{Version: 1, Blocks: []ContentBlock{}}
	}
	b.message.ContentLength = b.workLength
	return b.message
}

// continueMessageContent appends native blocks to an already extracted body.
func continueMessageContent(message ParsedMessage) *MessageContentBuilder {
	b := &MessageContentBuilder{message: message, workLength: message.ContentLength}
	if message.ContentLength > 0 {
		b.workParts = 1
	}
	if message.ContentLayout != nil {
		layout := *message.ContentLayout
		layout.Blocks = slices.Clone(layout.Blocks)
		b.message.ContentLayout = &layout
	}
	b.message.ToolCalls = slices.Clone(message.ToolCalls)
	b.message.ToolResults = slices.Clone(message.ToolResults)
	return b
}

func (m ParsedMessage) hasNativeBody() bool {
	return strings.TrimSpace(m.Content) != "" || m.HasThinking || m.HasToolUse ||
		len(m.ToolResults) > 0 || len(m.ToolCalls) > 0 ||
		m.ToolResultText != "" || (m.Content == "" && m.ContentLength > 0)
}

func (m ParsedMessage) withBody(body ParsedMessage) ParsedMessage {
	body.setDialogue(m.Content)
	m.Content = body.Content
	m.ThinkingText = body.ThinkingText
	m.ToolResultText = body.ToolResultText
	m.ContentLayout = body.ContentLayout
	m.ContentLength = body.ContentLength
	m.HasThinking = body.HasThinking
	m.HasToolUse = body.HasToolUse
	m.ToolCalls = body.ToolCalls
	m.ToolResults = body.ToolResults
	return m
}

// withPlainBody is for reader fields whose native type is dialogue.
func (m ParsedMessage) withPlainBody() ParsedMessage {
	work := m.ContentLength
	removed := len(m.Content) - sanitizedWorkLength(m.Content)
	var body MessageContentBuilder
	body.AddText(m.Content)
	m.Content = body.Message().Content
	m = m.withBody(body.Message())
	m.ContentLength = max(0, work-removed)
	return m
}

// sanitizedWorkLength keeps provider-specific rendered work conventions while
// excluding bytes that cannot survive in the saved transcript.
func sanitizedWorkLength(text string) int {
	return len(stringutil.SanitizeUTF8(text))
}

// sanitizedHistoricalWorkLength applies the archive's byte-removal contract
// before native composition replaces the original fields. Reasoning bytes are
// deducted only from work beyond dialogue, preserving inline legacy counts.
func (m ParsedMessage) sanitizedHistoricalWorkLength() int {
	contentLength := sanitizedWorkLength(m.Content)
	work := max(0, m.ContentLength-(len(m.Content)-contentLength))
	removedThinking := len(m.ThinkingText) - sanitizedWorkLength(m.ThinkingText)
	work -= min(max(0, work-contentLength), removedThinking)
	if m.Content == "" && m.ThinkingText == "" {
		removedOutput := len(m.ToolResultText) - sanitizedWorkLength(m.ToolResultText)
		if m.ToolResultText == "" {
			for _, result := range m.ToolResults {
				raw := DecodeContent(result.ContentRaw)
				removedOutput += len(raw) - sanitizedWorkLength(raw)
			}
		}
		work = max(0, work-removedOutput)
	}
	return work
}

// setDialogue retains native non-text blocks when a command or IDE envelope
// replaces dialogue. Whitespace trimming preserves each original text span.
func (m *ParsedMessage) setDialogue(text string) {
	text = stringutil.SanitizeUTF8(text)
	if text == m.Content {
		return
	}
	old := m.Content
	m.Content = text
	m.ContentLength = max(len(text), m.ContentLength+len(text)-len(old))
	if m.ContentLayout == nil {
		return
	}
	layout := *m.ContentLayout
	layout.Blocks = slices.Clone(layout.Blocks)
	if strings.TrimSpace(old) == strings.TrimSpace(text) {
		start := strings.Index(old, text)
		start = max(start, 0)
		for i := range layout.Blocks {
			block := &layout.Blocks[i]
			if block.Kind == "text" {
				block.Start = min(len(text), max(0, block.Start-start))
				block.End = min(len(text), max(0, block.End-start))
			}
		}
	} else {
		blocks := layout.Blocks[:0]
		seen := false
		for _, block := range layout.Blocks {
			if block.Kind == "text" {
				if seen {
					continue
				}
				seen = true
				block.Start, block.End = 0, len(text)
			}
			blocks = append(blocks, block)
		}
		if !seen && text != "" {
			blocks = append(blocks, ContentBlock{Kind: "text", End: len(text)})
		}
		layout.Blocks = blocks
	}
	m.ContentLayout = &layout
}

// removeDialogueRanges applies sorted, disjoint byte ranges to native dialogue.
// Every text span keeps its position relative to thinking and calls.
func (m *ParsedMessage) removeDialogueRanges(ranges [][]int) {
	if len(ranges) == 0 {
		return
	}
	old := m.Content
	var out strings.Builder
	out.Grow(len(old))
	cursor := 0
	for _, span := range ranges {
		out.WriteString(old[cursor:span[0]])
		cursor = span[1]
	}
	out.WriteString(old[cursor:])
	m.Content = out.String()
	if m.ContentLayout == nil {
		return
	}
	remap := func(offset int) int {
		removed := 0
		for _, span := range ranges {
			if offset <= span[0] {
				break
			}
			removed += min(offset, span[1]) - span[0]
			if offset <= span[1] {
				break
			}
		}
		return offset - removed
	}
	layout := *m.ContentLayout
	layout.Blocks = slices.Clone(layout.Blocks)
	for i := range layout.Blocks {
		block := &layout.Blocks[i]
		if block.Kind == "text" {
			block.Start = remap(block.Start)
			block.End = remap(block.End)
		}
	}
	m.ContentLayout = &layout
}

// trimThinking preserves native span order while applying a reader's existing
// whitespace convention to the concatenated reasoning field.
func (m *ParsedMessage) trimThinking() {
	old := m.ThinkingText
	text := strings.TrimSpace(old)
	if text == old {
		return
	}
	m.ThinkingText = text
	if m.ContentLayout == nil {
		return
	}
	start := strings.Index(old, text)
	start = max(start, 0)
	layout := *m.ContentLayout
	layout.Blocks = slices.Clone(layout.Blocks)
	for i := range layout.Blocks {
		block := &layout.Blocks[i]
		if block.Kind == "thinking" {
			block.Start = min(len(text), max(0, block.Start-start))
			block.End = min(len(text), max(0, block.End-start))
		}
	}
	m.ContentLayout = &layout
}

// trimDialogue trims only native text at the edges of the original rendered
// body. Reasoning or tools before dialogue can protect Markdown indentation.
func (m *ParsedMessage) trimDialogue() {
	if m.ContentLayout == nil {
		return
	}
	first, last := -1, -1
	for i, block := range m.ContentLayout.Blocks {
		rendered := (block.Kind == "text" || block.Kind == "thinking") && block.End > block.Start
		if block.Kind == "tool_call" && block.CallIndex >= 0 && block.CallIndex < len(m.ToolCalls) {
			rendered = m.ToolCalls[block.CallIndex].Rendering != ""
		}
		if rendered {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return
	}
	text := m.Content
	if m.ContentLayout.Blocks[first].Kind == "text" {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
	}
	if block := m.ContentLayout.Blocks[last]; block.Kind == "text" {
		text = strings.TrimRightFunc(text, unicode.IsSpace)
	} else if block.Kind == "tool_call" {
		rendering := m.ToolCalls[block.CallIndex].Rendering
		m.ContentLength -= len(rendering) - len(strings.TrimRightFunc(rendering, unicode.IsSpace))
	}
	m.setDialogue(text)
}
