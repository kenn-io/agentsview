package signals

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
)

type (
	ToolOutcome        string
	ToolRepeat         string
	ToolSequenceEnding string
)

const (
	ToolOutcomeErrored ToolOutcome = "errored"
	ToolOutcomeEmpty   ToolOutcome = "empty"
	ToolOutcomeContent ToolOutcome = "content"
	ToolOutcomeUnknown ToolOutcome = "unknown"

	ToolRepeatNone          ToolRepeat = "none"
	ToolRepeatIdentical     ToolRepeat = "identical"
	ToolRepeatNearIdentical ToolRepeat = "near_identical"

	ToolSequenceEndingRecovered ToolSequenceEnding = "recovered"
	ToolSequenceEndingAbandoned ToolSequenceEnding = "abandoned"
	ToolSequenceEndingOpen      ToolSequenceEnding = "open"
)

type ToolCallOutcome struct {
	ToolUseID      string
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Outcome        ToolOutcome
	Repeat         ToolRepeat
	ToolChanged    bool
}

type ToolSequence struct {
	Start         int
	End           int
	Identical     bool
	NearIdentical bool
	ToolChanged   bool
	Ending        ToolSequenceEnding
}

type ToolSequences struct {
	Calls     []ToolCallOutcome
	Sequences []ToolSequence
}

// ExtractToolSequences classifies ordered calls and groups observed
// non-content runs with their following recovery or tail ending.
func ExtractToolSequences(calls []ToolCallRow, complete bool) ToolSequences {
	if calls == nil {
		return ToolSequences{}
	}

	result := ToolSequences{
		Calls:     make([]ToolCallOutcome, 0, len(calls)),
		Sequences: make([]ToolSequence, 0),
	}
	activeStart := -1
	previousActive := -1
	identical := false
	nearIdentical := false
	toolChanged := false

	for i, call := range calls {
		outcome := classifyToolOutcome(call)
		observed := ToolCallOutcome{
			ToolUseID:      call.ToolUseID,
			MessageOrdinal: call.MessageOrdinal,
			CallIndex:      call.CallIndex,
			ToolName:       call.ToolName,
			Outcome:        outcome,
			Repeat:         ToolRepeatNone,
		}

		if activeStart >= 0 {
			observed.ToolChanged = call.ToolName != calls[previousActive].ToolName
			observed.Repeat = classifyToolRepeat(
				calls[previousActive], call,
			)
			identical = identical || observed.Repeat == ToolRepeatIdentical
			nearIdentical = nearIdentical ||
				observed.Repeat == ToolRepeatNearIdentical
			toolChanged = toolChanged || observed.ToolChanged
		}
		result.Calls = append(result.Calls, observed)

		if activeStart < 0 {
			if startsToolSequence(outcome) {
				activeStart = i
				previousActive = i
				identical = false
				nearIdentical = false
				toolChanged = false
			}
			continue
		}

		if outcome == ToolOutcomeContent {
			result.Sequences = append(result.Sequences, ToolSequence{
				Start:         activeStart,
				End:           i + 1,
				Identical:     identical,
				NearIdentical: nearIdentical,
				ToolChanged:   toolChanged,
				Ending:        ToolSequenceEndingRecovered,
			})
			activeStart = -1
			previousActive = -1
			continue
		}

		previousActive = i
	}

	if activeStart >= 0 {
		ending := ToolSequenceEndingOpen
		if complete {
			ending = ToolSequenceEndingAbandoned
		}
		result.Sequences = append(result.Sequences, ToolSequence{
			Start:         activeStart,
			End:           len(calls),
			Identical:     identical,
			NearIdentical: nearIdentical,
			ToolChanged:   toolChanged,
			Ending:        ending,
		})
	}
	return result
}

func startsToolSequence(outcome ToolOutcome) bool {
	return outcome == ToolOutcomeErrored || outcome == ToolOutcomeEmpty
}

func classifyToolOutcome(call ToolCallRow) ToolOutcome {
	if IsFailure(call) {
		return ToolOutcomeErrored
	}
	if call.EventStatus != "" && call.EventStatus != "completed" &&
		call.EventStatus != "errored" && call.EventStatus != "cancelled" {
		return ToolOutcomeUnknown
	}

	if isStagedOnlySummary(call.ResultContent) {
		return ToolOutcomeUnknown
	}
	if call.EventStatus == "completed" && call.ResultContentLength == 0 &&
		call.ResultContent == "" && isSupportedEmptyTool(call.ToolName) {
		return ToolOutcomeEmpty
	}
	if isMeasuredEmptyToolResult(call.ToolName, call.ResultContent) {
		return ToolOutcomeEmpty
	}
	if call.ResultContent == "" || isImageOnlySummary(call.ResultContent) {
		return ToolOutcomeUnknown
	}
	return ToolOutcomeContent
}

func isSupportedEmptyTool(toolName string) bool {
	switch toolName {
	case "Grep", "Glob", "Read", "search", "WebSearch", "search_web", "web_search":
		return true
	default:
		return false
	}
}

func isMeasuredEmptyToolResult(toolName, content string) bool {
	content = strings.TrimSpace(content)
	switch toolName {
	case "Grep":
		return content == "No matches found" || content == "No files found"
	case "Glob":
		return content == "No files found"
	default:
		return false
	}
}

func classifyToolRepeat(previous, current ToolCallRow) ToolRepeat {
	if previous.ToolName != current.ToolName || current.InputJSON == "" {
		return ToolRepeatNone
	}
	if previous.InputJSON == current.InputJSON && previous.InputJSON != "" {
		return ToolRepeatIdentical
	}
	if previous.InputJSON == "" {
		return ToolRepeatNone
	}
	previousNormalized, previousOK := normalizeToolInput(previous.InputJSON)
	currentNormalized, currentOK := normalizeToolInput(current.InputJSON)
	if previousOK && currentOK && previousNormalized == currentNormalized &&
		previous.InputJSON != current.InputJSON {
		return ToolRepeatNearIdentical
	}
	return ToolRepeatNone
}

func normalizeToolInput(input string) (string, bool) {
	value := jsontext.Value([]byte(input))
	if err := value.Format(
		jsontext.CanonicalizeRawInts(false),
		jsontext.CanonicalizeRawFloats(false),
		jsontext.ReorderRawObjects(true),
	); err != nil {
		return "", false
	}
	return string(value), true
}

func isStagedOnlySummary(content string) bool {
	sections := summarySections(content)
	if len(sections) == 0 {
		return false
	}
	for _, section := range sections {
		section = stripSummaryLabel(section)
		lines := strings.Split(section, "\n")
		found := false
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if !isStagedMarker(line) {
				return false
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
}

func isStagedMarker(line string) bool {
	if !strings.HasPrefix(line, "staged:") || len(line) == len("staged:") {
		return false
	}
	for _, r := range line[len("staged:"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isImageOnlySummary(content string) bool {
	trimmed := strings.TrimSpace(content)
	if isImageOnlyJSON(trimmed) || isOffloadedImageReference(trimmed) {
		return true
	}
	if firstLine, rest, found := strings.Cut(trimmed, "\n"); found &&
		strings.HasSuffix(strings.TrimSpace(firstLine), ":") {
		body := strings.TrimSpace(rest)
		if isImageOnlyJSON(body) || isOffloadedImageReference(body) {
			return true
		}
	}
	sections := summarySections(content)
	if len(sections) == 0 {
		return false
	}
	for _, section := range sections {
		section = strings.TrimSpace(stripSummaryLabel(section))
		if !isImageOnlyJSON(section) && !isOffloadedImageReference(section) {
			return false
		}
	}
	return true
}

func isOffloadedImageReference(content string) bool {
	content = strings.TrimSpace(content)
	if strings.ContainsAny(content, "\r\n") ||
		!strings.HasPrefix(content, "![Image:") ||
		strings.Count(content, "](asset://") != 1 {
		return false
	}
	open := strings.Index(content, "](asset://")
	tail := content[open+2:]
	if !strings.HasSuffix(tail, ")") ||
		strings.IndexByte(tail, ')') != len(tail)-1 {
		return false
	}
	ref := tail[:len(tail)-1]
	return strings.HasPrefix(ref, "asset://") &&
		!strings.ContainsAny(ref, "()")
}

func summarySections(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	sections := make([]string, 0)
	start := 0
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(content); i++ {
		ch := content[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '\n':
			if depth == 0 && i+1 < len(content) &&
				content[i+1] == '\n' {
				if part := strings.TrimSpace(content[start:i]); part != "" {
					sections = append(sections, part)
				}
				start = i + 2
				i++
			}
		}
	}
	if part := strings.TrimSpace(content[start:]); part != "" {
		sections = append(sections, part)
	}
	return sections
}

func stripSummaryLabel(section string) string {
	firstLine, rest, found := strings.Cut(section, "\n")
	if !found {
		colon := strings.IndexByte(section, ':')
		if colon > 0 {
			rest := strings.TrimSpace(section[colon+1:])
			if isStagedMarker(rest) || strings.HasPrefix(rest, "[") ||
				strings.HasPrefix(rest, "{") ||
				strings.HasPrefix(rest, "![Image:") {
				return rest
			}
		}
		return section
	}
	first := strings.TrimSpace(firstLine)
	if !strings.HasSuffix(first, ":") || strings.HasPrefix(first, "{") ||
		strings.HasPrefix(first, "[") {
		return section
	}
	return strings.TrimSpace(rest)
}

func isImageOnlyJSON(content string) bool {
	var blocks []jsontext.Value
	if err := json.Unmarshal([]byte(content), &blocks); err == nil && len(blocks) > 0 {
		hasImage := false
		for _, block := range blocks {
			image, text, ok := classifyResultBlock(block)
			if !ok || text {
				return false
			}
			hasImage = hasImage || image
		}
		return hasImage
	}

	var block jsontext.Value
	if err := json.Unmarshal([]byte(content), &block); err != nil {
		return false
	}
	image, text, ok := classifyResultBlock(block)
	return ok && image && !text
}

func classifyResultBlock(raw jsontext.Value) (image, text, ok bool) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return false, false, false
	}
	var kind string
	if value, found := fields["type"]; found {
		if err := json.Unmarshal(value, &kind); err != nil {
			return false, false, false
		}
	}
	switch kind {
	case "input_image", "agentsview_image":
		return true, false, true
	case "text":
		var value string
		if rawText, found := fields["text"]; found {
			if err := json.Unmarshal(rawText, &value); err != nil {
				return false, false, false
			}
		}
		return false, strings.TrimSpace(value) != "", true
	default:
		return false, false, false
	}
}
