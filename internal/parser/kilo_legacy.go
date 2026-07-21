// ABOUTME: Parses Kilo (legacy) (kilocode.kilo-code) VSCode extension
// ABOUTME: session files from the tasks/ directory under VSCode
// ABOUTME: globalStorage. Each session is a task directory holding
// ABOUTME: task_metadata.json (only files_in_context), the Claude-shaped
// ABOUTME: api_conversation_history.json, and the Cline-shaped
// ABOUTME: ui_messages.json. The parser borrows roocode's Cline message
// ABOUTME: handling (tool-call and result pairing, reasoning pipeline,
// ABOUTME: compact boundaries, error linking) and derives session
// ABOUTME: timestamps, tokens, and cost from the ui_messages transcript
// ABOUTME: itself, since task_metadata.json does NOT carry the RooCode-
// ABOUTME: style IDs/tokens/cost/parent wiring.
// ABOUTME:
// ABOUTME: LEGACY-ONLY: this agent covers the pre-OpenCode legacy
// ABOUTME: extension (the RooCode-derived codebase). Kilo rebuilt the VS
// ABOUTME: Code extension on an OpenCode core (public beta 2026-03-10, GA
// ABOUTME: 2026-04-02); auto-update replaced this extension and new
// ABOUTME: sessions stopped appearing here around 2026-03-21. The rebuilt
// ABOUTME: extension shares ~/.local/share/kilo/kilo.db with the Kilo CLI
// ABOUTME: and is tracked by the `kilo` agent.
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

// kiloLegacyDefaultDirs returns the platform-specific default
// directories for Kilo (legacy) session storage. VSCode canonicalizes
// the on-disk extension directory name to all lowercase, so use
// "kilocode.kilo-code" even when the marketplace ID is mixed-case.
//
// Windows users with multiple VSCode variants typically have
// Code/User, Code - Insiders/User, and VSCodium/User; kilo IDE
// normally runs in the standard Code variant. Add the other
// variants here if need arises.
func kiloLegacyDefaultDirs() []string {
	return []string{
		// macOS
		"Library/Application Support/Code/User/globalStorage/kilocode.kilo-code",
		// Linux
		".config/Code/User/globalStorage/kilocode.kilo-code",
		// Windows
		"AppData/Roaming/Code/User/globalStorage/kilocode.kilo-code",
	}
}

// kiloLegacyMessage mirrors a ClineMessage in ui_messages.json.
// The shape is identical to RooCode's rooCodeMessage — both
// agents descend from Cline/Roo-Cline — so we share the same
// message and result-pairing logic.
type kiloLegacyMessage struct {
	Timestamp int64    `json:"ts"`
	Type      string   `json:"type"` // "ask" or "say"
	Ask       string   `json:"ask,omitempty"`
	Say       string   `json:"say,omitempty"`
	Text      string   `json:"text,omitempty"`
	Images    []string `json:"images,omitempty"`
	Partial   bool     `json:"partial,omitempty"`
	Reasoning string   `json:"reasoning,omitempty"`
}

// kiloLegacyTaskMetadata mirrors task_metadata.json. Kilo only
// records files_in_context here — there are no task IDs, no
// timestamps, no token totals, and no parent/child wiring. The
// parser therefore derives session metadata from the
// ui_messages transcript (and the task-dir mtime as a fallback)
// instead of this file.
type kiloLegacyTaskMetadata struct {
	FilesInContext []kiloLegacyFileContext `json:"files_in_context"`
}

type kiloLegacyFileContext struct {
	Path         string `json:"path"`
	RecordState  string `json:"record_state,omitempty"`
	RecordSource string `json:"record_source,omitempty"`
}

// kiloLegacyAPIHistoryMessage is a minimal view of an entry in the
// Claude-shaped api_conversation_history.json. Kilo embeds the
// active model, cost, and mode inside a user-role "environment_details"
// XML block (under "# Current Mode") on each turn, e.g.
//
//	# Current Mode
//	<slug>code</slug>
//	<name>Code</name>
//	<model>z-ai/glm-4.5-air:free</model>
//
// These are the only concrete model IDs Kilo persists (the
// ui_messages api_req_started events carry only the coarse
// inferenceProvider name), so we mine them from the API history.
type kiloLegacyAPIHistoryMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// kiloModelRe matches the <model>…</model> child of the
// "# Current Mode" block in an api_conversation_history environment
// detail. It is permissive about surrounding whitespace.
var kiloModelRe = regexp.MustCompile(
	`(?is)<model>\s*([^<>\s][^<>]*?)\s*</model>`,
)

// kiloWorkspaceDirRe matches the absolute workspace path RooCode
// embeds in every session's environment_details, e.g.
//
//	Current Workspace Directory (/Users/dev/code/widgets) Files
//
// The path appears in api_req_started say text in ui_messages.json
// and in the user-role environment blocks of
// api_conversation_history.json. task_metadata.json only stores
// workspace-relative file paths, so this line is the only reliable
// source for the session's project.
var kiloWorkspaceDirRe = regexp.MustCompile(
	`Current Workspace Directory \(([^)]+)\)`,
)

// extractKiloLegacyWorkspaceDir mines the absolute workspace
// directory from the session transcript so the project can be
// derived from it. It searches ui_messages.json first (the
// api_req_started environment block), then falls back to the
// api_conversation_history.json user-role environment blocks,
// which carry the same line even for short sessions that never
// emitted an api_req_started event. Returns "" when neither file
// records the workspace directory.
func extractKiloLegacyWorkspaceDir(
	uiBytes []byte, apiHistoryPath string,
) string {
	if mm := kiloWorkspaceDirRe.FindSubmatch(uiBytes); mm != nil {
		if dir := strings.TrimSpace(string(mm[1])); dir != "" {
			return dir
		}
	}
	apiBytes, err := os.ReadFile(apiHistoryPath)
	if err != nil {
		return ""
	}
	if mm := kiloWorkspaceDirRe.FindSubmatch(apiBytes); mm != nil {
		if dir := strings.TrimSpace(string(mm[1])); dir != "" {
			return dir
		}
	}
	return ""
}

// parseKiloLegacyAPIHistoryModels walks api_conversation_history.json
// and returns the model ID that was active for each assistant turn,
// in conversation order. The model is recorded on user-role
// environment blocks; we carry the most-recently-seen model forward
// so every assistant turn has a label even when an intermediate
// user message omits the block. Returns an empty slice when the
// file is missing or yields no model.
func parseKiloLegacyAPIHistoryModels(
	path string,
) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var msgs []kiloLegacyAPIHistoryMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, err
	}
	var models []string
	last := ""
	for _, m := range msgs {
		if m.Role != "user" {
			// A model block lives on the user turn immediately
			// preceding an assistant turn; flush the carried
			// model as the label for this assistant turn.
			models = append(models, last)
			continue
		}
		for _, part := range m.Content {
			if mm := kiloModelRe.FindStringSubmatch(part.Text); mm != nil {
				if model := strings.TrimSpace(mm[1]); model != "" {
					last = model
				}
			}
		}
	}
	return models, nil
}

// parseKiloLegacySession parses a single Kilo (legacy) task directory and
// returns the parsed session with messages. The task directory
// must contain task_metadata.json and may contain
// ui_messages.json and api_conversation_history.json. When
// ui_messages.json does not exist (empty task), returns nil
// session and nil messages with a nil error so the provider can
// skip cleanly.
func parseKiloLegacySession(
	taskDir string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	metadataPath := filepath.Join(taskDir, "task_metadata.json")
	messagesPath := filepath.Join(taskDir, "ui_messages.json")
	apiHistoryPath := filepath.Join(taskDir, "api_conversation_history.json")

	// task_metadata.json is required; without it the directory is
	// not a recognised task and we let the caller skip cleanly.
	metadataInfo, err := os.Stat(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat task_metadata.json: %w", err)
	}

	// Read task_metadata.json for the (limited) metadata Kilo
	// actually records: we only surface the first files_in_context
	// path as a project hint when neither transcript nor
	// projectHint already give us something better.
	var metadata kiloLegacyTaskMetadata
	if metadataBytes, readErr := os.ReadFile(metadataPath); readErr == nil {
		_ = json.Unmarshal(metadataBytes, &metadata)
	}

	// Read and parse ui_messages.json. We treat its absence as a
	// clean no-session so providers can record the file as a skip
	// rather than a parse failure. The aggregate totals and
	// earliest/latest timestamps are declared at the outer scope
	// so they survive below the if-block.
	var (
		parsedMessages   []ParsedMessage
		totalOutputTok   int
		totalInputTok    int
		peakContextTok   int
		totalCost        float64
		hasCost          bool
		provider         string
		model            string
		multiModel       bool
		workspaceDir     string
		minTS, maxTS     time.Time
		totalCacheReads  int
		totalCacheWrites int
	)
	if msgsBytes, readErr := os.ReadFile(messagesPath); readErr == nil {
		// The absolute workspace directory is the authoritative
		// project source; task_metadata.json only stores
		// workspace-relative paths. Mine it from the transcript's
		// environment_details before parsing messages.
		workspaceDir = extractKiloLegacyWorkspaceDir(
			msgsBytes, apiHistoryPath,
		)
		// Mine concrete model IDs (z-ai/glm-4.5-air:free, …) from
		// the API history's per-turn environment blocks. This is
		// authoritative — ui_messages only carries the coarse
		// inferenceProvider name.
		apiModels, apiModelErr := parseKiloLegacyAPIHistoryModels(
			apiHistoryPath,
		)
		if apiModelErr != nil {
			return nil, nil, fmt.Errorf(
				"parsing api_conversation_history.json: %w",
				apiModelErr,
			)
		}
		// The most-recent concrete model in the API history is the
		// session's effective model for the aggregated usage event.
		// If multiple distinct models were observed, omit the model
		// from the usage event to avoid misattribution.
		model = lastNonEmpty(apiModels)
		multiModel = distinctModels(apiModels) > 1
		if multiModel {
			model = ""
		}
		var parseErr error
		parsedMessages, totalOutputTok, totalInputTok, peakContextTok,
			totalCost, hasCost, provider, minTS, maxTS,
			totalCacheReads, totalCacheWrites, parseErr =
			parseKiloLegacyMessages(msgsBytes, apiModels)
		if parseErr != nil {
			return nil, nil, fmt.Errorf(
				"parsing ui_messages.json: %w", parseErr,
			)
		}
	}

	// Build session ID: "kilo-legacy:" + basename (task UUID).
	taskID := filepath.Base(taskDir)
	sessionID := string(AgentKiloLegacy) + ":" + taskID

	// Use the minimum transcript timestamp for startedAt, falling
	// back to the metadata file's mtime when no transcript exists.
	// If both are unavailable, end up at a zero time and the
	// caller/frontend surfaces the absence without crashing.
	startedAt := minTS
	if startedAt.IsZero() {
		startedAt = metadataInfo.ModTime()
	}
	// endedAt mirrors the roocode pattern: prefer maxTS so the
	// transcript's last moment (including paired command/MCP
	// response timestamps that we consume rather than emit) is
	// preserved. Fall back to the latest emitted message
	// timestamp, then the metadata mtime.
	endedAt := maxTS
	if endedAt.IsZero() {
		for _, msg := range parsedMessages {
			if msg.Timestamp.After(endedAt) {
				endedAt = msg.Timestamp
			}
		}
	}
	if endedAt.IsZero() {
		endedAt = metadataInfo.ModTime()
	}

	// Compute firstMessage + SessionName from the user's first
	// non-system prompt.
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
	sessionName := firstMsg
	if len(sessionName) > 80 {
		sessionName = sessionName[:77] + "..."
	}
	if sessionName == "" {
		sessionName = projectHint
	}

	// Derive project from the absolute workspace directory mined
	// from the transcript's environment_details — the authoritative
	// source, present in nearly every session. Fall back to the
	// first files_in_context path only when it is absolute (Kilo
	// normally records workspace-relative paths there, so this
	// rarely fires), then to the coarse project hint.
	project := projectHint
	if workspaceDir != "" {
		if p := ExtractProjectFromCwd(workspaceDir); p != "" {
			project = p
		}
	} else if len(metadata.FilesInContext) > 0 {
		if first := metadata.FilesInContext[0].Path; first != "" {
			if dir := absoluteParent(first); dir != "" {
				if p := ExtractProjectFromCwd(dir); p != "" {
					project = p
				}
			}
		}
	}

	// Count user messages with non-empty content (system-feedback
	// messages are excluded so the counter reflects real turns).
	userMsgCount := 0
	for _, msg := range parsedMessages {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
		}
	}

	// Source file identity: the provider supplies the authoritative
	// composite fingerprint (task_metadata.json anchor folded with
	// ui_messages.json and api_conversation_history.json) via
	// kiloLegacyFingerprintSource, and overwrites sess.File after parse.
	// Here we only anchor the primary source path; Size/Mtime are
	// filled in by the provider.
	fileInfo := FileInfo{
		Path: metadataPath,
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentKiloLegacy,
		FirstMessage:     firstMsg,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsedMessages),
		UserMessageCount: userMsgCount,
		SourceSessionID:  taskID,
		SourceVersion:    "kilo-legacy-task-v1",
		File:             fileInfo,
		Cwd:              workspaceDir,
	}

	// Token / cost accounting. Aggregate per-request
	// api_req_started events into a single ParsedUsageEvent so
	// the pricing catalog sees one entry per session. The event's
	// Model is the concrete model ID mined from
	// api_conversation_history.json when present, falling back to
	// the coarse inferenceProvider name (e.g. "Z.AI") that Kilo
	// records in ui_messages.json. A present-positive reported
	// cost is treated as authoritative regardless of Model, and
	// the session's cost is attributed to the derived project via
	// s.project in the analytics queries.
	if peakContextTok > 0 {
		sess.PeakContextTokens = peakContextTok
		sess.HasPeakContextTokens = true
	}
	if totalOutputTok > 0 {
		sess.TotalOutputTokens = totalOutputTok
		sess.HasTotalOutputTokens = true
	}
	sess.aggregateTokenPresenceKnown =
		sess.HasTotalOutputTokens || sess.HasPeakContextTokens

	if totalOutputTok > 0 || peakContextTok > 0 || hasCost {
		event := ParsedUsageEvent{
			SessionID: sessionID,
			Source:    "session",
			OccurredAt: func() string {
				if !endedAt.IsZero() {
					return endedAt.Format(time.RFC3339Nano)
				}
				return startedAt.Format(time.RFC3339Nano)
			}(),
			DedupKey: "session:" + sessionID,
		}
		// Input tokens are summed from each api_req_started
		// tokensIn so the usage event can feed catalog-based cost
		// computation once a priced model entry exists.
		if totalInputTok > 0 {
			event.InputTokens = totalInputTok
		}
		if totalOutputTok > 0 {
			event.OutputTokens = totalOutputTok
		}
		if hasCost {
			cost := totalCost
			event.CostUSD = &cost
		}
		if totalCacheReads > 0 {
			event.CacheReadInputTokens = totalCacheReads
		}
		if totalCacheWrites > 0 {
			event.CacheCreationInputTokens = totalCacheWrites
		}
		// Prefer the concrete model ID mined from the API history;
		// fall back to the coarse inferenceProvider name when the
		// history carries no model block. Skip the fallback when
		// multiple models were observed to avoid misattribution.
		if model != "" {
			event.Model = model
		} else if !multiModel && provider != "" {
			event.Model = provider
		}
		sess.UsageEvents = []ParsedUsageEvent{event}
	}

	// Termination classification mirrors roocode: orphaned tool
	// calls and thinking-only endings always flip to
	// tool_call_pending, regardless of explicit status.
	sess.TerminationStatus = classifyKiloLegacyTermination(parsedMessages)

	return sess, parsedMessages, nil
}

// parseKiloLegacyMessages reads and parses a ui_messages.json blob
// into ParsedMessages. Returns the aggregate total output tokens,
// peak context window, summed cost + presence flag, and the
// transcript's earliest/latest timestamps so the parent can
// compute startedAt/endedAt. Errors only on a malformed array
// payload; message-level decoding errors are tolerated.
func parseKiloLegacyMessages(
	data []byte,
	apiModels []string,
) (
	messages []ParsedMessage,
	totalOutput int,
	totalInput int,
	peakContext int,
	totalCost float64,
	hasCost bool,
	provider string,
	minTS, maxTS time.Time,
	totalCacheReads int,
	totalCacheWrites int,
	err error,
) {
	var rawMessages []json.RawMessage
	if unmarshalErr := json.Unmarshal(data, &rawMessages); unmarshalErr != nil {
		// Tolerate a single-object file (defensive).
		var single kiloLegacyMessage
		if singleErr := json.Unmarshal(data, &single); singleErr != nil {
			return nil, 0, 0, 0, 0, false, "",
				time.Time{}, time.Time{}, 0, 0,
				fmt.Errorf("parsing ui_messages.json: %w", unmarshalErr)
		}
		rawMessages = []json.RawMessage{data}
	}

	messages = make([]ParsedMessage, 0, len(rawMessages))
	ordinal := 0
	isFirst := true
	pendingCmdMsgIdx := -1
	pendingCmdErrored := false
	pendingMcpMsgIdx := -1
	pendingCodebaseSearchMsgIdx := -1
	pendingNewTaskMsgIdx := -1
	// pendingToolMsgIdx tracks the most recent tool call that has
	// no specialised deferred-result channel (e.g. appliedDiff)
	// so tool-specific errors (diff_error, rooignore_error) can
	// be paired back even when the call wouldn't otherwise get
	// a result.
	pendingToolMsgIdx := -1

	for _, rawMsg := range rawMessages {
		var msg kiloLegacyMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}

		// Skip partial streaming fragments — they are identical
		// to their final form when the partial=false version
		// arrives.
		if msg.Partial {
			continue
		}

		// Internal metadata messages are skipped, but we keep
		// the side effect of computing per-request token
		// accounting for peak context and aggregate totals.
		if kiloIsMetadataSay(msg.Say) {
			if msg.Say == "api_req_started" && msg.Text != "" {
				ctx, in, out, cost, costPresent, prov, cr, cw :=
					kiloExtractAPIRequestStats(msg.Text)
				if ctx > peakContext {
					peakContext = ctx
				}
				if in > 0 {
					totalInput += in
				}
				if out > 0 {
					totalOutput += out
				}
				if costPresent {
					hasCost = true
					totalCost += cost
				}
				if cr > 0 {
					totalCacheReads += cr
				}
				if cw > 0 {
					totalCacheWrites += cw
				}
				// Surface the most specific non-empty inference
				// provider so the usage event carries a model label
				// even though Kilo records only the provider name.
				if prov != "" {
					provider = prov
				}
			}
			continue
		}

		ts := time.UnixMilli(msg.Timestamp)
		if ts.After(maxTS) {
			maxTS = ts
		}
		if minTS.IsZero() || ts.Before(minTS) {
			minTS = ts
		}

		role, toolCalls, toolResults := classifyKiloLegacyMessage(
			msg, isFirst, ordinal,
		)
		isFirst = false

		content := strings.TrimSpace(msg.Text)
		reasoning := strings.TrimSpace(msg.Reasoning)
		if msg.Say == "reasoning" && reasoning == "" && content != "" {
			// Cline/RooCode puts reasoning into the text field
			// when the structured `reasoning` field is empty.
			reasoning = content
			content = ""
		}
		// Tool-call payloads carry JSON/command in `text`; clear
		// so we emit only the structured tool-call message.
		if len(toolCalls) > 0 {
			content = ""
		}

		// Compact-boundary messages may have empty text and are
		// still meaningful for CompactionCount tracking.
		if kiloIsCompactBoundary(msg.Say) {
			messages = append(messages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleSystem,
				Content:           content,
				IsSystem:          true,
				IsCompactBoundary: true,
				Timestamp:         ts,
			})
			ordinal++
			continue
		}

		if content == "" && reasoning == "" &&
			len(toolCalls) == 0 &&
			len(toolResults) == 0 &&
			len(msg.Images) == 0 {
			continue
		}

		// Preserve image-only messages by substituting placeholders.
		if content == "" && reasoning == "" &&
			len(toolCalls) == 0 &&
			len(toolResults) == 0 &&
			len(msg.Images) > 0 {
			content = strings.TrimSpace(
				strings.Repeat("[image] ", len(msg.Images)),
			)
		}

		// Unwrap JSON envelopes for followup and completion_result
		// asks. These carry {"question":"..."} or {"suggest":"..."}
		// payloads; extract the actual text for display.
		if msg.Ask == "followup" || msg.Ask == "completion_result" {
			if unwrapped := kiloUnwrapJSONEnvelope(content); unwrapped != "" {
				content = unwrapped
			}
		}

		if reasoning != "" {
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       "[Thinking]\n" + reasoning + "\n[/Thinking]",
				ThinkingText:  reasoning,
				HasThinking:   true,
				Timestamp:     ts,
				ContentLength: len(reasoning),
			})
			ordinal++
			// If the message had only reasoning and no content,
			// skip the regular-message emit to avoid an empty row.
			if content == "" && len(toolCalls) == 0 &&
				len(toolResults) == 0 {
				continue
			}
		}

		// Pair command_output → preceding execute_command tool
		// call. Always emit a result (even on empty output) so
		// the call is not left pending.
		if msg.Say == "command_output" && len(toolResults) > 0 {
			output := toolResults[0].ContentRaw
			paired := false
			if pendingCmdMsgIdx >= 0 &&
				pendingCmdMsgIdx < len(messages) {
				target := &messages[pendingCmdMsgIdx]
				if kiloCommandOutputIsError(output) {
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
				// Keep the command pending: Kilo Legacy streams
				// long command output as multiple command_output
				// entries, and a later chunk can carry the failure
				// (exit status, error line). Each chunk appends
				// its own result event; the stream ends when the
				// next tool call arrives.
				paired = true
			}
			if !paired && output != "" {
				messages = append(messages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleUser,
					Content:       output,
					IsSystem:      true,
					Timestamp:     ts,
					ContentLength: len(output),
					ToolResults:   toolResults,
				})
				ordinal++
			}
			continue
		}

		// Pair mcp_server_response → preceding use_mcp_server
		// call.
		if msg.Say == "mcp_server_response" {
			if pendingMcpMsgIdx >= 0 &&
				pendingMcpMsgIdx < len(messages) {
				target := &messages[pendingMcpMsgIdx]
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
				continue
			}
			if content != "" {
				messages = append(messages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleSystem,
					Content:       content,
					IsSystem:      true,
					Timestamp:     ts,
					ContentLength: len(content),
				})
				ordinal++
				continue
			}
		}

		// Pair codebase_search_result → preceding codebaseSearch
		// tool call. The result is a JSON payload with the search
		// results; emit as a completed ResultEvent.
		if msg.Say == "codebase_search_result" {
			if pendingCodebaseSearchMsgIdx >= 0 &&
				pendingCodebaseSearchMsgIdx < len(messages) {
				target := &messages[pendingCodebaseSearchMsgIdx]
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
				if pendingToolMsgIdx == pendingCodebaseSearchMsgIdx {
					pendingToolMsgIdx = -1
				}
				pendingCodebaseSearchMsgIdx = -1
				continue
			}
			// Orphaned search result — emit as standalone.
			if content != "" {
				messages = append(messages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleSystem,
					Content:       content,
					IsSystem:      true,
					Timestamp:     ts,
					ContentLength: len(content),
				})
				ordinal++
				continue
			}
		}

		// Handle subtask_result by pairing it with the preceding
		// newTask (delegated child) tool call, marking the delegation
		// completed. Without this, a session that ends right after the
		// child finishes leaves the newTask call unresolved and
		// termination analysis reports a false tool_call_pending.
		if msg.Say == "subtask_result" {
			paired := false
			if pendingNewTaskMsgIdx >= 0 &&
				pendingNewTaskMsgIdx < len(messages) {
				target := &messages[pendingNewTaskMsgIdx]
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
				messages = append(messages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleSystem,
					Content:       content,
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
			if pendingCmdMsgIdx >= 0 && pendingCmdMsgIdx < len(messages) {
				prior := messages[pendingCmdMsgIdx].ToolCalls
				if len(prior) > 0 && len(prior[0].ResultEvents) > 0 {
					finalizeKiloCommandStream(
						messages, pendingCmdMsgIdx, pendingCmdErrored,
					)
					pendingCmdMsgIdx = -1
					pendingCmdErrored = false
				}
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       "",
				ToolCalls:     toolCalls,
				HasToolUse:    true,
				Timestamp:     ts,
				ContentLength: 0,
			})
			msgIdx := len(messages) - 1
			switch toolCalls[0].ToolName {
			case "execute_command", "executeCommand":
				pendingCmdMsgIdx = msgIdx
				pendingCmdErrored = false
			case "codebaseSearch":
				pendingCodebaseSearchMsgIdx = msgIdx
			case "newTask":
				pendingNewTaskMsgIdx = msgIdx
			}
			// MCP calls use the mcp__<server>__<tool> name form,
			// consistent with other agent harnesses, so pair the
			// following mcp_server_response back to this call.
			// Track by name prefix, category, or the original
			// ask type to handle cases where serverName is missing.
			if strings.HasPrefix(toolCalls[0].ToolName, "mcp__") ||
				toolCalls[0].Category == "MCP" ||
				msg.Ask == "use_mcp_server" {
				pendingMcpMsgIdx = msgIdx
			}
			// Track every emitted tool call so a tool-specific
			// error (diff_error, rooignore_error) can still find
			// a target.
			pendingToolMsgIdx = msgIdx
			ordinal++
			continue
		}

		// Error say / ask events pair with the most recent
		// unresolved tool call. Skip standalone emission when
		// pairing succeeds.
		errTargets := pendingToolErrorTargets{
			cmd:     &pendingCmdMsgIdx,
			mcp:     &pendingMcpMsgIdx,
			newTask: &pendingNewTaskMsgIdx,
			general: &pendingToolMsgIdx,
		}
		if kiloIsErrorSay(msg.Say) {
			if pairErrorToPendingTool(
				messages, errTargets, content, ts,
			) {
				continue
			}
		}
		if kiloIsToolErrorEvent(msg.Say, msg.Ask) {
			if pairErrorToPendingTool(
				messages, errTargets, content, ts,
			) {
				continue
			}
		}

		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
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

	// Attribute the concrete model ID mined from the API history to
	// every assistant turn. The api_conversation_history user
	// environment blocks carry the per-turn model, but the two
	// transcripts are not reliably 1:1 turn-aligned, so we use the
	// session's effective model (the most-recent concrete model seen
	// across the history) for all assistant messages rather than risk
	// mislabeling individual turns. Provider-only usage events stay
	// model-less.

	// Finalize any pending command stream: flip the last "running"
	// event to "completed" or "errored" based on the error-sticky flag.
	if pendingCmdMsgIdx >= 0 {
		finalizeKiloCommandStream(messages, pendingCmdMsgIdx, pendingCmdErrored)
	}

	if model := lastNonEmpty(apiModels); model != "" {
		for i := range messages {
			if messages[i].Role == RoleAssistant {
				messages[i].Model = model
			}
		}
	}

	return messages, totalOutput, totalInput, peakContext,
		totalCost, hasCost, provider, minTS, maxTS,
		totalCacheReads, totalCacheWrites, nil
}

// finalizeKiloCommandStream closes a streamed command_output sequence by
// flipping the last chunk's "running" status to the aggregate final
// status: "errored" when any chunk in the stream looked like a failure
// (error-sticky — a normal chunk after an error must not read as
// success), "completed" otherwise. A last event that is not "running"
// (e.g. an "errored" event appended by error-say pairing) is already
// final and left untouched.
func finalizeKiloCommandStream(
	messages []ParsedMessage, idx int, errored bool,
) {
	if idx < 0 || idx >= len(messages) {
		return
	}
	target := &messages[idx]
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

// lastNonEmpty returns the last non-empty string in s, or "" when
// every element is empty. Used to pick a session's effective model
// from the API history's per-turn model sequence.
func lastNonEmpty(s []string) string {
	if s == nil {
		return ""
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != "" {
			return s[i]
		}
	}
	return ""
}

// distinctModels counts the number of distinct non-empty model strings
// in s. Used to detect model-switching sessions where attributing all
// usage to a single model would be incorrect.
func distinctModels(s []string) int {
	seen := make(map[string]bool, len(s))
	for _, m := range s {
		if m != "" {
			seen[m] = true
		}
	}
	return len(seen)
}

// classifyKiloLegacyMessage determines the role, tool calls, and
// tool results for a single Cline message. The isFirst flag
// handles RooCode/Cline's quirk that message [0] is always the
// user's initial task prompt (say="text", not ask).
func classifyKiloLegacyMessage(
	msg kiloLegacyMessage, isFirst bool, ordinal int,
) (RoleType, []ParsedToolCall, []ParsedToolResult) {
	say := msg.Say
	ask := msg.Ask

	if isFirst {
		return RoleUser, nil, nil
	}

	switch say {
	case "text":
		return RoleAssistant, nil, nil
	case "reasoning":
		return RoleAssistant, nil, nil
	case "command_output":
		// Always return a result (even on empty output) so an
		// empty-but-present output still completes the
		// preceding execute_command call instead of leaving it
		// pending.
		output := strings.TrimSpace(msg.Text)
		return RoleUser, nil, []ParsedToolResult{{
			ContentLength: len(output),
			ContentRaw:    output,
		}}
	case "completion_result":
		return RoleAssistant, nil, nil
	case "subtask_result":
		return RoleSystem, nil, nil
	case "user_feedback", "user_feedback_diff":
		return RoleUser, nil, nil
	case "error", "diff_error", "rooignore_error",
		"shell_integration_warning":
		return RoleSystem, nil, nil
	case "mcp_server_response":
		return RoleSystem, nil, nil
	case "condense_context", "condense_context_error",
		"sliding_window_truncation":
		return RoleSystem, nil, nil
	case "codebase_search_result":
		return RoleAssistant, nil, nil
	}

	switch ask {
	case "tool":
		tc := parseKiloLegacyToolCall(msg.Text, ordinal)
		if tc == nil {
			return RoleAssistant, nil, nil
		}
		if resultContent, present := kiloToolResultContent(
			tc.ToolName, msg.Text,
		); present {
			tc.ResultEvents = append(tc.ResultEvents,
				ParsedToolResultEvent{
					Status:  "completed",
					Content: resultContent,
				},
			)
		}
		// newTask delegates to a child session. The
		// NormalizeToolCategory call above already maps newTask
		// → "Task" via taxonomy.go, so no inline override is
		// needed here.
		return RoleAssistant, []ParsedToolCall{*tc}, nil
	case "command", "execute_command":
		cmdText := strings.TrimSpace(msg.Text)
		if cmdText == "" {
			return RoleAssistant, nil, nil
		}
		cmdName := "execute_command"
		if ask == "execute_command" {
			cmdName = "executeCommand"
		}
		inputMap := map[string]string{"command": cmdText}
		inputJSON, err := json.Marshal(inputMap)
		if err != nil {
			return RoleAssistant, nil, nil
		}
		return RoleAssistant, []ParsedToolCall{{
			ToolUseID: fmt.Sprintf(
				"kilo-legacy:%s:%d", cmdName, ordinal,
			),
			ToolName:  cmdName,
			Category:  "Bash",
			InputJSON: string(inputJSON),
		}}, nil
	case "completion_result":
		return RoleAssistant, nil, nil
	case "use_mcp_server":
		tc := parseKiloLegacyToolCall(msg.Text, ordinal)
		if tc == nil {
			return RoleAssistant, nil, nil
		}
		return RoleAssistant, []ParsedToolCall{*tc}, nil
	case "followup":
		return RoleAssistant, nil, nil
	}

	return RoleAssistant, nil, nil
}

// kiloTerminalTools are tool calls that signal the agent has
// explicitly ended the session rather than left a call pending a
// result. finishTask is Kilo's completion signal; a session ending
// on it is clean even though the tool call carries no tool_result.
var kiloTerminalTools = map[string]bool{
	"finishTask": true,
}

// kiloLastAssistantEndsWithTerminalTool reports whether the final
// non-system assistant message ends on a terminal tool call (e.g.
// finishTask). Such sessions are clean: the trailing tool call is
// the agent's own completion signal, not an interrupted call
// awaiting a result.
func kiloLastAssistantEndsWithTerminalTool(
	messages []ParsedMessage,
) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.IsSystem {
			continue
		}
		if m.Role != RoleAssistant {
			return false
		}
		if len(m.ToolCalls) == 0 {
			return false
		}
		return kiloTerminalTools[m.ToolCalls[len(m.ToolCalls)-1].ToolName]
	}
	return false
}

// classifyKiloLegacyTermination maps a parsed message stream to the
// standard TerminationStatus vocabulary. Kilo does not carry an
// explicit "status" field analog to RooCode's history_item, so
// the classification is always content-based. Orphaned tool
// calls and thinking-only endings flip to
// TerminationToolCallPending; sessions that have clean ending
// assistant text stay TerminationClean.
func classifyKiloLegacyTermination(
	messages []ParsedMessage,
) TerminationStatus {
	if len(messages) == 0 {
		return ""
	}
	// A trailing terminal tool call (finishTask) is the agent's
	// explicit completion signal, so the session is clean even
	// though the call has no tool_result.
	if kiloLastAssistantEndsWithTerminalTool(messages) {
		return TerminationClean
	}
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	if kiloLastMessageIsThinkingOnly(messages) {
		return TerminationToolCallPending
	}
	return TerminationClean
}

// kiloLastMessageIsThinkingOnly reports whether the last
// non-system message is an assistant message that contains only
// thinking/reasoning content with no substantive response and
// no tool calls — a strong signal that the session was
// interrupted mid-thought.
func kiloLastMessageIsThinkingOnly(messages []ParsedMessage) bool {
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

// kiloUnwrapJSONEnvelope extracts the primary text content from a JSON
// envelope. Followup messages carry {"question":"...","suggest":...} and
// completion_result messages carry {"suggest":"..."}. Returns the
// extracted text or empty string if parsing fails.
func kiloUnwrapJSONEnvelope(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '{' {
		return ""
	}
	var envelope struct {
		Question string          `json:"question"`
		Suggest  json.RawMessage `json:"suggest"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return ""
	}
	if envelope.Question != "" {
		return envelope.Question
	}
	// Suggest can be a string or an array; extract string value.
	var suggestStr string
	if err := json.Unmarshal(envelope.Suggest, &suggestStr); err == nil {
		return suggestStr
	}
	return ""
}

// kiloErrorTargets helpers now live in termination.go as the
// shared PendingToolErrorTargets / PairErrorToPendingTool so
// both Kilo (legacy) and RooCode (and any future Cline-family agent)
// share the same pairing logic.
// kiloIsMetadataSay reports whether the say type is internal
// metadata that should be skipped during transcript parsing.
func kiloIsMetadataSay(say string) bool {
	switch say {
	case "api_req_started", "api_req_deleted",
		"api_req_retried", "api_req_retry_delayed",
		"checkpoint_saved", "mcp_server_request_started":
		return true
	}
	return false
}

// kiloIsCompactBoundary reports whether the say type is a
// context management event that should be emitted as a compact
// boundary message for CompactionCount tracking.
func kiloIsCompactBoundary(say string) bool {
	switch say {
	case "condense_context", "sliding_window_truncation":
		return true
	}
	return false
}

// kiloIsErrorSay reports whether the say type is an error
// diagnostic that should be linked to the preceding tool call
// as a failure signal. shell_integration_warning is excluded
// because it's a non-fatal warning, not a tool failure.
func kiloIsErrorSay(say string) bool {
	switch say {
	case "error", "diff_error", "rooignore_error":
		return true
	}
	return false
}

// kiloIsToolErrorEvent reports whether the ask/say type indicates
// a tool or API failure that should be linked to the preceding
// tool call as an errored ResultEvent.
func kiloIsToolErrorEvent(say, ask string) bool {
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

// pendingToolErrorTargets bundles the pending-tool trackers an
// error event can attach to. Every tracker holds an index into
// parsedMessages of a tool call awaiting a result, or -1 when
// none is pending. cmd covers execute_command calls that get a
// separate command_output; mcp covers MCP invocations that get a
// separate mcp_server_response; newTask covers delegated child
// task calls that get a subtask_result; general covers any tool
// call with no specialised deferred-result channel so tool-
// specific errors (diff_error, rooignore_error) still find a
// target.
type pendingToolErrorTargets struct {
	cmd     *int
	mcp     *int
	newTask *int
	general *int
}

// pairErrorToPendingTool links an error event to the most recent
// unresolved tool call as an errored ResultEvent, preserving the
// error timestamp. Among the valid trackers it picks the one with
// the greatest message index: an error refers to whatever ran
// last, so an older pending command must not absorb a diff_error
// that belongs to a later edit. All trackers pointing at the
// paired message are cleared so the error is not double-counted.
// Returns true if pairing succeeded.
func pairErrorToPendingTool(
	parsedMessages []ParsedMessage,
	targets pendingToolErrorTargets,
	content string,
	ts time.Time,
) bool {
	idx := -1
	for _, t := range []*int{targets.cmd, targets.mcp, targets.newTask, targets.general} {
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
	for _, t := range []*int{targets.cmd, targets.mcp, targets.newTask, targets.general} {
		if *t == idx {
			*t = -1
		}
	}
	return true
}

// kiloResultBearingReadTools maps lowercase result-bearing tool
// names to the JSON payload fields that are execution results
// and must be stripped from InputJSON. Cline/RooCode payloads
// combine arguments and results in a single object; for the
// read-family, "content" is the tool output (file contents,
// directory listing, search hits), not an argument.
var kiloResultBearingReadTools = map[string][]string{
	"readfile":                {"content"},
	"listfiles":               {"content"},
	"listfilestoplevel":       {"content"},
	"listfilesrecursive":      {"content"},
	"listcodedefinitionnames": {"content"},
	"searchfiles":             {"content"},
}

// kiloToolResultContent extracts the embedded result content
// from a tool's JSON payload. Only read-family tools whose
// schemas define "content" as result data are included; write
// and edit tools use "content"/"diff" as input and must not be
// treated as completed results.
//
// present is true when the tool is a result-bearing tool AND
// the JSON carries an explicit "content" key, so an empty-but-
// present result still completes the call instead of leaving it
// pending.
func kiloToolResultContent(toolName, text string) (string, bool) {
	if _, ok := kiloResultBearingReadTools[strings.ToLower(toolName)]; !ok {
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

// kiloExtractAPIRequestStats decodes the per-request token and
// cost values stored in the text field of an api_req_started
// Cline message. Returns the full context window (input +
// cache reads + cache writes) for that request, the output
// token count, the summed cost, and whether cost was present.
// Treats an explicit cost of 0 as present (the provider reported
// 0; it is authoritative, not absent).
func kiloExtractAPIRequestStats(text string) (
	contextWindow int,
	inputTokens int,
	outputTokens int,
	cost float64,
	costPresent bool,
	provider string,
	cacheReads int,
	cacheWrites int,
) {
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return 0, 0, 0, 0, false, "", 0, 0
	}
	tokensIn := JSONFloatInt(data["tokensIn"])
	tokensOut := JSONFloatInt(data["tokensOut"])
	cacheReads = JSONFloatInt(data["cacheReads"])
	cacheWrites = JSONFloatInt(data["cacheWrites"])
	contextWindow = tokensIn + cacheReads + cacheWrites
	inputTokens = tokensIn

	// Cline records a cost even when the provider returned a free
	// response; missing fields are absent (not zero). Use raw
	// JSON access so a present 0 is distinguishable from absent.
	// An explicit cost value — including 0 — is authoritative:
	// usageMissing only indicates that the request did not return
	// usage data, but a recorded cost field stays reported.
	if raw, ok := data["cost"]; ok {
		if f, ok := raw.(float64); ok {
			cost = f
			costPresent = true
		}
	}
	// Kilo records only the inference provider name on each
	// request (e.g. "Z.AI", "Moonshot AI"); it is the closest
	// thing to a model label the format exposes.
	if p, ok := data["inferenceProvider"].(string); ok {
		provider = p
	}
	return contextWindow, inputTokens, tokensOut, cost, costPresent, provider, cacheReads, cacheWrites
}

// JSONFloatInt extracts a JSON-decoded numeric value as an int,
// returning 0 when the value is absent or not a number. Shared
// across parsers (RooCode, Kilo (legacy)).
func JSONFloatInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// kiloExitStatusRe and kiloErrorAnywhereRe are the command-
// output error patterns (matching roocode's behaviour): a
// permissive exit-code regex and multiline anchored patterns for
// strong error markers.
var kiloExitStatusRe = regexp.MustCompile(
	`(?i)exit\s+(?:code|status)\s*:?\s*([1-9]\d*)`,
)

var kiloErrorAnywhereRe = regexp.MustCompile(
	`(?im)` +
		`npm ERR!|` +
		`^\s*Error:\s|` +
		`^\s*Fatal:\s|` +
		`^\s*Failed:\s`,
)

// kiloCommandOutputIsError detects error patterns in command
// output text. Conservative: false negatives are preferred over
// false positives. The harness lesson applies here too — strong
// signal patterns like `npm ERR!` are reliable anywhere, but
// generic `error:` patterns need multiline anchoring.
func kiloCommandOutputIsError(output string) bool {
	if kiloExitStatusRe.MatchString(output) {
		return true
	}
	if kiloErrorAnywhereRe.MatchString(output) {
		return true
	}
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

// parseKiloLegacyToolCall extracts a ParsedToolCall from the JSON
// text of an ask="tool" message. Kilo emits two distinct payload
// shapes here:
//
//   - Cline/RooCode legacy: {"tool":"readFile","path":"src/foo.ts", ...}
//   - MCP (use_mcp_tool): {"type":"use_mcp_tool","serverName":"chrome-devtools",
//     "toolName":"take_snapshot","arguments":"{...}"}
//
// The MCP shape carries no "tool" key, so callers that only read
// toolData["tool"] silently drop every MCP call. We detect the
// "use_mcp_tool" type and build an mcp__<server>__<tool> ToolName
// (e.g. "mcp__chrome-devtools__take_snapshot"), matching the
// convention Claude, OpenCode and Zencoder use, so MCP calls
// classify and render consistently across agents.
//
// Result-only fields (per kiloResultBearingReadTools) are stripped
// before marshalling InputJSON, and SkillName is inferred for
// SKILL.md reads just like RooCode does.
func parseKiloLegacyToolCall(text string, ordinal int) *ParsedToolCall {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var toolData map[string]any
	if err := json.Unmarshal([]byte(text), &toolData); err != nil {
		return nil
	}
	toolName, _ := toolData["tool"].(string)
	// MCP calls carry the tool name under "toolName" with a
	// "type" envelope and no "tool" key. Handle both use_mcp_tool
	// and access_mcp_resource envelope types.
	if toolName == "" {
		if t, _ := toolData["type"].(string); t != "" {
			switch t {
			case "use_mcp_tool":
				if mcpName, _ := toolData["toolName"].(string); mcpName != "" {
					serverName, _ := toolData["serverName"].(string)
					tc := parseKiloLegacyMCPToolCall(
						mcpName, serverName, toolData, ordinal,
					)
					if tc == nil {
						return nil
					}
					tc.ToolUseID = fmt.Sprintf(
						"kilo-legacy:%s:%d", tc.ToolName, ordinal,
					)
					return tc
				}
			case "access_mcp_resource":
				serverName, _ := toolData["serverName"].(string)
				tc := parseKiloLegacyMCPToolCall(
					t, serverName, toolData, ordinal,
				)
				if tc == nil {
					return nil
				}
				tc.ToolUseID = fmt.Sprintf(
					"kilo-legacy:%s:%d", tc.ToolName, ordinal,
				)
				return tc
			}
		}
		return nil
	}
	for _, field := range kiloResultBearingReadTools[strings.ToLower(toolName)] {
		delete(toolData, field)
	}
	inputJSON, err := json.Marshal(toolData)
	if err != nil {
		return nil
	}
	toolUseID := fmt.Sprintf("kilo-legacy:%s:%d", toolName, ordinal)
	tc := &ParsedToolCall{
		ToolUseID: toolUseID,
		ToolName:  toolName,
		Category:  NormalizeToolCategory(toolName),
		InputJSON: string(inputJSON),
	}
	if strings.EqualFold(toolName, "skill") {
		tc.SkillName, _ = toolData["skill"].(string)
		if tc.SkillName == "" {
			tc.SkillName, _ = toolData["name"].(string)
		}
	} else {
		tc.SkillName = inferToolSkillName(toolName, tc.InputJSON)
	}
	// FilePath is exposed from the payload when present so the
	// frontend can route Edits / Writes to the right file even
	// if InputJSON is opaque.
	if p, ok := toolData["path"].(string); ok {
		tc.FilePath = p
	}
	return tc
}

// parseKiloLegacyMCPToolCall builds a ParsedToolCall for an MCP
// use_mcp_tool payload. The ToolName uses the mcp__<server>__<tool>
// form (e.g. "mcp__chrome-devtools__take_snapshot"), matching the
// convention used by Claude, OpenCode, Zencoder and the other
// harness parsers, so MCP calls classify and render consistently
// across agents. The raw "arguments" string is re-parsed to a JSON
// object for InputJSON; if it is missing or unparseable, the
// envelope text is used verbatim. Result-bearing stripping is
// skipped because MCP arguments are pure input with no embedded
// result fields.
func parseKiloLegacyMCPToolCall(
	toolName, serverName string, toolData map[string]any, ordinal int,
) *ParsedToolCall {
	qualified := toolName
	if serverName != "" {
		qualified = "mcp__" + serverName + "__" + toolName
	}
	// "arguments" may be a JSON string (the common Kilo shape) or an
	// already-decoded object. Normalise to a JSON object for InputJSON.
	inputJSON := buildKiloLegacyMCPInputJSON(toolData)
	tc := &ParsedToolCall{
		ToolName:  qualified,
		Category:  NormalizeToolCategory(qualified),
		InputJSON: inputJSON,
	}
	tc.SkillName = inferToolSkillName(qualified, inputJSON)
	return tc
}

// buildKiloLegacyMCPInputJSON renders the InputJSON for an MCP tool
// call. It prefers the decoded "arguments" object; when "arguments"
// is a JSON string it parses it, and when absent/unparseable it
// falls back to the full envelope minus the "type" envelope key.
func buildKiloLegacyMCPInputJSON(toolData map[string]any) string {
	if raw, ok := toolData["arguments"]; ok {
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				var probe any
				if err := json.Unmarshal([]byte(v), &probe); err == nil {
					return v
				}
			}
		case map[string]any, []any:
			if b, err := json.Marshal(v); err == nil {
				return string(b)
			}
		}
	}
	env := make(map[string]any, len(toolData))
	for k, val := range toolData {
		if k == "type" {
			continue
		}
		env[k] = val
	}
	if b, err := json.Marshal(env); err == nil {
		return string(b)
	}
	return "{}"
}

// absoluteParent returns the parent directory of an absolute
// path, returning "" for relative paths or paths whose parent
// is the current directory. Used to surface a project hint from
// task_metadata.files_in_context[0].path.
func absoluteParent(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	parent := filepath.Dir(p)
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return ""
	}
	return parent
}

// kiloLegacyFingerprintSource computes a composite fingerprint
// from task_metadata.json, ui_messages.json, and
// api_conversation_history.json so any of the three changing
// triggers a reparse.
func kiloLegacyFingerprintSource(path string) (SourceFingerprint, error) {
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
		h, "task_metadata", path, info,
	); err != nil {
		return SourceFingerprint{}, err
	}
	// Sibling files that contribute to composite freshness.
	dir := filepath.Dir(path)
	for _, name := range []string{
		"ui_messages.json",
		"api_conversation_history.json",
	} {
		sibPath := filepath.Join(dir, name)
		sibInfo, err := siblingMetadataFileInfo(sibPath)
		if err != nil {
			return SourceFingerprint{}, err
		}
		if sibInfo == nil {
			continue
		}
		fp.Size += sibInfo.Size()
		if ts := sibInfo.ModTime().UnixNano(); ts > fp.MTimeNS {
			fp.MTimeNS = ts
		}
		if err := addSiblingMetadataFingerprintPart(
			h, name, sibPath, sibInfo,
		); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fp.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fp, nil
}
