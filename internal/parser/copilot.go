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
	copilotEventModelChange     = "session.model_change"
	copilotEventSessionShutdown = "session.shutdown"
	copilotEventUserMessage     = "user.message"
	copilotEventAssistantMsg    = "assistant.message"
	copilotEventToolComplete    = "tool.execution_complete"
	copilotEventAssistantReason = "assistant.reasoning"
)

// copilotSessionBuilder accumulates state while scanning a
// Copilot JSONL session file line by line.
type copilotSessionBuilder struct {
	messages            []ParsedMessage
	firstMessage        string
	startedAt           time.Time
	endedAt             time.Time
	sessionID           string
	project             string
	currentModel        string
	shutdownModelCounts map[string]int64 // accumulated across all session.shutdown events
	ordinal             int
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
	case copilotEventModelChange:
		b.handleModelChange(data)
	case copilotEventSessionShutdown:
		b.handleSessionShutdown(data)
	case copilotEventUserMessage:
		b.handleUserMessage(data, ts)
	case copilotEventAssistantMsg:
		b.handleAssistantMessage(data, ts)
	case copilotEventToolComplete:
		b.handleToolComplete(data, ts)
	case copilotEventAssistantReason:
		b.handleAssistantReasoning()
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

func (b *copilotSessionBuilder) handleModelChange(
	data gjson.Result,
) {
	if m := data.Get("newModel").Str; m != "" {
		b.currentModel = m
	}
}

// handleSessionShutdown accumulates model request counts from
// modelMetrics across all shutdown events in the file (Copilot
// appends a new shutdown on each reconnect). The model with the
// highest total requests across all shutdowns wins.
func (b *copilotSessionBuilder) handleSessionShutdown(
	data gjson.Result,
) {
	// currentModel in the shutdown payload is a secondary
	// fallback: use it only if we haven't learned the model yet.
	if m := data.Get("currentModel").Str; m != "" && b.currentModel == "" {
		b.currentModel = m
	}

	// Accumulate per-model request counts across all shutdowns.
	data.Get("modelMetrics").ForEach(func(key, val gjson.Result) bool {
		if count := val.Get("requests.count").Int(); count > 0 {
			if b.shutdownModelCounts == nil {
				b.shutdownModelCounts = make(map[string]int64)
			}
			b.shutdownModelCounts[key.Str] += count
		}
		return true
	})
}

func (b *copilotSessionBuilder) handleUserMessage(
	data gjson.Result, ts time.Time,
) {
	content := strings.TrimSpace(data.Get("content").Str)
	if content == "" {
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

	// Use the model field to keep currentModel current and to
	// backfill the model on the preceding assistant message
	// (which was appended before the tool result arrived).
	if m := data.Get("model").Str; m != "" {
		b.currentModel = m
		for i := len(b.messages) - 1; i >= 0; i-- {
			if b.messages[i].Role == RoleAssistant {
				if b.messages[i].Model == "" {
					b.messages[i].Model = m
				}
				break
			}
		}
	}
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
		FirstMessage:     b.firstMessage,
		StartedAt:        b.startedAt,
		EndedAt:          b.endedAt,
		MessageCount:     len(b.messages),
		UserMessageCount: userCount,
		MainModel: func() string {
			if len(b.shutdownModelCounts) > 0 {
				var best string
				var bestCount int64
				for model, count := range b.shutdownModelCounts {
					if count > bestCount || (count == bestCount && model < best) {
						best = model
						bestCount = count
					}
				}
				return best
			}
			return ComputeMainModel(b.messages)
		}(),
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
