package friction

import (
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	thinkingOpen  = "[Thinking]\n"
	thinkingClose = "\n[/Thinking]"
)

// AssistantText extracts text blocks from stored assistant content, which
// also contains inlined tool renderings and thinking blocks.
func AssistantText(
	content, thinking string, calls []RawToolCall, redacted bool,
) string {
	text := content
	for _, call := range calls {
		if rendering := renderingInText(text, call, redacted); rendering != "" {
			text = removePart(text, strings.Index(text, rendering), len(rendering))
		}
	}
	return stripThinking(text, thinking)
}

// renderingInText chooses the longest present rendering in the preferred
// form, falling back to the other form only when none is present.
func renderingInText(text string, call RawToolCall, redacted bool) string {
	pairs := parser.ToolUseRenderingCandidates(
		call.Category, call.ToolName, call.InputJSON,
	)
	pick := func(form func(parser.ToolUseRenderingPair) string) string {
		best := ""
		for _, pair := range pairs {
			rendering := form(pair)
			if len(rendering) > len(best) && strings.Contains(text, rendering) {
				best = rendering
			}
		}
		return best
	}
	full := func(pair parser.ToolUseRenderingPair) string { return pair.Full }
	red := func(pair parser.ToolUseRenderingPair) string { return pair.Redacted }
	if redacted {
		if rendering := pick(red); rendering != "" {
			return rendering
		}
		return pick(full)
	}
	if rendering := pick(full); rendering != "" {
		return rendering
	}
	return pick(red)
}

// removePart deletes a block and one adjacent newline separator. The
// preceding separator takes priority when both sides have one.
func removePart(text string, at, n int) string {
	if at < 0 {
		return text
	}
	end := at + n
	switch {
	case at > 0 && text[at-1] == '\n':
		at--
	case end < len(text) && text[end] == '\n':
		end++
	}
	return text[:at] + text[end:]
}

// stripThinking removes line-start thinking blocks whose close marker ends
// a line. The inner text is matched against thinking when available.
func stripThinking(text, thinking string) string {
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], thinkingOpen)
		if i < 0 {
			break
		}
		start := from + i
		if start > 0 && text[start-1] != '\n' {
			from = start + len(thinkingOpen)
			continue
		}
		end := blockEnd(text, start, thinking)
		if end < 0 {
			from = start + len(thinkingOpen)
			continue
		}
		text = removePart(text, start, end-start)
		from = max(start-1, 0)
	}
	return text
}

// blockEnd returns the end of the longest inner text found in thinking,
// falling back to the first line-ending close marker when none matches.
func blockEnd(text string, start int, thinking string) int {
	innerStart := start + len(thinkingOpen)
	first := -1
	matched := -1
	for search := innerStart - 1; search < len(text); {
		j := strings.Index(text[search:], thinkingClose)
		if j < 0 {
			break
		}
		closeAt := search + j
		end := closeAt + len(thinkingClose)
		search = closeAt + 1
		if end < len(text) && text[end] != '\n' {
			continue
		}
		if first < 0 {
			first = end
		}
		inner := text[innerStart:max(closeAt, innerStart)]
		if thinking == "" {
			return end
		}
		if strings.Contains(thinking, inner) {
			matched = end
		}
	}
	if matched >= 0 {
		return matched
	}
	return first
}
