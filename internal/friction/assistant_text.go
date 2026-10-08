package friction

import (
	"encoding/json/v2"
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
	if len(calls) == 0 {
		return text
	}
	// The Codex parser emits every tool call as its own message whose content
	// is only the call's rendering, so these messages carry no prose. Headers
	// can show a summary or raw input that cannot be rebuilt from stored rows.
	switch parser.AgentType(agent) {
	case parser.AgentCodex, parser.AgentTraeX, parser.AgentAugureCode:
		return ""
	default:
		// Other parsers inline renderings beside prose.
	}
	if agent == "openhands" && len(calls) == 1 {
		if at := openHandsSummaryStart(text, calls[0].ToolName); at >= 0 {
			return removePart(text, at, len(text)-at)
		}
	}
	parts := textParts{newTextPart(text)}
	for _, call := range calls {
		if at, n := renderingInText(agent, parts, call, redacted); at >= 0 {
			parts = parts.remove(at, n)
		}
	}
	return parts.String()
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
func renderingInText(agent string, text textParts, call RawToolCall, redacted bool) (int, int) {
	input := call.InputJSON
	// Transcript-only archives drop arguments but keep the path the
	// rendering shows, e.g. "[Read: TODO.md]". Fill every key renderers
	// read a path from, so each provider's form can be rebuilt.
	if input == "" && call.FilePath != "" {
		args := map[string]string{}
		for _, key := range []string{"file_path", "path", "filePath", "file"} {
			args[key] = call.FilePath
		}
		if b, err := json.Marshal(args, json.Deterministic(true)); err == nil {
			input = string(b)
		}
	}
	// Full-content Claude archives normally contain this exact rendering.
	// Avoid scanning the arguments again to build an unused redacted form.
	if !redacted {
		switch parser.AgentType(agent) {
		case parser.AgentClaude, parser.AgentOpenClaude, parser.AgentCowork:
			full := parser.ToolUseRendering(call.ToolName, input)
			if full != "" {
				if at := text.index(full, 0); at >= 0 {
					return at, len(full)
				}
			}
		default:
			// Other providers use the candidate search below.
		}
	}
	pairs := parser.ToolUseRenderingCandidates(
		parser.AgentType(agent), call.Category, call.ToolName, input,
	)
	pick := func(
		form func(parser.ToolUseRenderingPair) string,
		skipFullRenderingPrefixes bool,
	) (int, int) {
		bestAt, bestLen := -1, 0
		for _, pair := range pairs {
			rendering := form(pair)
			if len(rendering) <= bestLen {
				continue
			}
			first := text.index(rendering, 0)
			for at := first; at >= 0; at = text.index(rendering, at+len(rendering)) {
				if skipFullRenderingPrefixes && prefixesFullRendering(
					text, at, rendering, pairs,
				) {
					continue
				}
				// Selection has always removed the first occurrence of the
				// chosen rendering, even when a later occurrence selected it.
				bestAt, bestLen = first, len(rendering)
				break
			}
		}
		return bestAt, bestLen
	}
	full := func(pair parser.ToolUseRenderingPair) string { return pair.Full }
	red := func(pair parser.ToolUseRenderingPair) string { return pair.Redacted }
	if redacted {
		if at, n := pick(red, true); at >= 0 {
			return at, n
		}
		if at, n := pick(full, false); at >= 0 {
			return at, n
		}
		return labeledHeaderInText(text, pairs, red)
	}
	if at, n := pick(full, false); at >= 0 {
		return at, n
	}
	return pick(red, false)
}

// labeledHeaderInText matches a rendering whose header detail could not be
// rebuilt: a whole-line "[Label: detail]" header followed by the rest of the
// rebuilt form. Transcript-only archives drop arguments such as Gemini's
// dir_path.
func labeledHeaderInText(
	text textParts, pairs []parser.ToolUseRenderingPair,
	form func(parser.ToolUseRenderingPair) string,
) (int, int) {
	for _, pair := range pairs {
		rendering := form(pair)
		inside, ok := strings.CutPrefix(rendering, "[")
		if !ok {
			continue
		}
		label, _, _ := strings.Cut(inside, "]")
		label, _, _ = strings.Cut(label, ":")
		if label == "" {
			continue
		}
		_, body, _ := strings.Cut(rendering, "\n")
		header := "[" + label + ": "
		for at := text.index(header, 0); at >= 0; at = text.index(header, at+1) {
			if at > 0 && !text.hasPrefix(at-1, "\n") {
				continue
			}
			s := text.String()
			end := strings.IndexByte(s[at:], '\n')
			if end < 0 {
				end = len(s) - at
			}
			if !strings.HasSuffix(s[at:at+end], "]") {
				continue
			}
			if body == "" {
				return at, end
			}
			if text.hasPrefix(at+end, "\n"+body) {
				return at, end + 1 + len(body)
			}
		}
	}
	return -1, 0
}

// prefixesFullRendering reports whether a redacted candidate at the start of
// text is only the header of a longer rendering for the same call.
func prefixesFullRendering(
	text textParts, at int, redacted string, pairs []parser.ToolUseRenderingPair,
) bool {
	for _, pair := range pairs {
		if len(pair.Full) > len(redacted) &&
			strings.HasPrefix(pair.Full, redacted) &&
			text.hasPrefix(at, pair.Full) {
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
