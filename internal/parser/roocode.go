// ABOUTME: Parses RooCode (rooveterinaryinc.roo-cline) VSCode extension
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
	"slices"
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
	TotalCost               *float64 `json:"totalCost"`
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

	// Incomplete or legacy history items can omit the id. Fall back
	// to the task directory name (which is the task ID on disk) so
	// such sessions get distinct IDs instead of all colliding on
	// "roocode:" and overwriting one another.
	if historyItem.ID == "" {
		historyItem.ID = filepath.Base(filepath.Clean(taskDir))
	}

	// Model name from the API config name (provider profile).
	model := historyItem.APIConfigName

	// Read and parse ui_messages.json (may not exist for empty sessions).
	parsedMessages, peakCtx, maxTS, err := parseRooCodeMessages(
		messagesPath, model,
	)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("parsing ui_messages.json: %w", err)
	}

	// If the history item records that a child task completed this
	// session (CompletedByChildID) but the transcript carried no
	// subtask_result message to pair, resolve the last unresolved
	// newTask so termination analysis does not report a false
	// tool_call_pending for the delegation.
	if historyItem.CompletedByChildID != "" {
		resolveTrailingRooNewTask(parsedMessages)
	}

	// Build session ID: "roocode:" + task ID.
	sessionID := string(AgentRooCode) + ":" + historyItem.ID

	// Parse startedAt from the history item timestamp (ms since epoch).
	startedAt := time.UnixMilli(historyItem.Timestamp)

	// Determine endedAt from the latest transcript timestamp. Prefer
	// maxTS (which includes paired command/MCP response timestamps
	// that are consumed during pairing and never emitted as messages),
	// and fall back to the latest emitted message timestamp.
	endedAt := maxTS
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
		sessionName = truncate(sessionName, 77)
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
	// order in ui_messages.json. Failed delegations never spawned
	// a child, so errored newTask calls are skipped to keep the
	// remaining calls aligned with their child IDs.
	if len(historyItem.ChildIDs) > 0 {
		childIdx := 0
		for mi := range parsedMessages {
			for ci := range parsedMessages[mi].ToolCalls {
				tc := &parsedMessages[mi].ToolCalls[ci]
				if tc.ToolName != "newTask" ||
					childIdx >= len(historyItem.ChildIDs) {
					continue
				}
				if rooToolCallErrored(tc) {
					continue
				}
				tc.SubagentSessionID = string(AgentRooCode) + ":" +
					historyItem.ChildIDs[childIdx]
				childIdx++
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
		historyItem.TotalCost != nil {
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
		// Set CostUSD whenever totalCost is present, including an
		// explicit zero. A reported cost of 0 (free tier, local model)
		// is authoritative and must override catalog-based pricing;
		// treating it as absent would misprice token-bearing sessions.
		if historyItem.TotalCost != nil {
			cost := *historyItem.TotalCost
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
func parseRooCodeMessages(
	path string, model string,
) ([]ParsedMessage, int, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, time.Time{}, err
	}

	var rawMessages []json.RawMessage
	if err := json.Unmarshal(data, &rawMessages); err != nil {
		// Try parsing as a single message.
		var single rooCodeMessage
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return nil, 0, time.Time{}, fmt.Errorf(
				"parsing ui_messages.json: %w", err,
			)
		}
		rawMessages = []json.RawMessage{data}
	}

	parsedMessages := make([]ParsedMessage, 0, len(rawMessages))
	ordinal := 0
	isFirst := true
	peakCtx := 0
	// maxTS tracks the latest raw transcript timestamp across all
	// parsed messages, including paired command/MCP responses that are
	// consumed during pairing and never emitted as standalone messages.
	// It drives EndedAt so result timestamps are not lost.
	var maxTS time.Time
	// pendingCmdMsgIdx tracks the index in parsedMessages of the
	// most recent execute_command tool call awaiting a result.
	// -1 means no pending command.
	pendingCmdMsgIdx := -1
	// pendingCmdErrored remembers whether any streamed command_output
	// chunk of the pending command looked like a failure, so the
	// stream's final status is error-sticky: a normal chunk after an
	// error must not make the command read as a success.
	pendingCmdErrored := false
	pendingMcpMsgIdx := -1
	// pendingNewTaskMsgIdx tracks the index of the most recent newTask
	// (delegated child) tool call awaiting a subtask_result. -1 means
	// none pending.
	pendingNewTaskMsgIdx := -1
	// pendingToolMsgIdx tracks the index of the most recent tool call
	// that has no specialized deferred-result channel, so tool-specific
	// errors (diff_error, rooignore_error) can be paired with it. -1
	// means none pending.
	pendingToolMsgIdx := -1
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
		if ts.After(maxTS) {
			maxTS = ts
		}

		// Determine role and extract tool calls / tool results.
		role, toolCalls, toolResults := classifyRooCodeMessage(
			msg, isFirst, ordinal,
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

		// Preserve image-only messages. RooCode stores pasted or
		// attached images as data URIs in the images field, so a
		// prompt or feedback message can carry images with no text.
		// Represent each as an [image] placeholder so the message
		// survives the empty-content check below; the data URI
		// itself is not transcript content.
		if content == "" && reasoning == "" && len(toolCalls) == 0 &&
			len(msg.Images) > 0 {
			content = strings.TrimSpace(
				strings.Repeat("[image] ", len(msg.Images)),
			)
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

		// Handle command_output by pairing it with the preceding
		// execute_command tool call. Instead of emitting a standalone
		// system message, update the tool call's ResultContent and
		// ResultEvents so the signals system can detect failures.
		if msg.Say == "command_output" && len(toolResults) > 0 {
			output := toolResults[0].ContentRaw
			paired := false
			if pendingCmdMsgIdx >= 0 &&
				pendingCmdMsgIdx < len(parsedMessages) {
				target := &parsedMessages[pendingCmdMsgIdx]
				// Streamed chunks are recorded as "running": the
				// command may still be executing, so no single chunk
				// can claim the final status. The stream's last chunk
				// is flipped to an aggregate, error-sticky status by
				// finalizeRooCommandStream when the stream ends (next
				// tool call or end of transcript).
				if rooCommandOutputIsError(output) {
					pendingCmdErrored = true
				}
				for ci := range target.ToolCalls {
					tc := &target.ToolCalls[ci]
					tc.ResultEvents = append(
						tc.ResultEvents,
						ParsedToolResultEvent{
							Status:    "running",
							Content:   output,
							Timestamp: ts,
						},
					)
				}
				if pendingToolMsgIdx == pendingCmdMsgIdx {
					pendingToolMsgIdx = -1
				}
				// Keep the command pending: RooCode streams long
				// command output as multiple command_output entries,
				// and a later chunk can carry the failure (exit
				// status, error line). Each chunk appends its own
				// result event; the stream ends when the next tool
				// call arrives.
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
							Status:    "completed",
							Content:   content,
							Timestamp: ts,
						},
					)
				}
				if pendingToolMsgIdx == pendingMcpMsgIdx {
					pendingToolMsgIdx = -1
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

		// Handle subtask_result by pairing it with the preceding
		// newTask (delegated child) tool call, marking the delegation
		// completed. Without this, a session that ends right after the
		// child finishes leaves the newTask call unresolved and
		// termination analysis reports a false tool_call_pending.
		if msg.Say == "subtask_result" {
			paired := false
			if pendingNewTaskMsgIdx >= 0 &&
				pendingNewTaskMsgIdx < len(parsedMessages) {
				target := &parsedMessages[pendingNewTaskMsgIdx]
				for ci := range target.ToolCalls {
					tc := &target.ToolCalls[ci]
					tc.ResultEvents = append(
						tc.ResultEvents,
						ParsedToolResultEvent{
							Status:    "completed",
							Content:   content,
							Timestamp: ts,
						},
					)
				}
				if pendingToolMsgIdx == pendingNewTaskMsgIdx {
					pendingToolMsgIdx = -1
				}
				pendingNewTaskMsgIdx = -1
				paired = true
			}
			if !paired && content != "" {
				// No pending newTask to pair with — emit as
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

		if len(toolCalls) > 0 {
			// A new tool call conclusively ends a command whose output
			// stream has begun: later command_output entries belong to
			// whatever runs next (or fall back to standalone), not to
			// the finished command. A command with no output yet stays
			// pending — its first output can legitimately trail other
			// activity when the user proceeds while it runs.
			if pendingCmdMsgIdx >= 0 && pendingCmdMsgIdx < len(parsedMessages) {
				prior := parsedMessages[pendingCmdMsgIdx].ToolCalls
				if len(prior) > 0 && len(prior[0].ResultEvents) > 0 {
					finalizeRooCommandStream(
						parsedMessages, pendingCmdMsgIdx, pendingCmdErrored,
					)
					pendingCmdMsgIdx = -1
					pendingCmdErrored = false
				}
			}
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
			msgIdx := len(parsedMessages) - 1
			switch toolCalls[0].ToolName {
			case "execute_command":
				pendingCmdMsgIdx = msgIdx
				pendingCmdErrored = false
			case "newTask":
				// Track delegated child task for pairing with
				// the subtask_result that reports its completion.
				pendingNewTaskMsgIdx = msgIdx
			default:
				// Track any MCP tool call (Category="MCP") for
				// pairing with mcp_server_response.
				if toolCalls[0].Category == "MCP" {
					pendingMcpMsgIdx = msgIdx
				}
			}
			// Track every tool call generally so tool-specific
			// errors (diff_error, rooignore_error) can pair with the
			// most recent unresolved call even when it has no
			// specialized deferred-result channel. Calls already
			// completed via embedded results (e.g. readFile content)
			// are not valid error targets — and they end the previous
			// call's turn, so clear the tracker instead of letting a
			// stale target absorb a later unrelated error.
			if len(toolCalls[0].ResultEvents) == 0 {
				pendingToolMsgIdx = msgIdx
			} else {
				pendingToolMsgIdx = -1
			}
			ordinal++
		} else if content != "" {
			errTargets := rooErrorTargets{
				cmd:     &pendingCmdMsgIdx,
				mcp:     &pendingMcpMsgIdx,
				newTask: &pendingNewTaskMsgIdx,
				general: &pendingToolMsgIdx,
			}

			// Tool error events (mistake_limit_reached, api_req_failed)
			// indicate the agent hit a failure limit. Link to the most
			// recent unresolved tool call as errored. Skip standalone
			// emission when pairing succeeds.
			if rooCodeIsToolErrorEvent(msg.Say, msg.Ask) {
				if rooPairErrorToPendingTool(
					parsedMessages, errTargets, content, ts,
				) {
					continue
				}
			}

			// Error say types (error, diff_error, rooignore_error)
			// indicate a tool failure — e.g. diff_error after
			// appliedDiff or rooignore_error after a file tool. Pair
			// them with the most recent unresolved tool call so the
			// signals system counts them as failures. Skip standalone
			// emission when pairing succeeds.
			if rooCodeIsErrorSay(msg.Say) {
				if rooPairErrorToPendingTool(
					parsedMessages, errTargets, content, ts,
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
			// A normal conversational message ends the most recent
			// tool call's turn: a tool-specific error arriving after
			// it belongs to whatever comes next, not to that call.
			// The command and MCP trackers stay pending — their
			// results arrive on dedicated say types that can
			// legitimately trail other messages.
			pendingToolMsgIdx = -1
		}
	}

	// The transcript ended while a command stream was open. Close it
	// with the aggregate status: a trailing output stream reads as the
	// command's result (an interrupted-mid-command snapshot is
	// indistinguishable from a finished one, and leaving it running
	// would report a false tool_call_pending for every session whose
	// last activity was a command).
	finalizeRooCommandStream(parsedMessages, pendingCmdMsgIdx, pendingCmdErrored)

	return parsedMessages, peakCtx, maxTS, nil
}

// finalizeRooCommandStream closes a streamed command_output sequence by
// flipping the last chunk's "running" status to the aggregate final
// status: "errored" when any chunk in the stream looked like a failure
// (error-sticky — a normal chunk after an error must not read as
// success), "completed" otherwise. A last event that is not "running"
// (e.g. an "errored" event appended by error-say pairing) is already
// final and left untouched.
func finalizeRooCommandStream(
	parsedMessages []ParsedMessage, idx int, errored bool,
) {
	if idx < 0 || idx >= len(parsedMessages) {
		return
	}
	target := &parsedMessages[idx]
	for ci := range target.ToolCalls {
		tc := &target.ToolCalls[ci]
		n := len(tc.ResultEvents)
		if n == 0 {
			continue
		}
		last := &tc.ResultEvents[n-1]
		if last.Status != "running" {
			continue
		}
		if errored {
			last.Status = "errored"
		} else {
			last.Status = "completed"
		}
	}
}

// classifyRooCodeMessage determines the role, tool calls, and tool results
// for a single RooCode message. The isFirst flag is true only for the very
// first message in the transcript, which carries the user's initial task.
func classifyRooCodeMessage(
	msg rooCodeMessage, isFirst bool, ordinal int,
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
		// by returning the output as a ParsedToolResult. Always
		// return a result (even when empty) so an empty-but-present
		// output still completes the call instead of leaving it
		// pending.
		output := strings.TrimSpace(msg.Text)
		return RoleUser, nil, []ParsedToolResult{{
			ContentLength: len(output),
			ContentRaw:    output,
		}}
	case "completion_result":
		return RoleAssistant, nil, nil
	case "subtask_result":
		// Result from a delegated child task. parseRooCodeMessages
		// intercepts this say type and pairs it with the preceding
		// newTask tool call; the standalone fallback is a system msg.
		// Always return a result (even when empty) so an empty
		// subtask_result still completes the delegation instead of
		// leaving the newTask call pending.
		output := strings.TrimSpace(msg.Text)
		return RoleSystem, nil, []ParsedToolResult{{
			ContentLength: len(output),
			ContentRaw:    output,
		}}
	case "user_feedback", "user_feedback_diff":
		// User feedback on the assistant's work.
		return RoleUser, nil, nil
	case "error", "diff_error", "rooignore_error",
		"shell_integration_warning":
		// Error and warning messages. Treat as system.
		return RoleSystem, nil, nil
	case "mcp_server_response":
		// MCP server tool response. parseRooCodeMessages pairs this
		// with the preceding use_mcp_server tool call. Always return
		// a result (even when empty) so an empty-but-present response
		// still completes the call instead of leaving it pending.
		output := strings.TrimSpace(msg.Text)
		return RoleSystem, nil, []ParsedToolResult{{
			ContentLength: len(output),
			ContentRaw:    output,
		}}
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
		tc := parseRooCodeToolCall(msg.Text, ordinal)
		if tc == nil {
			return RoleAssistant, nil, nil
		}
		// Extract embedded result content for tools whose schemas
		// define "content" as result data (e.g. readFile). Write/
		// edit tools use "content" as input and are excluded.
		if resultContent, present := rooToolResultContent(
			tc.ToolName, msg.Text,
		); present {
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
		// Assistant invokes a shell command. Encode as JSON
		// to satisfy the frontend's structured-InputJSON contract.
		cmdText := strings.TrimSpace(msg.Text)
		if cmdText == "" {
			return RoleAssistant, nil, nil
		}
		inputMap := map[string]string{"command": cmdText}
		inputJSON, err := json.Marshal(inputMap)
		if err != nil {
			return RoleAssistant, nil, nil
		}
		return RoleAssistant, []ParsedToolCall{{
			ToolUseID: fmt.Sprintf("roocode:execute_command:%d", ordinal),
			ToolName:  "execute_command",
			Category:  "Bash",
			InputJSON: string(inputJSON),
		}}, nil
	case "completion_result":
		return RoleAssistant, nil, nil
	case "use_mcp_server":
		// Assistant invokes an MCP server tool. The text field
		// carries JSON with server name and tool details.
		tc := parseRooCodeMcpCall(msg.Text, ordinal)
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
	if len(messages) == 0 {
		return ""
	}
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

// resolveTrailingRooNewTask marks the last unresolved newTask tool
// call in messages as completed. It is used as a fallback when the
// history item reports completion via a child task (CompletedByChildID)
// but the transcript carried no subtask_result message to pair. Only
// the most recent unresolved newTask is resolved; earlier ones that
// already have ResultEvents are left untouched.
func resolveTrailingRooNewTask(messages []ParsedMessage) {
	if len(messages) == 0 {
		return
	}
	for _, v := range slices.Backward(messages) {
		tcs := v.ToolCalls
		for ci := range slices.Backward(tcs) {
			// Take the element by index: v.ToolCalls shares its
			// backing array with the original message, so this
			// append is visible to the caller.
			tc := &tcs[ci]
			if tc.ToolName != "newTask" {
				continue
			}
			if len(tc.ResultEvents) > 0 {
				return
			}
			tc.ResultEvents = append(tc.ResultEvents,
				ParsedToolResultEvent{Status: "completed"})
			return
		}
	}
}

// rooLastMessageIsThinkingOnly reports whether the last
// non-system message is an assistant message containing only
// thinking/reasoning content with no actual response text or
// tool calls. This is a strong signal that the session was
// interrupted mid-thought.
func rooLastMessageIsThinkingOnly(messages []ParsedMessage) bool {
	for _, v := range slices.Backward(messages) {
		m := v
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

// rooErrorTargets bundles the pending-tool trackers an error event can
// attach to. Every tracker holds an index into parsedMessages of a tool
// call awaiting a result, or -1 when none is pending. general covers any
// tool call that has no specialized deferred-result channel (e.g.
// appliedDiff, readFile) so tool-specific errors like diff_error and
// rooignore_error still find a target.
type rooErrorTargets struct {
	cmd     *int
	mcp     *int
	newTask *int
	general *int
}

// trackers returns the pending-tool index pointers an error event may
// pair with or clear.
func (t rooErrorTargets) trackers() []*int {
	return []*int{t.cmd, t.mcp, t.newTask, t.general}
}

// rooToolCallErrored reports whether a tool call carries an errored
// result event, meaning the call failed rather than completed.
func rooToolCallErrored(tc *ParsedToolCall) bool {
	for _, ev := range tc.ResultEvents {
		if ev.Status == "errored" {
			return true
		}
	}
	return false
}

// rooPairErrorToPendingTool links an error event to the most recent
// unresolved tool call as an errored ResultEvent, preserving the error
// timestamp. Among the valid trackers it picks the one with the
// greatest message index: an error refers to whatever ran last, so an
// older pending command must not absorb a diff_error that belongs to a
// later edit. All trackers pointing at the paired message are cleared
// so the error is not double-counted. Returns true if pairing
// succeeded.
func rooPairErrorToPendingTool(
	parsedMessages []ParsedMessage,
	targets rooErrorTargets,
	content string,
	ts time.Time,
) bool {
	idx := -1
	for _, t := range targets.trackers() {
		if *t >= 0 && *t < len(parsedMessages) && *t > idx {
			idx = *t
		}
	}
	if idx < 0 {
		return false
	}
	target := &parsedMessages[idx]
	for ci := range target.ToolCalls {
		tc := &target.ToolCalls[ci]
		tc.ResultEvents = append(
			tc.ResultEvents,
			ParsedToolResultEvent{
				Status:    "errored",
				Content:   content,
				Timestamp: ts,
			},
		)
	}
	// Clear every tracker referencing the paired message. Clearing
	// the newTask tracker here keeps a later subtask_result from
	// attaching a completion to a delegation that already failed.
	for _, t := range targets.trackers() {
		if *t == idx {
			*t = -1
		}
	}
	return true
}

// rooResultBearingReadTools maps (lowercased) tool names whose
// payloads carry execution results in the "content" field to the set
// of payload keys that are result-only and must be stripped from
// InputJSON. These are read-family tools where "content" is the tool
// output (file contents, directory listing, search hits), not an
// argument. Write/edit tools (writeToFile, insertContent, appliedDiff,
// searchAndReplace) deliberately omit "content" here because their
// "content"/"diff" fields are inputs, not results.
var rooResultBearingReadTools = map[string][]string{
	"readfile":                {"content"},
	"listfiles":               {"content"},
	"listfilestoplevel":       {"content"},
	"listfilesrecursive":      {"content"},
	"listcodedefinitionnames": {"content"},
	"searchfiles":             {"content"},
}

// rooToolResultContent extracts the embedded result content from a
// tool's JSON payload. Only read-family tools whose schemas define
// "content" as result data are included; write/edit tools use
// "content"/"diff" as input and must not be treated as completed
// results.
//
// It returns (content, present). present is true when the tool is a
// result-content tool and the JSON carries an explicit "content" key
// (even if that value is the empty string), so an empty-but-present
// result still completes the tool call instead of leaving it pending.
func rooToolResultContent(toolName, text string) (string, bool) {
	if _, ok := rooResultBearingReadTools[strings.ToLower(toolName)]; !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	var toolData map[string]any
	if err := json.Unmarshal([]byte(text), &toolData); err != nil {
		return "", false
	}
	content, ok := toolData["content"].(string)
	if !ok {
		return "", false
	}
	return content, true
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

// parseRooCodeMcpCall extracts a ParsedToolCall from the JSON text of
// an ask="use_mcp_server" message. Canonical Roo/Cline payloads carry
// a "type" discriminator instead of a "tool" field:
//
//	{"type":"use_mcp_tool","serverName":"weather","toolName":"get_forecast","arguments":"..."}
//	{"type":"access_mcp_resource","serverName":"docs","uri":"docs://readme"}
//
// use_mcp_tool calls are named after the invoked toolName; resource
// accesses keep the type as the name. Payloads without "type" fall
// back to the generic decoder for the simple {"tool":...} shape.
func parseRooCodeMcpCall(text string, ordinal int) *ParsedToolCall {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	var toolData map[string]any
	if err := json.Unmarshal([]byte(trimmed), &toolData); err != nil {
		return nil
	}

	typeName, _ := toolData["type"].(string)
	if typeName == "" {
		return parseRooCodeToolCall(text, ordinal)
	}

	toolName := typeName
	if typeName == "use_mcp_tool" {
		if name, _ := toolData["toolName"].(string); name != "" {
			toolName = name
		}
	}

	inputJSON, err := json.Marshal(toolData)
	if err != nil {
		return nil
	}

	return &ParsedToolCall{
		ToolUseID: fmt.Sprintf("roocode:%s:%d", toolName, ordinal),
		ToolName:  toolName,
		Category:  "MCP",
		InputJSON: string(inputJSON),
	}
}

// parseRooCodeToolCall extracts a ParsedToolCall from the JSON text of
// an ask="tool" message. The text field contains a JSON object like:
// {"tool":"readFile","path":"src/foo.ts","isOutsideWorkspace":false,...}
func parseRooCodeToolCall(text string, ordinal int) *ParsedToolCall {
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

	// Strip result-only fields from the payload before building
	// InputJSON. Roo tool payloads combine arguments and results in a
	// single object; for read-family tools the "content" field is the
	// tool output, not an argument, and must not be retained as input
	// (it can hold large file contents that result storage blocks).
	for _, field := range rooResultBearingReadTools[strings.ToLower(toolName)] {
		delete(toolData, field)
	}

	// Re-marshal to get canonical JSON for InputJSON.
	inputJSON, err := json.Marshal(toolData)
	if err != nil {
		return nil
	}

	toolUseID := fmt.Sprintf("roocode:%s:%d", toolName, ordinal)
	tc := &ParsedToolCall{
		ToolUseID: toolUseID,
		ToolName:  toolName,
		Category:  NormalizeToolCategory(toolName),
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
	_, after, ok := strings.Cut(rest, closingTag)
	if !ok {
		return false
	}
	afterClose := strings.TrimSpace(after)
	return afterClose == ""
}
