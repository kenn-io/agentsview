package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// zencoderCwdRe extracts the working directory from a Zencoder
// system message's environment block.
var zencoderCwdRe = regexp.MustCompile(
	`Working directory:\s+(.+)`,
)

// zencoderSessionIDRe extracts a subagent session ID from a
// tool-result text block (e.g. "<session-id>uuid</session-id>").
var zencoderSessionIDRe = regexp.MustCompile(
	`<session-id>([^<]+)</session-id>`,
)

// zencoderSessionBuilder accumulates state while scanning a
// Zencoder JSONL session file line by line.
type zencoderSessionBuilder struct {
	messages        []ParsedMessage
	subagentMap     map[string]string // toolCallId -> "zencoder:<session-id>"
	firstMessage    string
	startedAt       time.Time
	endedAt         time.Time
	sessionID       string
	parentID        string
	creationReason  string
	project         string
	cwd             string
	ordinal         int
	headerProcessed bool
}

func newZencoderSessionBuilder() *zencoderSessionBuilder {
	return &zencoderSessionBuilder{
		subagentMap: map[string]string{},
		project:     "unknown",
	}
}

// processHeader handles the first line of a Zencoder JSONL file.
func (b *zencoderSessionBuilder) processHeader(line string) {
	b.headerProcessed = true
	b.sessionID = gjson.Get(line, "id").Str
	b.parentID = gjson.Get(line, "parentId").Str
	b.creationReason = gjson.Get(line, "creationReason").Str

	createdAt := gjson.Get(line, "createdAt").Str
	if ts := parseTimestamp(createdAt); !ts.IsZero() {
		b.startedAt = ts
	}
	updatedAt := gjson.Get(line, "updatedAt").Str
	if ts := parseTimestamp(updatedAt); !ts.IsZero() {
		b.endedAt = ts
	}
}

// processMessage handles lines 2+ (messages) in a Zencoder
// JSONL file.
func (b *zencoderSessionBuilder) processMessage(line string) {
	role := gjson.Get(line, "role").Str
	ts := parseTimestamp(gjson.Get(line, "createdAt").Str)

	// Update session-level time bounds from per-message
	// timestamps. This covers cases where header timestamps
	// are missing or stale.
	if !ts.IsZero() {
		if b.startedAt.IsZero() || ts.Before(b.startedAt) {
			b.startedAt = ts
		}
		if ts.After(b.endedAt) {
			b.endedAt = ts
		}
	}

	switch role {
	case "system":
		b.handleSystemMessage(line, ts)
	case "user":
		b.handleUserMessage(line, ts)
	case "assistant":
		b.handleAssistantMessage(line, ts)
	case "tool":
		b.handleToolMessage(line, ts)
	case "finish":
		reason := gjson.Get(line, "reason").Str
		if reason == "" {
			reason = "unknown"
		}
		content := "[Turn finished: " + reason + "]"
		b.messages = append(b.messages, ParsedMessage{
			Ordinal:       b.ordinal,
			Role:          RoleUser,
			IsSystem:      true,
			Content:       content,
			ContentLength: len(content),
			Timestamp:     ts,
		})
		b.ordinal++
		// "permission" -- skip entirely.
	}
}

func (b *zencoderSessionBuilder) handleSystemMessage(
	line string, ts time.Time,
) {
	// System content is a plain string (not array).
	content := gjson.Get(line, "content").Str
	if content == "" {
		return
	}

	// Extract project from "Working directory: /path".
	if m := zencoderCwdRe.FindStringSubmatch(content); len(m) > 1 {
		cwd := strings.TrimSpace(m[1])
		if b.cwd == "" {
			b.cwd = cwd
		}
		if proj := ExtractProjectFromCwd(cwd); proj != "" {
			b.project = proj
		}
	}

	// Store the system message for display.
	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleUser,
		IsSystem:      true,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     ts,
	})
	b.ordinal++
}

func (b *zencoderSessionBuilder) handleUserMessage(
	line string, ts time.Time,
) {
	userContent, systemContent := extractZencoderUserContent(
		gjson.Get(line, "content"),
	)

	if strings.TrimSpace(userContent) != "" {
		if b.firstMessage == "" {
			b.firstMessage = truncate(
				strings.ReplaceAll(userContent, "\n", " "), 300,
			)
		}

		b.messages = append(b.messages, ParsedMessage{
			Ordinal:       b.ordinal,
			Role:          RoleUser,
			Content:       userContent,
			ContentLength: len(userContent),
			Timestamp:     ts,
		})
		b.ordinal++
	}

	if strings.TrimSpace(systemContent) != "" {
		b.messages = append(b.messages, ParsedMessage{
			Ordinal:       b.ordinal,
			Role:          RoleUser,
			IsSystem:      true,
			Content:       systemContent,
			ContentLength: len(systemContent),
			Timestamp:     ts,
		})
		b.ordinal++
	}
}

func (b *zencoderSessionBuilder) handleAssistantMessage(
	line string, ts time.Time,
) {
	content, hasThinking, hasToolUse, tcs :=
		extractZencoderAssistantContent(
			gjson.Get(line, "content"),
		)

	if strings.TrimSpace(content) == "" && !hasToolUse {
		return
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleAssistant,
		Content:       content,
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(content),
		ToolCalls:     tcs,
		Timestamp:     ts,
	})
	b.ordinal++
}

func (b *zencoderSessionBuilder) handleToolMessage(
	line string, ts time.Time,
) {
	var toolResults []ParsedToolResult
	var systemParts []string
	gjson.Get(line, "content").ForEach(
		func(_, block gjson.Result) bool {
			if block.Get("type").Str != "tool-result" {
				return true
			}
			toolCallID := block.Get("toolCallId").Str
			if toolCallID == "" {
				return true
			}

			// Track which content blocks are tagged (system)
			// so we can strip them from ContentRaw.
			var taggedIndices []int

			// Extract <session-id> from tool-result content
			// to map subagent tool calls to their sessions.
			// Also collect system-tagged text blocks.
			idx := 0
			block.Get("content").ForEach(
				func(_, cb gjson.Result) bool {
					if m := zencoderSessionIDRe.FindStringSubmatch(
						cb.Get("text").Str,
					); len(m) > 1 {
						b.subagentMap[toolCallID] =
							"zencoder:" + m[1]
					}
					if tag := cb.Get("tag").Str; tag != "" &&
						cb.Get("type").Str == "text" {
						if text := cb.Get("text").Str; text != "" {
							systemParts = append(systemParts, text)
							taggedIndices = append(taggedIndices, idx)
						}
					}
					idx++
					return true
				},
			)

			// Build filtered content that excludes tagged
			// blocks to avoid rendering them twice (once in
			// tool output via ContentRaw, once as a separate
			// system message).
			contentRaw := block.Get("content").Raw
			if len(taggedIndices) > 0 {
				contentRaw = filterJSONArrayIndices(
					block.Get("content"), taggedIndices,
				)
			}

			cl := zencoderToolResultContentLength(
				gjson.Parse(contentRaw),
			)
			toolResults = append(toolResults, ParsedToolResult{
				ToolUseID:     toolCallID,
				ContentLength: cl,
				ContentRaw:    contentRaw,
			})

			return true
		},
	)

	if len(toolResults) == 0 {
		return
	}

	totalLen := 0
	for _, tr := range toolResults {
		totalLen += tr.ContentLength
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleUser,
		ContentLength: totalLen,
		ToolResults:   toolResults,
		Timestamp:     ts,
	})
	b.ordinal++

	if len(systemParts) > 0 {
		sysContent := strings.Join(systemParts, "\n")
		b.messages = append(b.messages, ParsedMessage{
			Ordinal:       b.ordinal,
			Role:          RoleUser,
			IsSystem:      true,
			Content:       sysContent,
			ContentLength: len(sysContent),
			Timestamp:     ts,
		})
		b.ordinal++
	}
}

// extractZencoderUserContent extracts text from a Zencoder user
// message content array, filtering by tag. Returns user text
// (blocks with tag "" or "user-input") and system text (blocks
// with tags like "instructions", "system-reminder", "todo-reminder").
func extractZencoderUserContent(
	content gjson.Result,
) (userText, systemText string) {
	if !content.IsArray() {
		return "", ""
	}

	var userParts, systemParts []string
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			text := block.Get("text").Str
			if text == "" {
				return true
			}
			tag := block.Get("tag").Str
			if tag == "" || tag == "user-input" {
				userParts = append(userParts, text)
			} else {
				systemParts = append(systemParts, text)
			}
		case "skill":
			name := block.Get("name").Str
			body := block.Get("content").Str
			if name != "" {
				systemParts = append(systemParts,
					"[Skill: "+name+"]\n"+body+"\n[/Skill]")
			}
		default:
			return true
		}
		return true
	})
	return strings.Join(userParts, "\n"),
		strings.Join(systemParts, "\n")
}

// extractZencoderAssistantContent extracts text, thinking, and
// tool calls from a Zencoder assistant message content array.
func extractZencoderAssistantContent(
	content gjson.Result,
) (string, bool, bool, []ParsedToolCall) {
	if !content.IsArray() {
		return "", false, false, nil
	}

	var (
		parts       []string
		toolCalls   []ParsedToolCall
		hasThinking bool
		hasToolUse  bool
	)
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			if text := block.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
		case "reasoning":
			if text := block.Get("text").Str; text != "" {
				hasThinking = true
				parts = append(parts,
					"[Thinking]\n"+text+"\n[/Thinking]")
			}
		case "tool-call":
			hasToolUse = true
			name := block.Get("toolName").Str
			if name == "" {
				return true
			}
			tc := ParsedToolCall{
				ToolUseID: block.Get("toolCallId").Str,
				ToolName:  name,
				Category:  NormalizeToolCategory(name),
				InputJSON: block.Get("input").Raw,
			}
			toolCalls = append(toolCalls, tc)
			// Synthesize a Claude-compatible JSON block for
			// formatToolUse, which expects "name" and "input".
			synth := fmt.Sprintf(
				`{"name":%q,"input":%s}`,
				name,
				orDefault(block.Get("input").Raw, "{}"),
			)
			parts = append(parts,
				formatToolUse(gjson.Parse(synth)))
		}
		return true
	})

	return strings.Join(parts, "\n"),
		hasThinking, hasToolUse, toolCalls
}

// filterJSONArrayIndices returns a JSON array string with
// elements at the given indices removed. The indices must be
// sorted ascending (as produced by iteration order).
func filterJSONArrayIndices(
	arr gjson.Result, exclude []int,
) string {
	if !arr.IsArray() {
		return arr.Raw
	}
	excludeSet := make(map[int]struct{}, len(exclude))
	for _, i := range exclude {
		excludeSet[i] = struct{}{}
	}
	var parts []string
	idx := 0
	arr.ForEach(func(_, el gjson.Result) bool {
		if _, skip := excludeSet[idx]; !skip {
			parts = append(parts, el.Raw)
		}
		idx++
		return true
	})
	return "[" + strings.Join(parts, ",") + "]"
}

// zencoderToolResultContentLength computes the total text
// length from a tool-result's content array.
func zencoderToolResultContentLength(
	content gjson.Result,
) int {
	if !content.IsArray() {
		return 0
	}
	total := 0
	content.ForEach(func(_, block gjson.Result) bool {
		// Various content types: text, text-file-chunk,
		// shell-result, etc. all may have a "text" field.
		total += len(block.Get("text").Str)
		return true
	})
	return total
}

func parseZencoderSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	b := newZencoderSessionBuilder()

	lineNum := 0
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			lineNum++
			continue
		}

		if lineNum == 0 {
			b.processHeader(line)
		} else {
			b.processMessage(line)
		}
		lineNum++
	}

	if err := lr.Err(); err != nil {
		return nil, nil,
			fmt.Errorf("reading zencoder %s: %w", path, err)
	}

	// Filter: require at least one non-system user or assistant
	// message. Files with only headers and system blocks (e.g.
	// environment banners) are not real conversations.
	hasContent := false
	for _, m := range b.messages {
		if m.Content != "" && !m.IsSystem {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil, nil, nil
	}

	sessionID := b.sessionID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(
			filepath.Base(path), ".jsonl",
		)
	}
	sessionID = "zencoder:" + sessionID

	// Map creationReason to RelationshipType.
	var relType RelationshipType
	var parentSessionID string
	if b.parentID != "" {
		switch b.creationReason {
		case "directContinuation",
			"summarizedContinuation",
			"summarizationRequest":
			relType = RelContinuation
			parentSessionID = "zencoder:" + b.parentID
		default:
			// "newChat" or unknown -- no relationship.
		}
	}

	// Annotate tool calls with their subagent session IDs.
	annotateSubagentSessions(b.messages, b.subagentMap)

	userCount := 0
	for _, m := range b.messages {
		if m.Role == RoleUser && m.Content != "" && !m.IsSystem {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          b.project,
		Machine:          machine,
		Agent:            AgentZencoder,
		ParentSessionID:  parentSessionID,
		RelationshipType: relType,
		Cwd:              b.cwd,
		FirstMessage:     b.firstMessage,
		StartedAt:        b.startedAt,
		EndedAt:          b.endedAt,
		MessageCount:     len(b.messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, b.messages, nil
}

// IsZencoderSessionFileName reports whether name matches
// the Zencoder session file pattern (*.jsonl).
func IsZencoderSessionFileName(name string) bool {
	return strings.HasSuffix(name, ".jsonl")
}
