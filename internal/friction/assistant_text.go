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
	agent, content, thinking string, calls []RawToolCall, redacted bool,
) string {
	text := stripThinking(content, thinking)
	if agent == "openhands" && len(calls) == 1 {
		if at := openHandsSummaryStart(text, calls[0].ToolName); at >= 0 {
			return removePart(text, at, len(text)-at)
		}
	}
	for _, call := range calls {
		if rendering := renderingInText(text, call, redacted); rendering != "" {
			text = removePart(text, strings.Index(text, rendering), len(rendering))
		}
	}
	return text
}

// OpenHands ActionEvents contain prose, one action, then optional thinking.
// After thinking is stripped, the action ends the message. Its event summary
// is absent from stored arguments, so match the summary header at a line start.
func openHandsSummaryStart(text, toolName string) int {
	label, end := toolName, len(text)-1
	switch toolName {
	case "terminal":
		label = "Bash"
		end = strings.LastIndex(text, "]\n$ ")
		if strings.HasSuffix(text, "]\n$") {
			end = len(text) - len("]\n$") // Empty-command polls are trimmed.
		}
	case "file_editor", "delegate", "task_tracker":
		return -1 // These actions do not render the event summary.
	default:
		if !strings.HasSuffix(text, "]") {
			return -1
		}
	}
	if end < 0 {
		return -1
	}
	header := "[" + label + ": "
	if at := strings.LastIndex(text[:end], "\n"+header); at >= 0 {
		return at + 1
	}
	if strings.HasPrefix(text, header) {
		return 0
	}
	return -1
}

// renderingInText chooses the longest present rendering in the preferred
// form, falling back to the other form only when none is present.
func renderingInText(text string, call RawToolCall, redacted bool) string {
	pairs := parser.ToolUseRenderingCandidates(
		call.Category, call.ToolName, call.InputJSON,
	)
	pick := func(
		form func(parser.ToolUseRenderingPair) string,
		skipFullRenderingPrefixes bool,
	) string {
		best := ""
		for _, pair := range pairs {
			rendering := form(pair)
			if len(rendering) <= len(best) {
				continue
			}
			for from := 0; from < len(text); {
				i := strings.Index(text[from:], rendering)
				if i < 0 {
					break
				}
				at := from + i
				if skipFullRenderingPrefixes && prefixesFullRendering(
					text[at:], rendering, pairs,
				) {
					from = at + len(rendering)
					continue
				}
				best = rendering
				break
			}
		}
		return best
	}
	full := func(pair parser.ToolUseRenderingPair) string { return pair.Full }
	red := func(pair parser.ToolUseRenderingPair) string { return pair.Redacted }
	if redacted {
		if rendering := pick(red, true); rendering != "" {
			return rendering
		}
		return pick(full, false)
	}
	if rendering := pick(full, false); rendering != "" {
		return rendering
	}
	return pick(red, false)
}

// prefixesFullRendering reports whether a redacted candidate at the start of
// text is only the header of a longer rendering for the same call.
func prefixesFullRendering(
	text, redacted string, pairs []parser.ToolUseRenderingPair,
) bool {
	for _, pair := range pairs {
		if len(pair.Full) > len(redacted) &&
			strings.HasPrefix(pair.Full, redacted) &&
			strings.HasPrefix(text, pair.Full) {
			return true
		}
	}
	return false
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

// blockEnd returns the end of the longest inner text found in thinking.
// Without thinking text, it uses the first line-ending close marker.
func blockEnd(text string, start int, thinking string) int {
	innerStart := start + len(thinkingOpen)
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
		inner := text[innerStart:max(closeAt, innerStart)]
		if thinking == "" {
			return end
		}
		if strings.Contains(thinking, inner) {
			matched = end
		}
	}
	return matched
}
