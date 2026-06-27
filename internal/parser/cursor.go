package parser

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// maxCursorTranscriptSize is the maximum transcript file size
// we'll read into memory. Cursor transcripts are typically
// under 500 KB; 10 MB provides generous headroom.
const maxCursorTranscriptSize = 10 << 20

// parseSession parses a Cursor agent transcript file. Transcripts are plain
// text with "user:" and "assistant:" role markers, tool calls, and thinking
// blocks.
func (p *cursorProvider) parseSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	// Open with O_NOFOLLOW (Unix) to reject symlinks at the
	// final path component, closing the TOCTOU window between
	// discovery validation and file read.
	f, err := openNoFollow(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Use Fstat on the open fd — this reflects the actual
	// opened file, not whatever the path currently points to.
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf(
			"skip %s: not a regular file", path,
		)
	}

	// Use LimitReader to enforce the size cap even if the
	// file grows after Fstat. Read one extra byte so we can
	// detect truncation.
	data, err := io.ReadAll(
		io.LimitReader(f, maxCursorTranscriptSize+1),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > maxCursorTranscriptSize {
		return nil, nil, fmt.Errorf(
			"skip %s: file too large (read %d bytes, max %d)",
			path, len(data), maxCursorTranscriptSize,
		)
	}

	text := string(data)
	var messages []ParsedMessage
	if isCursorJSONL(text) {
		messages = parseCursorJSONL(text)
	} else {
		lines := strings.Split(text, "\n")
		messages = parseCursorMessages(lines)
	}
	if len(messages) == 0 {
		return nil, nil, nil
	}

	sessionID := CursorSessionID(path)

	var firstMessage string
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			firstMessage = truncate(
				strings.ReplaceAll(m.Content, "\n", " "), 300,
			)
			break
		}
	}

	// Compute hash from the already-read data to avoid
	// re-opening the file by path (which would be another
	// TOCTOU opportunity).
	hash := fmt.Sprintf("%x", sha256.Sum256(data))

	mtime := info.ModTime()
	sess := &ParsedSession{
		ID:           sessionID,
		Project:      project,
		Machine:      machine,
		Agent:        AgentCursor,
		FirstMessage: firstMessage,
		StartedAt:    mtime,
		EndedAt:      mtime,
		MessageCount: len(messages),
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: mtime.UnixNano(),
			Hash:  hash,
		},
	}
	return sess, messages, nil
}

// cursorBlock represents a raw block of lines between role
// markers in a Cursor transcript.
type cursorBlock struct {
	role  RoleType
	lines []string
}

// parseCursorMessages splits transcript lines on role markers
// and converts each block into a ParsedMessage.
func parseCursorMessages(lines []string) []ParsedMessage {
	blocks := splitCursorBlocks(lines)
	messages := make([]ParsedMessage, 0, len(blocks))

	for i, block := range blocks {
		content, hasThinking, toolCalls := extractCursorContent(
			block.role, block.lines,
		)
		content = strings.TrimSpace(content)
		if content == "" && len(toolCalls) == 0 {
			continue
		}

		messages = append(messages, ParsedMessage{
			Ordinal:       i,
			Role:          block.role,
			Content:       content,
			Timestamp:     time.Time{},
			HasThinking:   hasThinking,
			HasToolUse:    len(toolCalls) > 0,
			ContentLength: len(content),
			ToolCalls:     toolCalls,
		})
	}

	// Re-number ordinals to be contiguous after filtering
	for i := range messages {
		messages[i].Ordinal = i
	}
	return messages
}

// splitCursorBlocks splits lines into blocks delimited by
// "user:" or "assistant:" on a line by itself.
func splitCursorBlocks(lines []string) []cursorBlock {
	var blocks []cursorBlock
	var current *cursorBlock

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "user:" || trimmed == "assistant:" {
			if current != nil {
				blocks = append(blocks, *current)
			}
			role := RoleUser
			if trimmed == "assistant:" {
				role = RoleAssistant
			}
			current = &cursorBlock{role: role}
			continue
		}
		if current != nil {
			current.lines = append(current.lines, line)
		}
	}
	if current != nil {
		blocks = append(blocks, *current)
	}
	return blocks
}

// extractCursorContent processes lines for a single message
// block, returning the visible text content, whether thinking
// was present, and any tool calls found.
func extractCursorContent(
	role RoleType, lines []string,
) (string, bool, []ParsedToolCall) {
	if role == RoleUser {
		content := extractUserQuery(lines)
		return content, false, nil
	}
	return extractAssistantContent(lines)
}

// extractUserQuery extracts text from <user_query> tags.
// Falls back to joining all lines if no tags are found.
func extractUserQuery(lines []string) string {
	text := strings.Join(lines, "\n")

	start := strings.Index(text, "<user_query>")
	end := strings.Index(text, "</user_query>")
	if start >= 0 && end > start {
		return strings.TrimSpace(
			text[start+len("<user_query>") : end],
		)
	}

	return strings.TrimSpace(text)
}

// extractAssistantContent parses assistant message lines for
// visible text, thinking blocks, and tool calls.
func extractAssistantContent(
	lines []string,
) (string, bool, []ParsedToolCall) {
	var textParts []string
	var toolCalls []ParsedToolCall
	hasThinking := false

	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Thinking block
		if strings.HasPrefix(trimmed, "[Thinking]") {
			hasThinking = true
			i++
			for i < len(lines) {
				if isBlockBodyEnd(lines[i]) {
					break
				}
				i++
			}
			continue
		}

		// Tool call
		if toolName, ok := strings.CutPrefix(
			trimmed, "[Tool call] ",
		); ok {
			i++
			bodyStart := i
			for i < len(lines) {
				if isBlockBodyEnd(lines[i]) {
					break
				}
				i++
			}
			inputJSON := cursorToolInputJSON(
				toolName,
				lines[bodyStart:i],
			)
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolName:  toolName,
				Category:  NormalizeToolCategory(toolName),
				InputJSON: inputJSON,
				SkillName: inferToolSkillName(
					toolName,
					inputJSON,
				),
			})
			continue
		}

		// Tool result — skip the header and body
		if strings.HasPrefix(trimmed, "[Tool result]") {
			i++
			for i < len(lines) {
				if isBlockBodyEnd(lines[i]) {
					break
				}
				i++
			}
			continue
		}

		// Regular text
		textParts = append(textParts, line)
		i++
	}

	content := strings.TrimSpace(strings.Join(textParts, "\n"))
	return content, hasThinking, toolCalls
}

func cursorToolInputJSON(toolName string, lines []string) string {
	raw := strings.TrimSpace(strings.Join(dedentCursorBlock(lines), "\n"))
	if raw == "" {
		return ""
	}
	if gjson.Valid(raw) {
		return raw
	}
	if strings.EqualFold(toolName, "ApplyPatch") &&
		(strings.Contains(raw, "*** Begin Patch") ||
			strings.HasPrefix(raw, "@@")) {
		return marshalCursorToolParams(map[string]string{
			"patch": raw,
		})
	}

	params := make(map[string]string)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return ""
		}
		params[key] = strings.TrimSpace(value)
	}
	if len(params) == 0 {
		return ""
	}
	return marshalCursorToolParams(params)
}

func dedentCursorBlock(lines []string) []string {
	minIndent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := cursorLineIndent(line)
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent <= 0 {
		return lines
	}

	dedented := make([]string, len(lines))
	for i, line := range lines {
		if len(line) < minIndent {
			dedented[i] = strings.TrimLeft(line, " \t")
			continue
		}
		dedented[i] = line[minIndent:]
	}
	return dedented
}

func cursorLineIndent(line string) int {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return i
		}
	}
	return len(line)
}

func marshalCursorToolParams(params map[string]string) string {
	data, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	return string(data)
}

// isAssistantMarker returns true if the line is a structural
// marker within an assistant block (thinking, tool call, or
// tool result).
func isAssistantMarker(trimmed string) bool {
	return strings.HasPrefix(trimmed, "[Thinking]") ||
		strings.HasPrefix(trimmed, "[Tool call] ") ||
		strings.HasPrefix(trimmed, "[Tool result]")
}

// isBlockBodyEnd returns true if line signals the end of a
// structured block body (thinking, tool call, or tool result).
// Block bodies consist of indented or empty lines; a non-empty
// line at the left margin is either a new marker or regular
// assistant prose that should not be consumed.
func isBlockBodyEnd(line string) bool {
	trimmed := strings.TrimSpace(line)
	if isAssistantMarker(trimmed) {
		return true
	}
	// Non-empty line at left margin ends the block.
	return trimmed != "" && len(line) > 0 && line[0] != ' ' &&
		line[0] != '\t'
}

// CursorSessionID derives a session ID from a transcript file
// path by stripping whatever extension is present.
func CursorSessionID(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return "cursor:" + base
}

// isCursorJSONL returns true if the data looks like JSONL
// (Anthropic API message format) rather than plain text.
// Scans up to 4 KB to locate the first non-empty line, then
// validates the full line from the original data.
func isCursorJSONL(data string) bool {
	const maxScan = 4096

	// Find the byte offset of the first non-whitespace
	// character within the scan window.
	limit := min(len(data), maxScan)
	lineStart := -1
	for i := range limit {
		if data[i] != '\n' && data[i] != '\r' &&
			data[i] != ' ' && data[i] != '\t' {
			lineStart = i
			break
		}
	}
	if lineStart < 0 {
		return false
	}

	// Extract the full first line from the original data
	// (not the truncated scan window).
	lineEnd := strings.IndexByte(data[lineStart:], '\n')
	var line string
	if lineEnd < 0 {
		line = data[lineStart:]
	} else {
		line = data[lineStart : lineStart+lineEnd]
	}
	return gjson.Valid(strings.TrimSpace(line))
}

// parseCursorJSONL parses a Cursor JSONL transcript where
// each line is an Anthropic API message object with "role"
// and "message.content" fields.
func parseCursorJSONL(data string) []ParsedMessage {
	lines := strings.Split(data, "\n")
	var messages []ParsedMessage
	ordinal := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !gjson.Valid(line) {
			continue
		}

		role := gjson.Get(line, "role").Str
		if role != "user" && role != "assistant" {
			continue
		}

		content := gjson.Get(line, "message.content")
		if !content.Exists() {
			continue
		}

		var msg ParsedMessage
		msg.Ordinal = ordinal

		if role == "user" {
			msg.Role = RoleUser
			msg.Content = extractJSONLUserContent(content)
		} else {
			msg.Role = RoleAssistant
			text, _, hasThinking, hasToolUse,
				toolCalls, toolResults :=
				ExtractTextContent(content)
			msg.Content = text
			msg.HasThinking = hasThinking
			msg.HasToolUse = hasToolUse
			msg.ToolCalls = toolCalls
			msg.ToolResults = toolResults
		}

		msg.Content = strings.TrimSpace(msg.Content)
		msg.ContentLength = len(msg.Content)
		if msg.Content == "" &&
			len(msg.ToolCalls) == 0 &&
			len(msg.ToolResults) == 0 {
			continue
		}

		messages = append(messages, msg)
		ordinal++
	}
	return messages
}

// extractJSONLUserContent extracts text from a user message's
// content field. If the content is a string, strips
// <user_query> tags. If it's an array of blocks, collects
// text blocks and strips tags from the combined result.
func extractJSONLUserContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return extractUserQuery(
			strings.Split(content.Str, "\n"),
		)
	}

	if !content.IsArray() {
		return ""
	}

	var parts []string
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").Str == "text" {
			text := block.Get("text").Str
			if text != "" {
				parts = append(parts, text)
			}
		}
		return true
	})

	if len(parts) == 0 {
		return ""
	}
	combined := strings.Join(parts, "\n")
	return extractUserQuery(strings.Split(combined, "\n"))
}

// cursorHighMarkers are directory names that are very
// unlikely to appear inside usernames (capitalized or
// plural forms). Checked first during path decoding.
var cursorHighMarkers = map[string]bool{
	"Documents": true, "Code": true,
	"projects": true, "repos": true,
}

// cursorLowMarkers are directory names that could
// plausibly appear in usernames (short, lowercase).
// Only checked as a fallback after high markers.
var cursorLowMarkers = map[string]bool{
	"code": true, "src": true,
	"work": true, "dev": true,
}

// DecodeCursorProjectDir extracts a clean project name from
// a Cursor-style hyphenated directory name. Cursor encodes
// absolute paths by replacing / and . with hyphens, e.g.
// "Users-fiona-Documents-mcp-cursor-analytics".
//
// Scans forward from the home-directory root to find the
// first marker, handling multi-token usernames (e.g.
// "Users-john-doe-Documents-project").
func DecodeCursorProjectDir(dirName string) string {
	if dirName == "" {
		return ""
	}

	parts := strings.Split(dirName, "-")

	// Determine the earliest position a marker could
	// appear, based on the platform root prefix:
	//   macOS/Linux: Users-<name>-<marker> (min idx 2)
	//                home-<name>-<marker>  (min idx 2)
	//   Windows:     C-Users-<name>-<marker> (min idx 3)
	minIdx := -1
	if len(parts) >= 3 &&
		(parts[0] == "Users" || parts[0] == "home") {
		minIdx = 2
	} else if len(parts) >= 4 &&
		len(parts[0]) == 1 && parts[1] == "Users" {
		minIdx = 3
	}

	// Two-pass scan: high-confidence markers first (unlikely
	// in usernames), then low-confidence as fallback. This
	// handles "Users-john-code-doe-Documents-app" correctly
	// (finds Documents, not code).
	if minIdx >= 0 {
		for _, tier := range []map[string]bool{
			cursorHighMarkers, cursorLowMarkers,
		} {
			for i := minIdx; i < len(parts)-1; i++ {
				if !tier[parts[i]] {
					continue
				}
				result := strings.Join(
					parts[i+1:], "-",
				)
				if result != "" {
					return NormalizeName(result)
				}
			}
		}
	}

	// Fallback: last two components
	if len(parts) >= 2 {
		return NormalizeName(
			strings.Join(parts[len(parts)-2:], "-"),
		)
	}
	return NormalizeName(dirName)
}
