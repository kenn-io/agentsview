package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Copilot JSONL event types.
const (
	copilotEventSessionStart    = "session.start"
	copilotEventUserMessage     = "user.message"
	copilotEventAssistantMsg    = "assistant.message"
	copilotEventToolComplete    = "tool.execution_complete"
	copilotEventAssistantReason = "assistant.reasoning"
	copilotEventModelChange     = "session.model_change"
)

// copilotSessionBuilder accumulates state while scanning a
// Copilot JSONL session file line by line.
type copilotSessionBuilder struct {
	messages     []ParsedMessage
	firstMessage string
	startedAt    time.Time
	endedAt      time.Time
	sessionID    string
	project      string
	ordinal      int
	currentModel string
}

func newCopilotSessionBuilder() *copilotSessionBuilder {
	return &copilotSessionBuilder{
		project: "unknown",
	}
}

// processLine handles a single non-empty, valid JSON line.
func (b *copilotSessionBuilder) processLine(line string) {
	ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
	if !ts.IsZero() {
		if b.startedAt.IsZero() {
			b.startedAt = ts
		}
		b.endedAt = ts
	}

	data := gjson.Get(line, "data")

	switch gjson.Get(line, "type").Str {
	case copilotEventSessionStart:
		b.handleSessionStart(data)
	case copilotEventUserMessage:
		b.handleUserMessage(data, ts)
	case copilotEventAssistantMsg:
		b.handleAssistantMessage(data, ts)
	case copilotEventToolComplete:
		b.handleToolComplete(data, ts)
	case copilotEventAssistantReason:
		b.handleAssistantReasoning()
	case copilotEventModelChange:
		if v := data.Get("newModel"); v.Exists() {
			b.currentModel = v.Str
		}
	}
}

func (b *copilotSessionBuilder) handleSessionStart(
	data gjson.Result,
) {
	if id := data.Get("sessionId").Str; id != "" {
		b.sessionID = id
	}

	cwd := data.Get("context.cwd").Str
	branch := data.Get("context.branch").Str
	if cwd != "" {
		if p := ExtractProjectFromCwdWithBranch(
			cwd, branch,
		); p != "" {
			b.project = p
		}
	}
}

func (b *copilotSessionBuilder) handleUserMessage(
	data gjson.Result, ts time.Time,
) {
	content := strings.TrimSpace(data.Get("content").Str)
	if content == "" {
		return
	}
	if isCopilotSyntheticSkillMessage(data, content) {
		return
	}

	if b.firstMessage == "" {
		b.firstMessage = truncate(
			strings.ReplaceAll(content, "\n", " "), 300,
		)
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleUser,
		Content:       content,
		Timestamp:     ts,
		ContentLength: len(content),
	})
	b.ordinal++
}

func isCopilotSyntheticSkillMessage(
	data gjson.Result, content string,
) bool {
	source := strings.TrimSpace(data.Get("source").Str)
	if strings.HasPrefix(source, "skill-") {
		return true
	}
	return strings.HasPrefix(content, "<skill-context")
}

func (b *copilotSessionBuilder) handleAssistantMessage(
	data gjson.Result, ts time.Time,
) {
	content := strings.TrimSpace(data.Get("content").Str)
	reasoningText := strings.TrimSpace(data.Get("reasoningText").Str)
	hasThinking := reasoningText != ""

	var toolCalls []ParsedToolCall
	data.Get("toolRequests").ForEach(
		func(_, req gjson.Result) bool {
			name := req.Get("name").Str
			if name == "" {
				return true
			}
			args := req.Get("arguments")
			inputJSON := args.Str
			if args.Type != gjson.String && args.Raw != "" {
				inputJSON = args.Raw
			}
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolUseID: req.Get("toolCallId").Str,
				ToolName:  name,
				Category:  NormalizeToolCategory(name),
				InputJSON: inputJSON,
			})
			return true
		},
	)

	hasToolUse := len(toolCalls) > 0

	// Build display content for tool calls.
	displayContent := content
	if hasToolUse && content == "" {
		displayContent = formatCopilotToolCalls(toolCalls)
	}

	// Prepend thinking block when reasoning text is present.
	if hasThinking {
		thinkBlock := "[Thinking]\n" + reasoningText + "\n[/Thinking]"
		if displayContent != "" {
			displayContent = thinkBlock + "\n\n" + displayContent
		} else {
			displayContent = thinkBlock
		}
	}

	if displayContent == "" && !hasToolUse {
		return
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleAssistant,
		Content:       displayContent,
		Timestamp:     ts,
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(displayContent),
		ToolCalls:     toolCalls,
		Model:         b.currentModel,
	})
	b.ordinal++
}

func (b *copilotSessionBuilder) handleToolComplete(
	data gjson.Result, ts time.Time,
) {
	toolCallID := data.Get("toolCallId").Str
	if toolCallID == "" {
		return
	}

	r := data.Get("result")
	content := r.Str
	if r.Type != gjson.String && r.Raw != "" {
		content = r.Raw
	}
	contentLen := len(content)

	// Emit a tool-result-only user message for pairing.
	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleUser,
		Timestamp:     ts,
		ContentLength: contentLen,
		ToolResults: []ParsedToolResult{{
			ToolUseID:     toolCallID,
			ContentLength: contentLen,
		}},
	})
	b.ordinal++
}

func (b *copilotSessionBuilder) handleAssistantReasoning() {
	// Mark the most recent assistant message as having
	// thinking, if one exists.
	for i := len(b.messages) - 1; i >= 0; i-- {
		if b.messages[i].Role == RoleAssistant {
			b.messages[i].HasThinking = true
			return
		}
	}
}

func formatCopilotToolCalls(
	calls []ParsedToolCall,
) string {
	var parts []string
	for _, tc := range calls {
		parts = append(parts,
			formatToolHeader(tc.Category, tc.ToolName))
	}
	return strings.Join(parts, "\n")
}

// readCopilotWorkspaceName reads the session name from the
// workspace.yaml sibling file in a directory-format session.
// Returns an empty string for flat .jsonl sessions or when
// no name is present.
func readCopilotWorkspaceName(eventsPath string) string {
	if filepath.Base(eventsPath) != "events.jsonl" {
		return ""
	}
	yamlPath := filepath.Join(
		filepath.Dir(eventsPath), "workspace.yaml",
	)
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		after, ok := strings.CutPrefix(line, "name: ")
		if !ok {
			continue
		}
		name := strings.TrimSpace(after)
		if name != "" {
			return truncate(
				strings.ReplaceAll(name, "\n", " "), 300,
			)
		}
	}
	return ""
}

// ParseCopilotSession parses a Copilot JSONL session file.
// Returns (nil, nil, nil) if the file doesn't exist or
// contains no user/assistant messages.
func ParseCopilotSession(
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
	b := newCopilotSessionBuilder()

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		b.processLine(line)
	}

	if err := lr.Err(); err != nil {
		return nil, nil,
			fmt.Errorf("reading copilot %s: %w", path, err)
	}

	// Filter: require at least one user or assistant message.
	hasContent := false
	for _, m := range b.messages {
		if m.Content != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil, nil, nil
	}

	sessionID := b.sessionID
	if sessionID == "" {
		sessionID = sessionIDFromPath(path)
	}
	sessionID = "copilot:" + sessionID

	// Prefer the workspace.yaml name (LLM-generated or user-set
	// title) over the raw first user message. Falls back to the
	// first user message when no name is present.
	firstMessage := b.firstMessage
	if wsName := readCopilotWorkspaceName(path); wsName != "" {
		firstMessage = wsName
	}

	userCount := 0
	for _, m := range b.messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          b.project,
		Machine:          machine,
		Agent:            AgentCopilot,
		FirstMessage:     firstMessage,
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

// sessionIDFromPath extracts a session ID from a Copilot
// file path. Handles both bare (<uuid>.jsonl) and directory
// (<uuid>/events.jsonl) layouts.
func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	if base == "events.jsonl" {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(base, ".jsonl")
}
