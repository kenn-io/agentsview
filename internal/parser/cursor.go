package parser

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	path, project, cwd, machine string,
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
	startedAt, endedAt := cursorSessionBounds(messages, mtime)
	sess := &ParsedSession{
		ID:           sessionID,
		Project:      project,
		Machine:      machine,
		Agent:        AgentCursor,
		Cwd:          cwd,
		FirstMessage: firstMessage,
		StartedAt:    startedAt,
		EndedAt:      endedAt,
		MessageCount: len(messages),
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: mtime.UnixNano(),
			Hash:  hash,
		},
	}
	// A delegating transcript records only a Subagent tool_use block with a
	// null id and no tool_result, so the subagents directory is the sole link
	// from a child to the session that spawned it.
	if loc, ok := cursorTranscriptLocationFromPath(path); ok && loc.ParentRawID != "" {
		sess.ParentSessionID = cursorSessionIDPrefix + loc.ParentRawID
		sess.RelationshipType = RelSubagent
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
		body, timestamp := extractCursorBody(block.role, block.lines)
		if !body.hasNativeBody() {
			continue
		}
		messages = append(messages, ParsedMessage{
			Ordinal: i, Role: block.role, Content: body.Content,
			Timestamp: timestamp,
		}.withBody(body))
	}

	// Re-number ordinals to be contiguous after filtering
	for i := range messages {
		messages[i].Ordinal = i
	}
	return messages
}

// splitCursorBlocks splits lines into blocks delimited by
// "user:" or "assistant:" at the left margin, with optional trailing whitespace.
func splitCursorBlocks(lines []string) []cursorBlock {
	var blocks []cursorBlock
	var current *cursorBlock

	for _, line := range lines {
		trimmed := strings.TrimRightFunc(line, unicode.IsSpace)
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

// extractCursorBody reads the producer-specific text grammar, including its
// native structural markers. Normalized dialogue is never parsed this way.
func extractCursorBody(role RoleType, lines []string) (ParsedMessage, time.Time) {
	if role == RoleUser {
		content, timestamp := extractCursorUserContent(lines)
		var builder MessageContentBuilder
		builder.AddText(content)
		body := builder.Message()
		body.ContentLength = sanitizedWorkLength(content)
		return body, timestamp
	}
	return extractCursorAssistantBody(lines), time.Time{}
}

var cursorTimestampPattern = regexp.MustCompile(
	`^<timestamp>(Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday), (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [0-9]{1,2}, [0-9]{4}, [0-9]{1,2}:[0-9]{2} (AM|PM) \(UTC([+-])[0-9]{1,2}(?::[0-9]{2})?\)</timestamp>`,
)

// extractCursorUserContent removes a recognized Cursor metadata tag and
// returns its parsed time without changing the shared user-query helper.
func extractCursorUserContent(lines []string) (string, time.Time) {
	text := strings.Join(lines, "\n")
	trimmed := strings.TrimLeft(text, " \t\r\n")
	match := cursorTimestampPattern.FindStringSubmatch(trimmed)
	if len(match) == 0 {
		return extractUserQuery(lines), time.Time{}
	}

	remaining := strings.TrimLeft(
		trimmed[len(match[0]):], " \t\r\n",
	)
	const (
		userQueryStart = "<user_query>"
		userQueryEnd   = "</user_query>"
	)
	if !strings.HasPrefix(remaining, userQueryStart) {
		return extractUserQuery(lines), time.Time{}
	}
	if strings.Index(remaining, userQueryEnd) < len(userQueryStart) {
		return extractUserQuery(lines), time.Time{}
	}

	timestamp, ok := parseCursorTimestamp(match[0])
	if !ok {
		return extractUserQuery(lines), time.Time{}
	}
	return extractUserQuery(strings.Split(trimmed[len(match[0]):], "\n")), timestamp
}

func parseCursorTimestamp(tag string) (time.Time, bool) {
	match := cursorTimestampPattern.FindStringSubmatch(tag)
	if len(match) == 0 {
		return time.Time{}, false
	}

	dateText := strings.TrimSuffix(
		strings.TrimPrefix(tag, "<timestamp>"),
		"</timestamp>",
	)
	dateText = strings.TrimSuffix(dateText, ")")
	zoneStart := strings.LastIndex(dateText, " (UTC")
	if zoneStart < 0 {
		return time.Time{}, false
	}
	localText := dateText[:zoneStart]
	zoneText := dateText[zoneStart+len(" (UTC"):]
	zoneText = strings.TrimSuffix(zoneText, ")")

	parsed, err := time.Parse("Monday, Jan 2, 2006, 3:04 PM", localText)
	if err != nil {
		return time.Time{}, false
	}

	sign := 1
	if zoneText[0] == '-' {
		sign = -1
	}
	zoneText = zoneText[1:]
	parts := strings.SplitN(zoneText, ":", 2)
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour > 23 {
		return time.Time{}, false
	}
	minute := 0
	if len(parts) == 2 {
		minute, err = strconv.Atoi(parts[1])
		if err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	offset := sign * (hour*60*60 + minute*60)
	return time.Date(
		parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(),
		parsed.Minute(), 0, 0, time.FixedZone("Cursor", offset),
	).UTC(), true
}

func cursorSessionBounds(messages []ParsedMessage, fallback time.Time) (time.Time, time.Time) {
	startedAt, endedAt := fallback, fallback
	found := false
	for _, message := range messages {
		if message.Timestamp.IsZero() {
			continue
		}
		if !found || message.Timestamp.Before(startedAt) {
			startedAt = message.Timestamp
		}
		if !found || message.Timestamp.After(endedAt) {
			endedAt = message.Timestamp
		}
		found = true
	}
	return startedAt, endedAt
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

// extractAssistantContent retains the field-only helper used by grammar tests.
func extractAssistantContent(lines []string) (string, bool, []ParsedToolCall) {
	body := extractCursorAssistantBody(lines)
	return body.Content, body.HasThinking, body.ToolCalls
}

func extractCursorAssistantBody(lines []string) ParsedMessage {
	var builder MessageContentBuilder
	var textLines, run []string
	var toolCalls []ParsedToolCall
	flushText := func() {
		builder.AddText(strings.Join(run, "\n"))
		run = nil
	}

	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])
		if isAssistantMarker(trimmed) {
			flushText()
			markerStart := i
			i++
			bodyStart := i
			for i < len(lines) && !isBlockBodyEnd(lines[i]) {
				i++
			}
			content := strings.TrimSpace(strings.Join(dedentCursorBlock(lines[bodyStart:i]), "\n"))
			switch {
			case strings.HasPrefix(trimmed, "[Thinking]"):
				builder.AddThinking(content)
			case strings.HasPrefix(trimmed, "[Tool call] "):
				name := strings.TrimPrefix(trimmed, "[Tool call] ")
				inputJSON := cursorToolInputJSON(name, lines[bodyStart:i])
				call := ParsedToolCall{
					ToolName: name, Category: NormalizeToolCategory(name), InputJSON: inputJSON,
					SkillName: inferToolSkillName(context.Background(), name, inputJSON),
					Rendering: strings.TrimSpace(strings.Join(lines[markerStart:i], "\n")),
				}
				toolCalls = append(toolCalls, call)
				builder.AddToolCall(call)
			case strings.HasPrefix(trimmed, "[Tool result]"):
				if content == "" {
					continue
				}
				if len(toolCalls) > 0 {
					call := &toolCalls[len(toolCalls)-1]
					call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{Content: content})
				} else {
					raw, _ := json.Marshal(content)
					builder.AddToolResult(ParsedToolResult{ContentRaw: string(raw), ContentLength: len(content)})
				}
			}
			continue
		}
		textLines = append(textLines, lines[i])
		run = append(run, lines[i])
		i++
	}
	flushText()
	body := builder.Message()
	body.setDialogue(strings.TrimSpace(body.Content))
	// The old reader measured dialogue only; retain that work convention.
	body.ContentLength = sanitizedWorkLength(strings.TrimSpace(strings.Join(textLines, "\n")))
	body.ToolCalls = toolCalls
	return body
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
	data, err := json.Marshal(params, json.Deterministic(true))
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

const cursorSessionIDPrefix = "cursor:"

// CursorSessionID derives a session ID from a transcript file
// path by stripping whatever extension is present.
func CursorSessionID(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return cursorSessionIDPrefix + base
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

		body := ExtractMessageContent(context.Background(), content)
		msg := body
		msg.Ordinal = ordinal
		if role == "user" {
			msg.Role = RoleUser
			text, timestamp := extractJSONLUserContent(content)
			msg.setDialogue(text)
			msg.Timestamp = timestamp
		} else {
			msg.Role = RoleAssistant
		}
		msg.trimDialogue()
		if !msg.hasNativeBody() {
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
func extractJSONLUserContent(content gjson.Result) (string, time.Time) {
	if content.Type == gjson.String {
		return extractCursorUserContent(
			strings.Split(content.Str, "\n"),
		)
	}

	if !content.IsArray() {
		return "", time.Time{}
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
		return "", time.Time{}
	}
	combined := strings.Join(parts, "\n")
	return extractCursorUserContent(strings.Split(combined, "\n"))
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
