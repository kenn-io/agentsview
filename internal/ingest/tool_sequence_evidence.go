package ingest

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// toolResultContentUnknown classifies individual retained results before agent
// labels and separators are added for display. Selection matches the summary's
// latest nonempty result per agent, including the anonymous result.
func toolResultContentUnknown(call db.ToolCall) bool {
	switch len(call.ResultEvents) {
	case 0:
		return isUnknownResultContent(call.ResultContent)
	case 1:
		return isUnknownResultContent(call.ResultEvents[0].Content)
	}
	seen := make(map[string]bool)
	found := false
	for _, event := range slices.Backward(call.ResultEvents) {
		if strings.TrimSpace(event.Content) == "" {
			continue
		}
		agent := strings.TrimSpace(event.AgentID)
		if seen[agent] {
			continue
		}
		seen[agent] = true
		if !isUnknownResultContent(event.Content) {
			return false
		}
		found = true
	}
	return found
}

func isUnknownResultContent(content string) bool {
	content = strings.TrimSpace(content)
	return isStagedMarker(content) || isImageOnlyResult(content)
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

func isImageOnlyResult(content string) bool {
	content = strings.TrimSpace(content)
	imageMarkersOnly := strings.Contains(content, "[image]") &&
		strings.TrimSpace(strings.ReplaceAll(content, "[image]", "")) == ""
	return content == "[binary content]" || imageMarkersOnly || isImageOnlyJSON(content) ||
		isOffloadedImageReference(content)
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
	case "image", "input_image", "agentsview_image":
		return true, false, true
	case "input_text", "output_text", "text":
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
