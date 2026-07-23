// ABOUTME: Parses Poolside Agent CLI (`pool`) trajectory NDJSON files from
// ABOUTME: ~/Library/Application Support/poolside/trajectories/.
// ABOUTME: Each trajectory is a single NDJSON file with events like
// ABOUTME: session.start, session.input, assistant_message.start/end,
// ABOUTME: tool_call.parsed, tool_call.result, thought.start/end,
// ABOUTME: and tool_call.inference.start/end (per-request model +
// ABOUTME: token counts). The parser extracts messages, tool calls
// ABOUTME: with results, thinking text, and emits one usage event per
// ABOUTME: inference call with the model that actually served it, so
// ABOUTME: mid-session model switches attribute each token spend to the
// ABOUTME: model that generated it.
package parser

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const poolsideIDPrefix = "poolside:"

// poolsideEvent represents a single event in the NDJSON trajectory.
type poolsideEvent struct {
	ID        string `json:"id"`
	StepID    string `json:"step_id,omitempty"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`

	SessionStart           *poolsideSessionStart           `json:"session_start,omitempty"`
	SessionInput           *poolsideSessionInput           `json:"session_input,omitempty"`
	AssistantMessageEnd    *poolsideAssistantMessageEnd    `json:"assistant_message_end,omitempty"`
	ToolCallParsed         *poolsideToolCallParsed         `json:"tool_call_parsed,omitempty"`
	ToolCallResult         *poolsideToolCallResult         `json:"tool_call_result,omitempty"`
	ThoughtEnd             *poolsideThoughtEnd             `json:"thought_end,omitempty"`
	ToolCallInferenceStart *poolsideToolCallInferenceStart `json:"tool_call_inference_start,omitempty"`
	ToolCallInferenceEnd   *poolsideToolCallInferenceEnd   `json:"tool_call_inference_end,omitempty"`
	SessionExit            *poolsideSessionExit            `json:"session_exit,omitempty"`
	SessionError           *poolsideSessionError           `json:"session_error,omitempty"`
}

// poolsideSessionStart contains session metadata.
type poolsideSessionStart struct {
	Workspace          string   `json:"workspace"`
	WorkingDirectories []string `json:"working_directories"`
	Prompt             string   `json:"prompt"`
}

// poolsideSessionInput contains user input.
type poolsideSessionInput struct {
	ID                        string `json:"id"`
	Prompt                    string `json:"prompt"`
	EstimatedPromptTokenUsage int    `json:"estimated_prompt_token_usage"`
	Mode                      string `json:"mode"`
}

// poolsideAssistantMessageEnd contains assistant message content.
type poolsideAssistantMessageEnd struct {
	AssistantMessage string `json:"assistant_message"`
}

// poolsideToolCallParsed contains parsed tool call data.
type poolsideToolCallParsed struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// poolsideToolCallResult contains tool call result.
type poolsideToolCallResult struct {
	ID                 string                  `json:"id"`
	ToolName           string                  `json:"tool_name"`
	ExecutionLatency   int64                   `json:"execution_latency"`
	Observation        string                  `json:"observation"`
	IsError            bool                    `json:"is_error"`
	ExecutionErrorKind string                  `json:"execution_error_kind"`
	ShellRunResult     *poolsideShellRunResult `json:"shell_run_tool_result,omitempty"`
}

// poolsideShellRunResult contains shell execution result data.
type poolsideShellRunResult struct {
	ShellID string `json:"shell_id"`
}

// poolsideThoughtEnd contains thinking/reasoning text.
type poolsideThoughtEnd struct {
	Thought string `json:"thought"`
}

// poolsideToolCallInferenceStart contains chat completion request metadata.
type poolsideToolCallInferenceStart struct {
	ChatCompletionRequest *poolsideChatCompletionRequest `json:"chat_completion_request,omitempty"`
}

// poolsideChatCompletionRequest contains model and request details.
type poolsideChatCompletionRequest struct {
	Model string `json:"model"`
}

// poolsideToolCallInferenceEnd contains per-request token counts.
type poolsideToolCallInferenceEnd struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CacheReadInputTokens  int `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}

// poolsideSessionExit contains session exit reason.
type poolsideSessionExit struct {
	Reason string `json:"reason"`
}

// poolsideSessionError contains session error.
type poolsideSessionError struct {
	Error string `json:"error"`
}

// parsePoolsideSession parses a poolside trajectory NDJSON file.
func parsePoolsideSession(
	trajectoryPath string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	file, err := os.Open(trajectoryPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening trajectory file: %w", err)
	}
	defer file.Close()

	var sessionStart *poolsideSessionStart
	var exitReason string
	var firstEventTime time.Time
	var lastEventTime time.Time

	// Track tool calls for result pairing by call ID.
	pendingToolCalls := make(map[string]*pendingToolCallInfo)
	var toolCallOrdinals map[string]int

	// Track shell_id -> cmd mapping for enriching shell management tools.
	shellIDToCmd := make(map[string]string)
	// Track pending shell tool calls (call_id -> cmd) for shell_id extraction.
	pendingShellCmds := make(map[string]string)

	var messages []ParsedMessage
	var ordinal int

	// Aggregate tokens across all inference events.
	var totalOutputTokens int
	var inferenceCount int
	// Peak context is the maximum total context (input + cache read +
	// cache write) seen on a single inference call, not the cumulative
	// sum across calls.
	var peakContextTokens int

	// pendingInferences[step_id] = model. Populated on each
	// tool_call.inference.start and consumed by the matching
	// tool_call.inference.end so each token spend is attributed to
	// the model that produced it. Poolside's real trajectories
	// pair start/end events by step_id.
	pendingInferences := make(map[string]string)
	// currentModel is refreshed on every inference.start and used as
	// the fall-back attribution when an inference.end arrives
	// without a matching start. It also drives Model stamping on
	// assistant messages at assistant_message.end, which fires after
	// all inferences for that turn.
	var currentModel string
	// currentMsgStepID is the step_id of the most recent
	// assistant_message.start. In real poolside trajectories every
	// assistant turn within a step_id cluster shares that step_id
	// with its tool_call.inference events, so the producing model
	// can be resolved via pendingInferences[currentMsgStepID] at
	// assistant_message.end even though inference.start fires BEFORE
	// assistant_message.start in the stream.
	var currentMsgStepID string

	// usageEvents is appended inline on each tool_call.inference.end
	// (one event per inference, model-aware). The session-level
	// aggregate fields below reflect the same data but are not
	// double-counted.
	var usageEvents []ParsedUsageEvent

	// Extract session ID from filename up front so per-inference
	// usage events emitted inside the scan loop can tag every row
	// with the canonical ID. Pattern: trajectory-<type>_<uuid>.ndjson
	fileName := filepath.Base(trajectoryPath)
	sessionID := poolsideIDPrefix + strings.TrimPrefix(fileName, "trajectory-")
	sessionID = strings.TrimSuffix(sessionID, ".ndjson")

	lr := newLineReader(file, maxLineSize)
	defer releaseLineReader(lr)
	var malformedLines int
	var lastLineWasMalformed bool

	for {
		line, ok := lr.next()
		if !ok {
			break
		}

		var event poolsideEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			malformedLines++
			lastLineWasMalformed = true
			continue
		}
		lastLineWasMalformed = false

		ts := parsePoolsideTimestamp(event.Timestamp)
		if !ts.IsZero() {
			if firstEventTime.IsZero() {
				firstEventTime = ts
			}
			lastEventTime = ts
		}

		switch event.Type {
		case "session.start":
			if event.SessionStart != nil {
				sessionStart = event.SessionStart
			}

		case "session.input":
			if event.SessionInput != nil && event.SessionInput.Prompt != "" {
				ordinal++
				messages = append(messages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleUser,
					Content:       event.SessionInput.Prompt,
					Timestamp:     ts,
					ContentLength: len(event.SessionInput.Prompt),
				})
			}

		case "assistant_message.start":
			ordinal++
			messages = append(messages, ParsedMessage{
				Ordinal:   ordinal,
				Role:      RoleAssistant,
				Timestamp: ts,
			})
			// Capture the step_id cluster for this assistant
			// turn so assistant_message.end can resolve the
			// producing model via pendingInferences[
			// currentMsgStepID] even when that model's
			// inference.start fired BEFORE the assistant
			// message in the stream (which is the real-data
			// ordering). Reset currentModel so a turn whose
			// step_id has no recorded inference does not
			// inherit the previous turn's model.
			currentMsgStepID = event.StepID
			currentModel = ""

		case "assistant_message.end":
			if event.AssistantMessageEnd != nil && len(messages) > 0 {
				lastMsg := &messages[len(messages)-1]
				if lastMsg.Role == RoleAssistant {
					// Update content if present and not yet set.
					if event.AssistantMessageEnd.AssistantMessage != "" && lastMsg.Content == "" {
						lastMsg.Content = event.AssistantMessageEnd.AssistantMessage
						lastMsg.ContentLength = len(event.AssistantMessageEnd.AssistantMessage)
					}
					// Resolve the producing model. Real poolside
					// fires tool_call.inference.start BEFORE
					// assistant_message.start within the same
					// step_id cluster, then
					// assistant_message.end BEFORE
					// tool_call.inference.end (verified against
					// real-data trace: line 4 asst.start, line 6
					// asst.end, line 8 inference.end — all share
					// one step_id). So pendingInferences[
					// currentMsgStepID] still holds the model at
					// this point.
					// Order dependency: if a future version of
					// poolside reorders these events, attribution
					// falls through to the currentModel fallback.
					msgModel := pendingInferences[currentMsgStepID]
					msgModel = strings.TrimSpace(msgModel)
					if msgModel == "" {
						msgModel = currentModel
					}
					if lastMsg.Model == "" && msgModel != "" {
						lastMsg.Model = msgModel
					}
				}
			}

		case "thought.end":
			if event.ThoughtEnd != nil && event.ThoughtEnd.Thought != "" &&
				len(messages) > 0 {
				lastMsg := &messages[len(messages)-1]
				if lastMsg.Role == RoleAssistant {
					lastMsg.HasThinking = true
					if lastMsg.ThinkingText == "" {
						lastMsg.ThinkingText = event.ThoughtEnd.Thought
					} else {
						lastMsg.ThinkingText += "\n" + event.ThoughtEnd.Thought
					}
				}
			}

		case "tool_call.parsed":
			if event.ToolCallParsed != nil {
				tc := event.ToolCallParsed
				name := tc.Name
				if toolCallOrdinals == nil {
					toolCallOrdinals = make(map[string]int)
				}
				toolCallOrdinals[name]++
				toolUseID := fmt.Sprintf("%s%s:%d", poolsideIDPrefix, name, toolCallOrdinals[name])

				inputJSON := string(tc.Args)
				if inputJSON == "" || inputJSON == "null" {
					inputJSON = "{}"
				}

				// Extract skill name from skill tool calls.
				// For other tools, infer from SKILL.md references in read/shell.
				var skillName string
				skillName = inferToolSkillName(name, inputJSON)
				if name == "skill" && skillName == "" {
					skillName = gjson.Get(inputJSON, "skill").Str
					if skillName == "" {
						skillName = gjson.Get(inputJSON, "name").Str
					}
				}

				// Track shell tool calls for shell_id extraction.
				// Key by call ID to avoid overwriting when multiple
				// shell calls share a step_id.
				if name == "shell" {
					var args struct {
						Cmd string `json:"cmd"`
					}
					if json.Unmarshal(tc.Args, &args) == nil && args.Cmd != "" {
						pendingShellCmds[tc.ID] = args.Cmd
					}
				}

				// Enrich shell management tools with original command.
				if name == "shell_status" || name == "shell_tail" || name == "shell_kill" {
					var args struct {
						ShellID string `json:"shell_id"`
					}
					if json.Unmarshal(tc.Args, &args) == nil && args.ShellID != "" {
						if cmd, ok := shellIDToCmd[args.ShellID]; ok {
							// Create enriched input JSON with the original command.
							enriched := map[string]any{
								"shell_id": args.ShellID,
								"cmd":      cmd,
							}
							if data, err := json.Marshal(enriched); err == nil {
								inputJSON = string(data)
							}
						}
					}
				}

				// Track pending tool call for result pairing by call ID.
				// In real poolside data, tool_call.parsed and tool_call.result
				// share the same payload ID. Keying by call ID (not step_id)
				// avoids overwriting when multiple calls share a step.
				lastMsgOrdinal := 0
				if len(messages) > 0 {
					lastMsgOrdinal = messages[len(messages)-1].Ordinal
				}
				pendingToolCalls[tc.ID] = &pendingToolCallInfo{
					ordinal:   lastMsgOrdinal,
					name:      name,
					toolUseID: toolUseID,
				}

				// Set tool use on the current assistant message.
				if len(messages) > 0 {
					lastMsg := &messages[len(messages)-1]
					if lastMsg.Role == RoleAssistant {
						lastMsg.HasToolUse = true
						lastMsg.ToolCalls = append(lastMsg.ToolCalls, ParsedToolCall{
							ToolUseID: toolUseID,
							ToolName:  name,
							Category:  NormalizeToolCategory(name),
							InputJSON: inputJSON,
							SkillName: skillName,
						})
					}
				}
			}

		case "tool_call.result":
			if event.ToolCallResult != nil {
				tr := event.ToolCallResult

				// Determine status based on error state.
				status := "completed"
				if tr.IsError {
					if tr.ExecutionErrorKind == "approval_denied" {
						status = "denied"
					} else {
						status = "error"
					}
				}

				// Extract shell_id from shell tool results for enriching management tools.
				// Key by call ID to match the parsed-event storage.
				if tr.ToolName == "shell" && tr.ShellRunResult != nil &&
					tr.ShellRunResult.ShellID != "" {
					if cmd, ok := pendingShellCmds[tr.ID]; ok {
						shellIDToCmd[tr.ShellRunResult.ShellID] = cmd
						delete(pendingShellCmds, tr.ID)
					}
				}

				// Pair result with pending tool call using the payload call ID.
				if info, ok := pendingToolCalls[tr.ID]; ok {
					if info.ordinal > 0 && info.ordinal <= len(messages) {
						msg := &messages[info.ordinal-1]
						for i := range msg.ToolCalls {
							if msg.ToolCalls[i].ToolUseID == info.toolUseID {
								msg.ToolCalls[i].ResultEvents = append(msg.ToolCalls[i].ResultEvents, ParsedToolResultEvent{
									Status:    status,
									Content:   tr.Observation,
									Timestamp: ts,
								})
								break
							}
						}
					}
					delete(pendingToolCalls, tr.ID)
				}
			}

		case "tool_call.inference.start":
			// Record the model under the step_id so the matching
			// inference.end can attribute its tokens correctly.
			// Real Poolside trajectories link start and end by
			// step_id, so this map is the source of truth for
			// model attribution.
			if event.ToolCallInferenceStart != nil &&
				event.ToolCallInferenceStart.ChatCompletionRequest != nil &&
				event.StepID != "" {
				m := event.ToolCallInferenceStart.ChatCompletionRequest.Model
				if m != "" {
					pendingInferences[event.StepID] = m
					currentModel = m
				}
			}

		case "tool_call.inference.end":
			if event.ToolCallInferenceEnd != nil {
				inf := event.ToolCallInferenceEnd
				totalOutputTokens += inf.OutputTokens
				totalContext := inf.InputTokens + inf.CacheReadInputTokens + inf.CacheWriteInputTokens
				if totalContext > peakContextTokens {
					peakContextTokens = totalContext
				}
				inferenceCount++

				// Resolve the model for this inference. Prefer
				// the start event's model (paired by step_id);
				// fall back to currentModel if the end arrived
				// without a matching start (the trajectory was
				// truncated or the start was malformed) so the
				// attribution is never silently dropped.
				evModel, hadStart := pendingInferences[event.StepID]
				if hadStart {
					delete(pendingInferences, event.StepID)
				}
				evModel = strings.TrimSpace(evModel)
				if evModel == "" {
					evModel = currentModel
				}

				if event.StepID != "" && evModel != "" {
					usageEvents = append(usageEvents, ParsedUsageEvent{
						SessionID:                sessionID,
						Source:                   "inference",
						Model:                    evModel,
						InputTokens:              inf.InputTokens,
						OutputTokens:             inf.OutputTokens,
						CacheCreationInputTokens: inf.CacheWriteInputTokens,
						CacheReadInputTokens:     inf.CacheReadInputTokens,
						OccurredAt:               ts.Format(time.RFC3339Nano),
						DedupKey: fmt.Sprintf(
							"session:%s:inference:%s",
							sessionID, event.StepID,
						),
					})
				}
			}

		case "session.exit":
			if event.SessionExit != nil {
				exitReason = event.SessionExit.Reason
			}

		case "session.error":
			if event.SessionError != nil && event.SessionError.Error != "" {
				exitReason = "session_error"
			}
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("reading trajectory file: %w", err)
	}

	// sessionID was extracted from the filename before the scan
	// loop so per-inference usage events could carry it inline.

	// Parse startedAt from first event timestamp.
	startedAt := firstEventTime

	// Determine endedAt from last event time.
	endedAt := lastEventTime

	// Count user messages.
	userMsgCount := 0
	for _, msg := range messages {
		if msg.Role == RoleUser && !msg.IsSystem {
			userMsgCount++
		}
	}

	// Extract project from working directories.
	project := projectHint
	if sessionStart != nil && len(sessionStart.WorkingDirectories) > 0 {
		if p := ExtractProjectFromCwd(sessionStart.WorkingDirectories[0]); p != "" {
			project = p
		}
	}

	// Build first message for session name.
	firstMsg := ""
	for _, msg := range messages {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			firstMsg = truncateFirstMessage(msg.Content)
			break
		}
	}

	sessionName := firstMsg
	if sessionName == "" {
		sessionName = projectHint
	}

	// Source file info.
	info, err := os.Stat(trajectoryPath)
	fileInfo := FileInfo{
		Path: trajectoryPath,
	}
	if err == nil {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	// Classify termination from exit reason and truncation state.
	termination := classifyPoolsideTermination(exitReason, messages, lastLineWasMalformed)

	sess := &ParsedSession{
		ID:                sessionID,
		Project:           project,
		Machine:           machine,
		Agent:             AgentPoolside,
		Cwd:               sessionStartCwd(sessionStart),
		FirstMessage:      firstMsg,
		SessionName:       sessionName,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		IsTruncated:       lastLineWasMalformed,
		MessageCount:      len(messages),
		UserMessageCount:  userMsgCount,
		MalformedLines:    malformedLines,
		SourceSessionID:   strings.TrimPrefix(sessionID, poolsideIDPrefix),
		SourceVersion:     "poolside-trajectory-v1",
		File:              fileInfo,
		TerminationStatus: termination,
	}

	// Set aggregate session-level token fields. Peak context is the
	// largest total context (input + cache read + cache write) seen on
	// a single inference, not a sum. Per-inference usage events were
	// emitted inline above so this block does not produce a duplicate
	// aggregate event.
	if inferenceCount > 0 {
		sess.HasTotalOutputTokens = true
		sess.TotalOutputTokens = totalOutputTokens
		if peakContextTokens > 0 {
			sess.HasPeakContextTokens = true
			sess.PeakContextTokens = peakContextTokens
		}
	}

	return sess, messages, usageEvents, nil
}

// pendingToolCallInfo tracks a tool call for result pairing.
type pendingToolCallInfo struct {
	ordinal   int
	name      string
	toolUseID string
}

// parsePoolsideTimestamp parses a Poolside ISO 8601 timestamp.
func parsePoolsideTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000000-07:00", ts)
	}
	if err != nil {
		return time.Time{}
	}
	return t
}

// sessionStartCwd returns the first working directory from session start.
func sessionStartCwd(start *poolsideSessionStart) string {
	if start != nil && len(start.WorkingDirectories) > 0 {
		return start.WorkingDirectories[0]
	}
	return ""
}

// classifyPoolsideTermination maps poolside exit reasons to termination status.
func classifyPoolsideTermination(reason string, messages []ParsedMessage, isTruncated bool) TerminationStatus {
	// Truncation (malformed final record) takes precedence over all
	// other signals — a trajectory cut off mid-write is the stronger
	// explanation for why the session ended.
	if isTruncated {
		return TerminationTruncated
	}

	// A trailing exit tool call is Poolside's normal session termination
	// mechanism — the session ends when exit is invoked, so no result
	// event follows. Treat exit_tool_called as clean before checking
	// for orphaned tool calls to avoid misclassifying normal exits.
	if reason == "exit_tool_called" {
		return TerminationClean
	}

	// Check for orphaned tool calls — unresolved tools that are NOT a
	// trailing exit indicate the session ended mid-execution.
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}

	switch reason {
	case "", "cancelled":
		return TerminationClean
	case "memory_compression_error", "session_error":
		// Memory compression errors and session errors indicate
		// the session was interrupted.
		return TerminationTruncated
	case "user_cancelled":
		return TerminationClean
	default:
		return TerminationClean
	}
}

// hashPoolsideSourceFile computes a fingerprint hash for a trajectory file.
func hashPoolsideSourceFile(path string) (string, int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, 0, err
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()

	h := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()

	if _, err := io.Copy(h, f); err != nil {
		return "", 0, 0, err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), size, mtime, nil
}
