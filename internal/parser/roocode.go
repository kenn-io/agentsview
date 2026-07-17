// ABOUTME: Parses RooCode (RooVeterinaryInc.roo-cline) VSCode extension
// ABOUTME: session files from the tasks/ directory under VSCode globalStorage.
// ABOUTME: Each session is a task directory with history_item.json (metadata)
// ABOUTME: and ui_messages.json (ClineMessage[] transcript). The parser extracts
// ABOUTME: message roles from the type/say/ask fields, skips partial messages,
// ABOUTME: and derives project name, model name, session name, and token counts
// ABOUTME: from history_item.json.
package parser

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// rooCodeHistoryItem mirrors the HistoryItem in history_item.json.
type rooCodeHistoryItem struct {
	ID                      string   `json:"id"`
	RootTaskID              string   `json:"rootTaskId,omitempty"`
	ParentTaskID            string   `json:"parentTaskId,omitempty"`
	Number                  int      `json:"number"`
	Timestamp               int64    `json:"ts"`
	Task                    string   `json:"task"`
	TokensIn                int      `json:"tokensIn"`
	TokensOut               int      `json:"tokensOut"`
	CacheWrites             int      `json:"cacheWrites,omitempty"`
	CacheReads              int      `json:"cacheReads,omitempty"`
	TotalCost               float64  `json:"totalCost"`
	Size                    int64    `json:"size,omitempty"`
	Workspace               string   `json:"workspace,omitempty"`
	Mode                    string   `json:"mode,omitempty"`
	APIConfigName           string   `json:"apiConfigName,omitempty"`
	Status                  string   `json:"status,omitempty"`
	DelegatedToID           string   `json:"delegatedToId,omitempty"`
	ChildIDs                []string `json:"childIds,omitempty"`
	AwaitingChildID         string   `json:"awaitingChildId,omitempty"`
	CompletedByChildID      string   `json:"completedByChildId,omitempty"`
	CompletionResultSummary string   `json:"completionResultSummary,omitempty"`
}

// rooCodeMessage mirrors the ClineMessage in ui_messages.json.
type rooCodeMessage struct {
	Timestamp int64    `json:"ts"`
	Type      string   `json:"type"` // "ask" or "say"
	Ask       string   `json:"ask,omitempty"`
	Say       string   `json:"say,omitempty"`
	Text      string   `json:"text,omitempty"`
	Images    []string `json:"images,omitempty"`
	Partial   bool     `json:"partial,omitempty"`
	Reasoning string   `json:"reasoning,omitempty"`
}

// parseRooCodeSession parses a single RooCode task directory and returns the
// parsed session with messages. The task directory must contain
// history_item.json and may contain ui_messages.json.
func parseRooCodeSession(
	taskDir string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	historyPath := filepath.Join(taskDir, "history_item.json")
	messagesPath := filepath.Join(taskDir, "ui_messages.json")

	// Read history_item.json for session metadata.
	historyData, err := os.ReadFile(historyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading history_item.json: %w", err)
	}

	var historyItem rooCodeHistoryItem
	if err := json.Unmarshal(historyData, &historyItem); err != nil {
		return nil, nil, fmt.Errorf("parsing history_item.json: %w", err)
	}

	// Model name from the API config name (provider profile).
	model := historyItem.APIConfigName

	// Read and parse ui_messages.json (may not exist for empty sessions).
	parsedMessages, peakCtx, err := parseRooCodeMessages(messagesPath, model)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("parsing ui_messages.json: %w", err)
	}

	// Build session ID: "roocode:" + task ID.
	sessionID := string(AgentRooCode) + ":" + historyItem.ID

	// Parse startedAt from the history item timestamp (ms since epoch).
	startedAt := time.UnixMilli(historyItem.Timestamp)

	// Determine endedAt from the last message timestamp.
	var endedAt time.Time
	for _, msg := range parsedMessages {
		if msg.Timestamp.After(endedAt) {
			endedAt = msg.Timestamp
		}
	}

	// Count user messages (non-system user role with non-empty content).
	userMsgCount := 0
	for _, msg := range parsedMessages {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
		}
	}

	// Build session name from the task description (first prompt).
	firstMsg := ""
	for _, msg := range parsedMessages {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			firstMsg = truncate(
				strings.ReplaceAll(msg.Content, "\n", " "),
				300,
			)
			break
		}
	}

	sessionName := historyItem.Task
	if sessionName == "" {
		sessionName = firstMsg
	}
	if len(sessionName) > 80 {
		sessionName = sessionName[:77] + "..."
	}
	if sessionName == "" {
		sessionName = projectHint
	}

	// Derive project from workspace path using git-root aware extraction.
	project := projectHint
	if historyItem.Workspace != "" {
		if p := ExtractProjectFromCwd(historyItem.Workspace); p != "" {
			project = p
		}
	}

	// Source file identity: use history_item.json as the primary source.
	// The fingerprint includes both history_item.json and ui_messages.json.
	info, err := os.Stat(historyPath)
	fileInfo := FileInfo{
		Path: historyPath,
	}
	if err == nil {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	// Include ui_messages.json size in source file size for freshness.
	if msgInfo, err := os.Stat(messagesPath); err == nil {
		fileInfo.Size += msgInfo.Size()
		if msgMtime := msgInfo.ModTime().UnixNano(); msgMtime > fileInfo.Mtime {
			fileInfo.Mtime = msgMtime
		}
	}

	messageCount := len(parsedMessages)

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentRooCode,
		Cwd:              historyItem.Workspace,
		FirstMessage:     firstMsg,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     messageCount,
		UserMessageCount: userMsgCount,
		SourceSessionID:  historyItem.ID,
		SourceVersion:    "roocode-task-v1",
		File:             fileInfo,
	}

	// Wire up parent-child task tree. RooCode supports subtask
	// delegation via parentTaskId and childIds fields.
	if historyItem.ParentTaskID != "" {
		sess.ParentSessionID = string(AgentRooCode) + ":" + historyItem.ParentTaskID
		sess.RelationshipType = RelSubagent
	}

	// Link newTask tool calls to their child sessions so the
	// frontend can render inline subagent content. RooCode
	// appends childIds chronologically, matching newTask message
	// order in ui_messages.json.
	if len(historyItem.ChildIDs) > 0 {
		childIdx := 0
		for mi := range parsedMessages {
			for ci := range parsedMessages[mi].ToolCalls {
				if parsedMessages[mi].ToolCalls[ci].ToolName == "newTask" &&
					childIdx < len(historyItem.ChildIDs) {
					parsedMessages[mi].ToolCalls[ci].SubagentSessionID =
						string(AgentRooCode) + ":" +
							historyItem.ChildIDs[childIdx]
					childIdx++
				}
			}
		}
	}

	// Token counts from history item.
	//
	// history_item's tokensIn and tokensOut are CUMULATIVE totals
	// across all API requests. We map tokensOut to TotalOutputTokens
	// (cumulative output) and use the peak from api_req_started
	// entries (extracted above) for PeakContextTokens. Cumulative
	// tokensIn is still reported through the usage event's
	// InputTokens field for cost accounting.
	if peakCtx > 0 {
		sess.PeakContextTokens = peakCtx
		sess.HasPeakContextTokens = true
	}
	if historyItem.TokensOut > 0 {
		sess.TotalOutputTokens = historyItem.TokensOut
		sess.HasTotalOutputTokens = true
	}
	sess.aggregateTokenPresenceKnown =
		sess.HasTotalOutputTokens || sess.HasPeakContextTokens

	// Classify termination status from history_item status and
	// message content. Maps RooCode's raw status to the standard
	// termination vocabulary used by all agents. Always call
	// classifyRooCodeTermination even when status is empty — it
	// detects orphaned tool calls and thinking-only endings.
	sess.TerminationStatus = classifyRooCodeTermination(
		historyItem.Status, parsedMessages,
	)

	// Emit usage event with model name, token counts, and recorded cost
	// for catalog-based pricing. Emit whenever accounting data exists;
	// model may be absent when apiConfigName is omitted.
	if historyItem.TokensIn > 0 || historyItem.TokensOut > 0 ||
		historyItem.CacheReads > 0 || historyItem.CacheWrites > 0 ||
		historyItem.TotalCost > 0 {
		event := ParsedUsageEvent{
			SessionID: sessionID,
			Source:    "session",
			Model:     model,
			OccurredAt: func() string {
				if !endedAt.IsZero() {
					return endedAt.Format(time.RFC3339Nano)
				}
				return startedAt.Format(time.RFC3339Nano)
			}(),
			DedupKey: "session:" + sessionID,
		}
		if historyItem.TokensIn > 0 {
			event.InputTokens = historyItem.TokensIn
		}
		if historyItem.TokensOut > 0 {
			event.OutputTokens = historyItem.TokensOut
		}
		if historyItem.CacheReads > 0 {
			event.CacheReadInputTokens = historyItem.CacheReads
		}
		if historyItem.CacheWrites > 0 {
			event.CacheCreationInputTokens = historyItem.CacheWrites
		}
		if historyItem.TotalCost > 0 {
			cost := historyItem.TotalCost
			event.CostUSD = &cost
		}
		sess.UsageEvents = []ParsedUsageEvent{event}
	}

	return sess, parsedMessages, nil
}

// parseRooCodeMessages reads and parses ui_messages.json into ParsedMessages.
// Returns nil, nil, 0 when the file does not exist (empty sessions).
// Also returns peakContextTokens: the maximum per-request tokensIn from
// api_req_started entries, representing the peak context window size.
//
// Message attribution in RooCode/Cline:
//   - Message [0] (first in transcript) is the user's initial task prompt,
//     recorded as type="say" say="text".
//   - All subsequent messages are from the assistant. The say/ask distinction
//     describes HOW the assistant communicates: "say" is the assistant telling
//     the user something, "ask" is the assistant requesting something (tool
//     approval, followup question, etc.).
//   - "command_output" messages carry tool/command execution results and are
//     attributed as user-system messages with tool results.
//   - "api_req_started" and "checkpoint_saved" are internal metadata and are
//     skipped.
func parseRooCodeMessages(path string, model string) ([]ParsedMessage, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}

	var rawMessages []json.RawMessage
	if err := json.Unmarshal(data, &rawMessages); err != nil {
		// Try parsing as a single message.
		var single rooCodeMessage
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return nil, 0, fmt.Errorf("parsing ui_messages.json: %w", err)
		}
		rawMessages = []json.RawMessage{data}
	}

	parsedMessages := make([]ParsedMessage, 0, len(rawMessages))
	ordinal := 0
	isFirst := true
	peakCtx := 0
	// pendingCmdMsgIdx tracks the index in parsedMessages of the
	// most recent execute_command tool call awaiting a result.
	// -1 means no pending command.
	pendingCmdMsgIdx := -1
	pendingMcpMsgIdx := -1
	for _, rawMsg := range rawMessages {
		var msg rooCodeMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue // Skip malformed messages.
		}

		// Skip partial messages.
		if msg.Partial {
			continue
		}

		// Skip internal metadata messages, but extract peak
		// context from api_req_started before skipping.
		if rooCodeIsMetadataSay(msg.Say) {
			if msg.Say == "api_req_started" && msg.Text != "" {
				var reqData map[string]any
				if json.Unmarshal([]byte(msg.Text), &reqData) == nil {
					ctx := floatInt(reqData["tokensIn"]) +
						floatInt(reqData["cacheReads"]) +
						floatInt(reqData["cacheWrites"])
					if ctx > peakCtx {
						peakCtx = ctx
					}
				}
			}
			continue
		}

		ts := time.UnixMilli(msg.Timestamp)

		// Determine role and extract tool calls / tool results.
		role, toolCalls, toolResults := classifyRooCodeMessage(
			msg, isFirst,
		)
		isFirst = false

		// Extract content and reasoning.
		// In real RooCode data, reasoning text is in the text field
		// when say="reasoning". Move it into the reasoning pipeline
		// so it renders as a collapsible thinking block.
		content := strings.TrimSpace(msg.Text)
		reasoning := strings.TrimSpace(msg.Reasoning)
		if msg.Say == "reasoning" && reasoning == "" && content != "" {
			reasoning = content
			content = ""
		}

		// Tool call messages carry JSON metadata or command strings
		// in the text field, not conversation content. Clear content
		// so we emit only the tool call message.
		if len(toolCalls) > 0 {
			content = ""
		}

		// Context management events (condense_context, sliding_window_truncation)
		// are compaction boundaries even when their text field is empty.
		// Emit them as minimal system messages with IsCompactBoundary so
		// the signals system counts them for CompactionCount.
		if rooCodeIsCompactBoundary(msg.Say) {
			parsedMessages = append(parsedMessages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleSystem,
				Content:           content,
				IsSystem:          true,
				IsCompactBoundary: true,
				Model:             model,
				Timestamp:         ts,
			})
			ordinal++
			continue
		}

		// Skip messages with no content, no reasoning, and no tool
		// calls/results.
		if content == "" && reasoning == "" && len(toolCalls) == 0 &&
			len(toolResults) == 0 {
			continue
		}

		// If there's reasoning text, emit it as a thinking message first.
		if reasoning != "" {
			parsedMessages = append(parsedMessages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       "[Thinking]\n" + reasoning + "\n[/Thinking]",
				ThinkingText:  reasoning,
				HasThinking:   true,
				Model:         model,
				Timestamp:     ts,
				ContentLength: len(reasoning),
			})
			ordinal++
		}

		hasToolCalls := len(toolCalls) > 0
		hasToolResults := len(toolResults) > 0

		// Handle command_output by pairing it with the preceding
		// execute_command tool call. Instead of emitting a standalone
		// system message, update the tool call's ResultContent and
		// ResultEvents so the signals system can detect failures.
		if msg.Say == "command_output" && hasToolResults {
			output := toolResults[0].ContentRaw
			paired := false
			if pendingCmdMsgIdx >= 0 &&
				pendingCmdMsgIdx < len(parsedMessages) {
				target := &parsedMessages[pendingCmdMsgIdx]
				for ci := range target.ToolCalls {
					tc := &target.ToolCalls[ci]
					status := "completed"
					if rooCommandOutputIsError(output) {
						status = "errored"
					}
					tc.ResultEvents = append(
						tc.ResultEvents,
						ParsedToolResultEvent{
							Status:  status,
							Content: output,
						},
					)
				}
				pendingCmdMsgIdx = -1
				paired = true
			}
			if !paired && output != "" {
				// No pending command to pair with — emit as
				// standalone system message (fallback).
				parsedMessages = append(parsedMessages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleUser,
					Content:       output,
					Model:         model,
					IsSystem:      true,
					Timestamp:     ts,
					ContentLength: len(output),
					ToolResults:   toolResults,
				})
				ordinal++
			}
			continue
		}

		// Handle mcp_server_response by pairing it with the preceding
		// use_mcp_server tool call. The MCP response text contains the
		// server's result (search results, data, etc.).
		if msg.Say == "mcp_server_response" {
			paired := false
			if pendingMcpMsgIdx >= 0 &&
				pendingMcpMsgIdx < len(parsedMessages) {
				target := &parsedMessages[pendingMcpMsgIdx]
				for ci := range target.ToolCalls {
					tc := &target.ToolCalls[ci]
					tc.ResultEvents = append(
						tc.ResultEvents,
						ParsedToolResultEvent{
							Status:  "completed",
							Content: content,
						},
					)
				}
				pendingMcpMsgIdx = -1
				paired = true
			}
			if !paired && content != "" {
				// No pending MCP tool to pair with — emit as
				// standalone system message (fallback).
				parsedMessages = append(parsedMessages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleSystem,
					Content:       content,
					Model:         model,
					IsSystem:      true,
					Timestamp:     ts,
					ContentLength: len(content),
				})
				ordinal++
			}
			continue
		}

		if hasToolCalls {
			// Emit assistant message with tool calls.
			parsedMessages = append(parsedMessages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       "",
				ToolCalls:     toolCalls,
				HasToolUse:    true,
				Model:         model,
				Timestamp:     ts,
				ContentLength: 0,
			})
			// Track tool calls that produce deferred results
			// (command_output, mcp_server_response) so we can pair
			// results back when they arrive.
			switch toolCalls[0].ToolName {
			case "execute_command":
				pendingCmdMsgIdx = len(parsedMessages) - 1
			default:
				// Track any MCP tool call (Category="MCP") for
				// pairing with mcp_server_response.
				if toolCalls[0].Category == "MCP" {
					pendingMcpMsgIdx = len(parsedMessages) - 1
				}
			}
			ordinal++
		} else if content != "" {
			// Tool error events (mistake_limit_reached, api_req_failed)
			// indicate the agent hit a failure limit. Link to the
			// preceding tool call (execute_command or MCP) as errored.
			// Skip standalone emission when pairing succeeds.
			if rooCodeIsToolErrorEvent(msg.Say, msg.Ask) {
				if rooPairErrorToPendingTool(
					parsedMessages, &pendingCmdMsgIdx, &pendingMcpMsgIdx, content,
				) {
					continue
				}
			}

			// Error say types (error, diff_error, rooignore_error)
			// indicate a tool failure. Pair them with the preceding
			// unpaired execute_command tool call so the signals
			// system counts them as failures. Skip standalone emission
			// when pairing succeeds.
			if rooCodeIsErrorSay(msg.Say) {
				if rooPairErrorToPendingTool(
					parsedMessages, &pendingCmdMsgIdx, &pendingMcpMsgIdx, content,
				) {
					continue
				}
			}

			// Regular message.
			parsedMessages = append(parsedMessages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          role,
				Content:       content,
				Model:         model,
				IsSystem:      role == RoleSystem,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++
		}
	}

	return parsedMessages, peakCtx, nil
}

// classifyRooCodeMessage determines the role, tool calls, and tool results
// for a single RooCode message. The isFirst flag is true only for the very
// first message in the transcript, which carries the user's initial task.
func classifyRooCodeMessage(
	msg rooCodeMessage, isFirst bool,
) (RoleType, []ParsedToolCall, []ParsedToolResult) {
	say := msg.Say
	ask := msg.Ask

	// Message [0] is always the user's task.
	if isFirst {
		return RoleUser, nil, nil
	}

	// --- Assistant messages (say) ---
	switch say {
	case "text":
		return RoleAssistant, nil, nil
	case "reasoning":
		// Reasoning text (in msg.Text) is redirected into the
		// thinking pipeline by parseRooCodeMessages when the
		// Reasoning struct field is empty.
		return RoleAssistant, nil, nil
	case "command_output":
		// Command/tool execution result. parseRooCodeMessages
		// pairs this with the preceding execute_command tool call
		// by returning the output as a ParsedToolResult.
		content := strings.TrimSpace(msg.Text)
		if content == "" {
			return RoleUser, nil, nil
		}
		return RoleUser, nil, []ParsedToolResult{{
			ContentLength: len(content),
			ContentRaw:    content,
		}}
	case "completion_result":
		return RoleAssistant, nil, nil
	case "subtask_result":
		// Result from a delegated child task. Treat as system.
		return RoleSystem, nil, nil
	case "user_feedback", "user_feedback_diff":
		// User feedback on the assistant's work.
		return RoleUser, nil, nil
	case "error", "diff_error", "rooignore_error",
		"shell_integration_warning":
		// Error and warning messages. Treat as system.
		return RoleSystem, nil, nil
	case "mcp_server_response":
		// MCP server tool response.
		return RoleSystem, nil, nil
	case "condense_context", "condense_context_error",
		"sliding_window_truncation":
		// Context management events. Treat as system.
		return RoleSystem, nil, nil
	case "codebase_search_result":
		// Codebase search result from an internal tool.
		return RoleAssistant, nil, nil
	}

	// --- Assistant requests (ask) ---
	switch ask {
	case "tool":
		// Assistant invokes a tool. The text field carries a JSON
		// payload with tool name, path, etc. For file tools
		// (readFile, appliedDiff, etc.), the result content is
		// embedded in the payload's "content" field.
		tc := parseRooCodeToolCall(msg.Text)
		if tc == nil {
			return RoleAssistant, nil, nil
		}
		// Extract embedded result content for tools whose schemas
		// define "content" as result data (e.g. readFile). Write/
		// edit tools use "content" as input and are excluded.
		if resultContent := rooToolResultContent(
			tc.ToolName, msg.Text,
		); resultContent != "" {
			tc.ResultEvents = append(tc.ResultEvents,
				ParsedToolResultEvent{
					Status:  "completed",
					Content: resultContent,
				},
			)
		}
		// newTask delegates work to a child session; classify it
		// as a Task category so the frontend's isTask gate
		// renders the inline SubagentInline component.
		if tc.ToolName == "newTask" {
			tc.Category = "Task"
		}
		return RoleAssistant, []ParsedToolCall{*tc}, nil
	case "command":
		// Assistant invokes a shell command.
		cmdText := strings.TrimSpace(msg.Text)
		if cmdText == "" {
			return RoleAssistant, nil, nil
		}
		return RoleAssistant, []ParsedToolCall{{
			ToolUseID: "roocode:execute_command",
			ToolName:  "execute_command",
			InputJSON: cmdText,
		}}, nil
	case "completion_result":
		return RoleAssistant, nil, nil
	case "use_mcp_server":
		// Assistant invokes an MCP server tool. The text field
		// carries JSON with server name and tool details.
		tc := parseRooCodeToolCall(msg.Text)
		if tc == nil {
			return RoleAssistant, nil, nil
		}
		tc.Category = "MCP"
		return RoleAssistant, []ParsedToolCall{*tc}, nil
	case "followup":
		// Assistant asks the user a followup question.
		return RoleAssistant, nil, nil
	}

	// Fallback: assistant messages are from the assistant.
	return RoleAssistant, nil, nil
}

// classifyRooCodeTermination maps RooCode's raw status string
// (from history_item.json) to the standard TerminationStatus
// vocabulary. It inspects the parsed messages for incomplete endings
// (orphaned tool calls, thinking-only blocks) regardless of the
// status field. Truly completed sessions must end with a proper
// assistant response (text, completion_result, etc.).
//
// Mapping:
//   - orphaned tool call → TerminationToolCallPending
//   - thinking-only last msg → TerminationToolCallPending
//   - "error"   → TerminationTruncated (session aborted)
//   - "completed" + normal → TerminationClean
//   - anything else (including "active") → "" (leave NULL)
func classifyRooCodeTermination(
	status string, messages []ParsedMessage,
) TerminationStatus {
	// Always check for incomplete endings regardless of status.
	// Truly completed sessions should have a final assistant response,
	// not just thinking content or unresolved tool calls.
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	if rooLastMessageIsThinkingOnly(messages) {
		return TerminationToolCallPending
	}
	switch status {
	case "error":
		return TerminationTruncated
	case "completed":
		return TerminationClean
	}
	return ""
}

// rooLastMessageIsThinkingOnly reports whether the last
// non-system message is an assistant message containing only
// thinking/reasoning content with no actual response text or
// tool calls. This is a strong signal that the session was
// interrupted mid-thought.
func rooLastMessageIsThinkingOnly(messages []ParsedMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.IsSystem {
			continue
		}
		if m.Role != RoleAssistant {
			return false
		}
		if !m.HasThinking {
			return false
		}
		if len(m.ToolCalls) > 0 {
			return false
		}
		return IsThinkingOnlyContent(m.Content)
	}
	return false
}

// floatInt extracts a float64-as-int from a JSON-decoded map value,
// returning 0 when the value is absent or not a number.
func floatInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// rooCodeIsMetadataSay reports whether the say type is internal metadata
// that should be skipped during transcript parsing.
func rooCodeIsMetadataSay(say string) bool {
	switch say {
	case "api_req_started", "api_req_deleted",
		"api_req_retried", "api_req_retry_delayed",
		"checkpoint_saved", "mcp_server_request_started":
		return true
	}
	return false
}

// rooCodeIsCompactBoundary reports whether the say type is a
// context management event that should be emitted as a compact
// boundary message for CompactionCount tracking.
func rooCodeIsCompactBoundary(say string) bool {
	switch say {
	case "condense_context", "sliding_window_truncation":
		return true
	}
	return false
}

// rooCodeIsErrorSay reports whether the say type is an error
// diagnostic that should be linked to the preceding tool call
// as a failure signal. shell_integration_warning is excluded
// because it's a non-fatal warning, not a tool failure.
func rooCodeIsErrorSay(say string) bool {
	switch say {
	case "error", "diff_error", "rooignore_error":
		return true
	}
	return false
}

// rooCodeIsToolErrorEvent reports whether the ask/say type indicates
// a tool or API failure that should be linked to the preceding
// tool call as an errored ResultEvent.
func rooCodeIsToolErrorEvent(say, ask string) bool {
	switch say {
	case "mistake_limit_reached", "api_req_failed":
		return true
	}
	switch ask {
	case "mistake_limit_reached", "api_req_failed":
		return true
	}
	return false
}

// rooPairErrorToPendingTool links an error event to the preceding
// pending tool call (execute_command first, then MCP) as an errored
// ResultEvent. Clears the pending index after pairing.
// Returns true if pairing succeeded.
func rooPairErrorToPendingTool(
	parsedMessages []ParsedMessage,
	pendingCmdMsgIdx, pendingMcpMsgIdx *int,
	content string,
) bool {
	if *pendingCmdMsgIdx >= 0 && *pendingCmdMsgIdx < len(parsedMessages) {
		target := &parsedMessages[*pendingCmdMsgIdx]
		for ci := range target.ToolCalls {
			tc := &target.ToolCalls[ci]
			tc.ResultEvents = append(
				tc.ResultEvents,
				ParsedToolResultEvent{
					Status:  "errored",
					Content: content,
				},
			)
		}
		*pendingCmdMsgIdx = -1
		return true
	} else if *pendingMcpMsgIdx >= 0 && *pendingMcpMsgIdx < len(parsedMessages) {
		target := &parsedMessages[*pendingMcpMsgIdx]
		for ci := range target.ToolCalls {
			tc := &target.ToolCalls[ci]
			tc.ResultEvents = append(
				tc.ResultEvents,
				ParsedToolResultEvent{
					Status:  "errored",
					Content: content,
				},
			)
		}
		*pendingMcpMsgIdx = -1
		return true
	}
	return false
}

// rooToolResultContent extracts the embedded result content from
// a tool's JSON payload. Only tools whose schemas define "content"
// as result data are included; write/edit tools use "content" as
// input and must not be treated as completed results.
func rooToolResultContent(toolName, text string) string {
	switch strings.ToLower(toolName) {
	case "readfile":
		// readFile returns file contents in the "content" field.
	default:
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var toolData map[string]any
	if err := json.Unmarshal([]byte(text), &toolData); err != nil {
		return ""
	}
	content, _ := toolData["content"].(string)
	return content
}

// rooCommandOutputIsError detects error patterns in command output
// text. Conservative — false negatives are preferred over false
// positives. Checks for non-zero exit codes (with optional colon
// separator), strong signal patterns anywhere in the output, and
// common error prefixes on the first line.
var rooExitStatusRe = regexp.MustCompile(
	`(?i)exit\s+(?:code|status)\s*:?\s*([1-9]\d*)`,
)

// rooErrorAnywhereRe matches strong error signal patterns that are
// reliable regardless of position in the output. Uses multiline
// mode (m) so ^ anchors to the start of every line, not just the
// start of the entire string.
var rooErrorAnywhereRe = regexp.MustCompile(
	`(?im)` +
		`npm ERR!|` +
		`^\s*Error:\s|` +
		`^\s*Fatal:\s|` +
		`^\s*Failed:\s`,
)

func rooCommandOutputIsError(output string) bool {
	if rooExitStatusRe.MatchString(output) {
		return true
	}
	if rooErrorAnywhereRe.MatchString(output) {
		return true
	}
	// Check the first non-empty line for error prefixes. Multi-line
	// command output often starts with a summary line like
	// "error: build failed" or "fatal: unable to access".
	lower := strings.ToLower(strings.TrimSpace(output))
	for _, prefix := range []string{
		"error:", "error ", "fatal:", "fatal ",
		"failed:", "failed ", "failure:",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// parseRooCodeToolCall extracts a ParsedToolCall from the JSON text of
// an ask="tool" message. The text field contains a JSON object like:
// {"tool":"readFile","path":"src/foo.ts","isOutsideWorkspace":false,...}
func parseRooCodeToolCall(text string) *ParsedToolCall {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var toolData map[string]any
	if err := json.Unmarshal([]byte(text), &toolData); err != nil {
		return nil
	}

	toolName, _ := toolData["tool"].(string)
	if toolName == "" {
		return nil
	}

	// Re-marshal to get canonical JSON for InputJSON.
	inputJSON, err := json.Marshal(toolData)
	if err != nil {
		return nil
	}

	toolUseID := "roocode:" + toolName
	tc := &ParsedToolCall{
		ToolUseID: toolUseID,
		ToolName:  toolName,
		InputJSON: string(inputJSON),
	}
	// Extract skill name for skill tool calls, matching the
	// pattern used by Claude, Codex, Forge, and other parsers.
	if strings.EqualFold(toolName, "skill") {
		tc.SkillName, _ = toolData["skill"].(string)
		if tc.SkillName == "" {
			tc.SkillName, _ = toolData["name"].(string)
		}
	} else {
		// Infer skill name from readFile calls to SKILL.md files,
		// matching how Cursor, Codex, Grok, Kimi, and ZCode detect
		// skill usage from file reads.
		tc.SkillName = inferToolSkillName(toolName, tc.InputJSON)
	}
	return tc
}

// discoverRooCodeSessions finds all task directories under a RooCode root.
// root is the globalStorage directory (e.g. ~/Library/.../RooVeterinaryInc.roo-cline).
// Sessions live under <root>/tasks/<taskId>/.
func discoverRooCodeSessions(root string) []rooCodeSessionDir {
	tasksDir := filepath.Join(root, "tasks")
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return nil
	}

	var dirs []rooCodeSessionDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Skip the _index.json file directory marker.
		if strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		taskDir := filepath.Join(tasksDir, entry.Name())
		if !IsRegularFile(filepath.Join(taskDir, "history_item.json")) {
			continue
		}
		dirs = append(dirs, rooCodeSessionDir{
			Path: taskDir,
		})
	}
	return dirs
}

// rooCodeSessionDir holds a discovered RooCode task directory path.
type rooCodeSessionDir struct {
	Path string
}

// rooCodeFingerprintSource computes a composite fingerprint from
// history_item.json and ui_messages.json for freshness detection.
func rooCodeFingerprintSource(path string) (SourceFingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", path,
		)
	}

	fp := SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}

	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(
		h, "history_item", path, info,
	); err != nil {
		return SourceFingerprint{}, err
	}

	// Include ui_messages.json for composite freshness.
	dir := filepath.Dir(path)
	msgPath := filepath.Join(dir, "ui_messages.json")
	msgInfo, err := siblingMetadataFileInfo(msgPath)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if msgInfo != nil {
		fp.Size += msgInfo.Size()
		if ts := msgInfo.ModTime().UnixNano(); ts > fp.MTimeNS {
			fp.MTimeNS = ts
		}
		if err := addSiblingMetadataFingerprintPart(
			h, "ui_messages", msgPath, msgInfo,
		); err != nil {
			return SourceFingerprint{}, err
		}
	}

	fp.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fp, nil
}

// IsThinkingOnlyContent reports whether content consists only of a
// [Thinking]...[/Thinking] block with no substantive text after the
// closing tag. This is how RooCode formats thinking-only assistant
// messages (Content = "[Thinking]\n<reasoning>\n[/Thinking]").
func IsThinkingOnlyContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || !strings.HasPrefix(trimmed, "[Thinking]") {
		return false
	}
	rest := strings.TrimSpace(trimmed[len("[Thinking]"):])
	closingTag := "[/Thinking]"
	idx := strings.Index(rest, closingTag)
	if idx < 0 {
		return false
	}
	afterClose := strings.TrimSpace(rest[idx+len(closingTag):])
	return afterClose == ""
}
