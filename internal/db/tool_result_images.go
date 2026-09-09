package db

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// ToolImageStats describes inline image payloads found by the projection.
type ToolImageStats struct {
	Payloads     int64
	StoredBytes  int64
	DecodedBytes int64
}

type toolImageBlock struct {
	Type     string          `json:"type"`
	ImageURL json.RawMessage `json:"image_url"`
}

// StripToolResultImages replaces supported inline image blocks in one stored
// result. Untouched JSON blocks retain their original bytes.
func StripToolResultImages(content string) (string, ToolImageStats) {
	projected, stats := stripToolResultImageArray(content)
	if stats.Payloads > 0 {
		return projected, stats
	}
	return stripToolResultSummaryImages(content)
}

func stripToolResultImageArray(content string) (string, ToolImageStats) {
	var blocks []json.RawMessage
	if err := json.Unmarshal([]byte(content), &blocks); err != nil || blocks == nil {
		return content, ToolImageStats{}
	}

	projected := make([]json.RawMessage, len(blocks))
	copy(projected, blocks)
	var stats ToolImageStats
	changed := false
	for i, raw := range blocks {
		var block toolImageBlock
		if err := json.Unmarshal(raw, &block); err != nil ||
			block.Type != "input_image" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			continue
		}
		mediaType, decodedBytes, storedBytes, ok := decodeInlineImageURL(block.ImageURL)
		if !ok {
			continue
		}
		placeholderFields := make(map[string]json.RawMessage, len(fields)+6)
		for key, value := range fields {
			if !strings.EqualFold(key, "type") &&
				!strings.EqualFold(key, "image_url") {
				placeholderFields[key] = value
			}
		}
		placeholderFields["type"] = json.RawMessage(`"agentsview_image"`)
		placeholderFields["version"] = json.RawMessage(`1`)
		textValue, _ := json.Marshal(
			fmt.Sprintf("[Image: %s, %d bytes]", mediaType, decodedBytes),
		)
		placeholderFields["text"] = textValue
		mediaValue, _ := json.Marshal(mediaType)
		placeholderFields["media_type"] = mediaValue
		byteSizeValue, _ := json.Marshal(decodedBytes)
		placeholderFields["byte_size"] = byteSizeValue
		sha256Value := json.RawMessage(`""`)
		if rawSHA, ok := fields["sha256"]; ok {
			var sha256 string
			if err := json.Unmarshal(rawSHA, &sha256); err == nil && sha256 != "" {
				sha256Value = rawSHA
			}
		}
		placeholderFields["sha256"] = sha256Value
		placeholder, err := json.Marshal(placeholderFields)
		if err != nil {
			continue
		}
		projected[i] = placeholder
		changed = true
		stats.Payloads++
		stats.StoredBytes += storedBytes
		stats.DecodedBytes += decodedBytes
	}
	if !changed {
		return content, ToolImageStats{}
	}
	var result bytes.Buffer
	result.WriteByte('[')
	for i, block := range projected {
		if i > 0 {
			result.WriteByte(',')
		}
		result.Write(block)
	}
	result.WriteByte(']')
	return result.String(), stats
}

func stripToolResultSummaryImages(content string) (string, ToolImageStats) {
	var result strings.Builder
	var stats ToolImageStats
	copied := 0
	for start := 0; start < len(content); {
		section := strings.TrimLeft(content[start:], " \t\r\n")
		arrayStart := len(content) - len(section)
		if !strings.HasPrefix(section, "[") {
			if newline := strings.IndexByte(section, '\n'); newline > 0 &&
				strings.HasSuffix(strings.TrimSpace(section[:newline]), ":") {
				arrayStart += newline + 1
			}
		}

		// Decode the complete value before looking for the next separator:
		// provider JSON can itself contain blank lines. Anonymous sections have
		// no agent label, including the trailing section of a mixed summary.
		decoder := json.NewDecoder(strings.NewReader(content[arrayStart:]))
		var raw json.RawMessage
		scanEnd := start
		if err := decoder.Decode(&raw); err == nil {
			end := arrayStart + int(decoder.InputOffset())
			scanEnd = end
			tail, _, _ := strings.Cut(content[end:], "\n\n")
			if len(raw) > 0 && raw[0] == '[' && strings.TrimSpace(tail) == "" {
				projected, found := stripToolResultImageArray(string(raw))
				if found.Payloads > 0 {
					result.WriteString(content[copied:arrayStart])
					result.WriteString(projected)
					copied = end
					stats.Payloads += found.Payloads
					stats.StoredBytes += found.StoredBytes
					stats.DecodedBytes += found.DecodedBytes
				}
			}
		}
		separator := strings.Index(content[scanEnd:], "\n\n")
		if separator < 0 {
			break
		}
		start = scanEnd + separator + 2
	}
	if stats.Payloads == 0 {
		return content, stats
	}
	result.WriteString(content[copied:])
	return result.String(), stats
}

func decodeInlineImageURL(raw json.RawMessage) (string, int64, int64, bool) {
	var uri string
	if err := json.Unmarshal(raw, &uri); err != nil {
		return "", 0, 0, false
	}
	if !strings.HasPrefix(strings.ToLower(uri), "data:") {
		return "", 0, 0, false
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return "", 0, 0, false
	}
	header := uri[:comma]
	payload := uri[comma+1:]
	if payload == "" {
		return "", 0, 0, false
	}
	parts := strings.Split(header, ";")
	if len(parts) != 2 || !strings.EqualFold(parts[1], "base64") {
		return "", 0, 0, false
	}
	mediaType := parts[0][len("data:"):]
	parsedType, _, err := mime.ParseMediaType(mediaType)
	if err != nil || !strings.HasPrefix(strings.ToLower(parsedType), "image/") {
		return "", 0, 0, false
	}
	var count countingWriter
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(payload))
	if _, err := io.Copy(&count, decoder); err != nil {
		return "", 0, 0, false
	}
	return parsedType, count.n, int64(len(uri)), true
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// ProjectToolResultImages applies the configured policy to a message graph.
// Drop mode copies the graph and its nested tool-result slices before editing.
func ProjectToolResultImages(
	messages []Message, policy config.ToolResultImages,
) ([]Message, ToolImageStats) {
	if policy != config.ToolResultImagesDrop || len(messages) == 0 {
		if messages == nil {
			return []Message{}, ToolImageStats{}
		}
		return messages, ToolImageStats{}
	}
	projected := make([]Message, len(messages))
	copy(projected, messages)
	var stats ToolImageStats
	for i := range projected {
		projected[i].ToolCalls = append([]ToolCall(nil), messages[i].ToolCalls...)
		for j := range projected[i].ToolCalls {
			call := &projected[i].ToolCalls[j]
			call.ResultEvents = append([]ToolResultEvent(nil), call.ResultEvents...)
			call.ResultContent, stats = projectToolResultText(call.ResultContent, call.ResultContentLength, stats)
			call.ResultContentLength = ResolveResultContentLength(
				call.ResultContent, call.ResultContentLength,
			)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.Content, stats = projectToolResultText(
					event.Content, event.ContentLength, stats,
				)
				event.ContentLength = ResolveResultContentLength(
					event.Content, event.ContentLength,
				)
			}
			// A deduplicated summary keeps its text in the sole event, so its
			// retained length must follow that event's projected content.
			if call.ResultContent == "" && call.ResultContentLength > 0 &&
				len(call.ResultEvents) == 1 && call.ResultEvents[0].Content != "" {
				call.ResultContentLength = call.ResultEvents[0].ContentLength
			}
		}
	}
	return projected, stats
}

func projectToolResultText(
	content string, _ int, stats ToolImageStats,
) (string, ToolImageStats) {
	projected, found := StripToolResultImages(content)
	stats.Payloads += found.Payloads
	stats.StoredBytes += found.StoredBytes
	stats.DecodedBytes += found.DecodedBytes
	return projected, stats
}

// SetToolResultImages stores the policy on a writable database handle.
func (db *DB) SetToolResultImages(policy config.ToolResultImages) {
	if db.readOnly {
		return
	}
	if policy != config.ToolResultImagesDrop {
		policy = config.ToolResultImagesKeep
	}
	db.toolResultImages = policy
}

// ToolResultImages returns the policy carried by this database handle.
func (db *DB) ToolResultImages() config.ToolResultImages {
	if db.toolResultImages == config.ToolResultImagesDrop {
		return config.ToolResultImagesDrop
	}
	return config.ToolResultImagesKeep
}

// ProjectToolResultImages applies the handle's configured policy.
func (db *DB) ProjectToolResultImages(messages []Message) ([]Message, ToolImageStats) {
	return ProjectToolResultImages(messages, db.ToolResultImages())
}
