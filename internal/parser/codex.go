package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Codex JSONL entry types.
const (
	codexTypeSessionMeta  = "session_meta"
	codexTypeResponseItem = "response_item"
	codexTypeTurnContext  = "turn_context"
	codexTypeEventMsg     = "event_msg"
	codexOriginatorExec   = "codex_exec"
)

var errCodexIncrementalNeedsFullParse = errors.New(
	"codex subagent event requires full parse",
)

// codexSessionBuilder accumulates state while scanning a Codex
// JSONL session file line by line.
type codexSessionBuilder struct {
	messages                 []ParsedMessage
	firstMessage             string
	firstUserContent         string
	sawUserTurnAfterFirst    bool
	mayReplayFirstUserPrompt bool
	startedAt                time.Time
	endedAt                  time.Time
	sessionID                string
	project                  string
	ordinal                  int
	currentModel             string
	callNames                map[string]string
	callRefs                 map[string]codexToolCallRef
	agentSpawnCalls          map[string]string
	agentWaitCalls           map[string]string
	pendingAgentEvents       map[string][]codexPendingEvent
	orphanNotificationIx     map[string]int
	lastTokenUsageRaw        string // dedup streaming duplicates

	// Most recent task lifecycle event seen on the file. Used to
	// classify termination_status — task_complete maps to
	// "awaiting user input" while task_started in flight (no
	// matching task_complete after) means the agent was working
	// when the file was last written.
	lastTaskEvent string

	// Suppresses the parent history a forked rollout replays at the
	// top of the file, which would otherwise double count messages
	// and token usage across the parent and the fork (#643).
	forkGate codexForkGate
}

// codexForkGate drops the replayed parent history at the top of a
// forked Codex rollout (#643).
//
// `codex fork` copies the parent's lines — its session_meta, turns,
// messages and token_count events — into the new file with re-stamped
// envelope timestamps, so the same usage exists in two session files
// and gets counted twice. Envelope timestamps cannot locate the
// boundary (the replay is re-stamped at fork creation), but turn ids
// are UUIDv7 values minted when the turn originally ran: every
// replayed turn predates the fork instant, and the first genuine turn
// is minted at or after it. The gate stays closed until the first
// turn_context whose turn_id timestamp is >= the fork's own creation
// time, then everything flows normally.
//
// Replayed turn_context entries from parents recorded before Codex
// stamped turn ids carry no turn_id at all; a CLI new enough to write
// forked_from_id always stamps genuine turns, so a missing turn_id
// while gated means replayed history. An unparseable turn_id fails
// open (pre-#643 behaviour) rather than risk dropping live data.
type codexForkGate struct {
	active    bool
	createdMs int64
}

// armFromMeta activates the gate when the session_meta belongs to a
// forked session and its creation instant can be anchored: from the
// fork's UUIDv7 id, the payload timestamp, or the JSONL envelope
// timestamp, in that order.
func (g *codexForkGate) armFromMeta(payload gjson.Result, envelopeTS time.Time) {
	if payload.Get("forked_from_id").Str == "" {
		return
	}
	ms := uuidV7Millis(payload.Get("id").Str)
	if ms == 0 {
		if t := parseTimestamp(payload.Get("timestamp").Str); !t.IsZero() {
			ms = t.UnixMilli()
		}
	}
	if ms == 0 && !envelopeTS.IsZero() {
		ms = envelopeTS.UnixMilli()
	}
	if ms == 0 {
		return // no anchor for the boundary — fail open
	}
	g.active = true
	g.createdMs = ms
}

// suppresses reports whether the line is replayed parent history.
// turn_context lines open the gate when their turn id was minted at
// or after the fork instant.
func (g *codexForkGate) suppresses(lineType string, payload gjson.Result) bool {
	if !g.active {
		return false
	}
	if lineType != codexTypeTurnContext {
		return true
	}
	tid := payload.Get("turn_id").Str
	if tid == "" {
		return true // pre-turn_id parent history
	}
	if ms := uuidV7Millis(tid); ms != 0 && ms < g.createdMs {
		return true
	}
	g.active = false
	return false
}

// uuidV7Millis extracts the millisecond timestamp embedded in a
// UUIDv7, returning 0 for anything that is not a v7 UUID.
func uuidV7Millis(id string) int64 {
	hex := strings.ReplaceAll(id, "-", "")
	if len(hex) != 32 || hex[12] != '7' {
		return 0
	}
	ms, err := strconv.ParseInt(hex[:12], 16, 64)
	if err != nil {
		return 0
	}
	return ms
}

type codexToolCallRef struct {
	messageIndex int
	callIndex    int
}

type codexPendingEvent struct {
	agentID   string
	source    string
	status    string
	text      string
	timestamp time.Time
	ordinal   int
}

func newCodexSessionBuilder(
	_ bool,
) *codexSessionBuilder {
	return &codexSessionBuilder{
		project:              "unknown",
		callNames:            make(map[string]string),
		callRefs:             make(map[string]codexToolCallRef),
		agentSpawnCalls:      make(map[string]string),
		agentWaitCalls:       make(map[string]string),
		pendingAgentEvents:   make(map[string][]codexPendingEvent),
		orphanNotificationIx: make(map[string]int),
	}
}

// processLine handles a single non-empty, valid JSON line.
func (b *codexSessionBuilder) processLine(
	line string,
) (skip bool) {
	tsStr := gjson.Get(line, "timestamp").Str
	ts := parseTimestamp(tsStr)
	if ts.IsZero() {
		if tsStr != "" {
			logParseError(tsStr)
		}
	} else {
		if b.startedAt.IsZero() {
			b.startedAt = ts
		}
		b.endedAt = ts
	}

	payload := gjson.Get(line, "payload")

	switch gjson.Get(line, "type").Str {
	case codexTypeSessionMeta:
		if b.forkGate.active {
			// A forked rollout replays the parent's session_meta
			// too — the fork's own meta came first and wins.
			return false
		}
		return b.handleSessionMeta(payload, ts)
	case codexTypeTurnContext:
		if b.forkGate.suppresses(codexTypeTurnContext, payload) {
			return false
		}
		b.currentModel = payload.Get("model").Str
	case codexTypeResponseItem:
		if b.forkGate.suppresses(codexTypeResponseItem, payload) {
			return false
		}
		b.handleResponseItem(payload, ts)
	case codexTypeEventMsg:
		if b.forkGate.suppresses(codexTypeEventMsg, payload) {
			return false
		}
		b.handleEventMsg(payload)
	}
	return false
}

func (b *codexSessionBuilder) handleSessionMeta(
	payload gjson.Result, envelopeTS time.Time,
) (skip bool) {
	b.sessionID = payload.Get("id").Str

	if cwd := payload.Get("cwd").Str; cwd != "" {
		branch := payload.Get("git.branch").Str
		if proj := ExtractProjectFromCwdWithBranch(cwd, branch); proj != "" {
			b.project = proj
		} else {
			b.project = "unknown"
		}
	}

	b.forkGate.armFromMeta(payload, envelopeTS)

	return false
}

func (b *codexSessionBuilder) handleResponseItem(
	payload gjson.Result, ts time.Time,
) {
	switch payload.Get("type").Str {
	case "function_call":
		b.handleFunctionCall(payload, ts)
		return
	case "function_call_output":
		b.handleFunctionCallOutput(payload, ts)
		return
	}

	role := payload.Get("role").Str
	if role != "user" && role != "assistant" {
		return
	}

	content := extractCodexContent(payload)
	if strings.TrimSpace(content) == "" {
		return
	}

	if role == "user" && b.handleSubagentNotification(content, ts) {
		return
	}

	if role == "user" {
		if isCodexTurnAbortedMessage(content) {
			b.markFirstUserReplayPossible()
		}
		if isCodexSystemMessage(content) {
			return
		}
	}

	if role == "user" {
		switch {
		case b.firstUserContent == "":
			b.firstUserContent = content
			b.firstMessage = truncate(
				strings.ReplaceAll(content, "\n", " "), 300,
			)
		case content == b.firstUserContent:
			if !b.sawUserTurnAfterFirst &&
				b.mayReplayFirstUserPrompt {
				// Codex can re-emit the initial prompt verbatim after
				// a turn_aborted continuation signal. Drop only that
				// positively identified replay; otherwise an identical
				// second prompt is real transcript content. The match
				// is on full content, not the truncated preview.
				b.mayReplayFirstUserPrompt = false
				return
			}
			b.sawUserTurnAfterFirst = true
			b.mayReplayFirstUserPrompt = false
		default:
			b.sawUserTurnAfterFirst = true
			b.mayReplayFirstUserPrompt = false
		}
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleType(role),
		Content:       content,
		Timestamp:     ts,
		ContentLength: len(content),
		Model:         b.currentModel,
	})
	b.ordinal++
}

func (b *codexSessionBuilder) handleEventMsg(payload gjson.Result) {
	eventType := payload.Get("type").Str
	switch eventType {
	case "task_started", "task_complete", "turn_aborted":
		b.lastTaskEvent = eventType
		if eventType == "turn_aborted" {
			b.markFirstUserReplayPossible()
		}
	case "token_count":
		b.handleTokenCountEvent(payload)
	case "collab_agent_spawn_end":
		b.handleCollabAgentSpawnEnd(payload)
	}
}

func (b *codexSessionBuilder) markFirstUserReplayPossible() {
	if b.firstUserContent == "" || b.sawUserTurnAfterFirst {
		return
	}
	b.mayReplayFirstUserPrompt = true
}

func (b *codexSessionBuilder) handleTokenCountEvent(
	payload gjson.Result,
) {
	raw := payload.Get("info.last_token_usage").Raw
	if raw == "" || raw == b.lastTokenUsageRaw {
		return
	}
	b.lastTokenUsageRaw = raw

	// Find last assistant message without usage in the current
	// turn. Stop at user message boundary so we don't cross
	// turns.
	for i, v := range slices.Backward(b.messages) {
		if v.Role == RoleUser {
			break
		}
		if v.Role == RoleAssistant &&
			v.TokenUsage == nil {
			b.applyCodexTokenUsage(&b.messages[i], raw)
			return
		}
	}
}

func (b *codexSessionBuilder) handleCollabAgentSpawnEnd(
	payload gjson.Result,
) {
	callID := payload.Get("call_id").Str
	agentID := strings.TrimSpace(payload.Get("new_thread_id").Str)
	if callID == "" || agentID == "" {
		return
	}
	b.agentSpawnCalls[agentID] = callID
	b.setCallSubagentSessionID(callID, codexSubagentSessionID(agentID))
}

// applyCodexTokenUsage normalizes Codex token usage fields
// into the Anthropic-style shape expected by the usage and cost
// queries. Codex reports input_tokens as the full input count
// (cached portion included), while the downstream cost formula
// treats input_tokens as the uncached remainder and bills
// cache_read_input_tokens separately. Subtracting cached here
// prevents double-counting the cached portion at the full input
// rate.
//
//	input_tokens - cached_input_tokens → input_tokens  (uncached)
//	output_tokens                      → output_tokens
//	cached_input_tokens                → cache_read_input_tokens
func (b *codexSessionBuilder) applyCodexTokenUsage(
	msg *ParsedMessage, raw string,
) {
	usage := gjson.Parse(raw)
	totalInput := int(usage.Get("input_tokens").Int())
	cached := int(usage.Get("cached_input_tokens").Int())
	output := int(usage.Get("output_tokens").Int())

	uncached := max(totalInput-cached, 0)

	normalized := map[string]int{
		"input_tokens":            uncached,
		"output_tokens":           output,
		"cache_read_input_tokens": cached,
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	msg.TokenUsage = j
	msg.OutputTokens = output
	msg.HasOutputTokens = output > 0
	msg.ContextTokens = uncached + cached
	msg.HasContextTokens = totalInput > 0 || cached > 0
}

func (b *codexSessionBuilder) handleFunctionCall(
	payload gjson.Result, ts time.Time,
) {
	name := payload.Get("name").Str
	if name == "" {
		return
	}
	callID := payload.Get("call_id").Str
	if callID != "" {
		b.callNames[callID] = name
	}

	content := formatCodexFunctionCall(name, payload)
	inputJSON := extractCodexInputJSON(payload)
	waitAgentIDs := []string(nil)
	if isCodexWaitAgentCall(name) && callID != "" {
		args, _ := parseCodexFunctionArgs(payload)
		waitAgentIDs = codexWaitAgentIDs(args)
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleAssistant,
		Content:       content,
		Timestamp:     ts,
		HasToolUse:    true,
		ContentLength: len(content),
		Model:         b.currentModel,
		ToolCalls: []ParsedToolCall{{
			ToolUseID: callID,
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: inputJSON,
		}},
	})
	if callID != "" {
		b.callRefs[callID] = codexToolCallRef{
			messageIndex: len(b.messages) - 1,
			callIndex:    0,
		}
	}
	b.ordinal++

	if isCodexWaitAgentCall(name) && callID != "" {
		for _, agentID := range waitAgentIDs {
			b.agentWaitCalls[agentID] = callID
			b.claimPendingAgentEvents(callID, agentID)
		}
	}
}

func (b *codexSessionBuilder) handleFunctionCallOutput(
	payload gjson.Result, ts time.Time,
) {
	callID := payload.Get("call_id").Str
	if callID == "" {
		return
	}

	output, _ := parseCodexFunctionOutput(payload)
	if !output.Exists() {
		return
	}

	switch b.callNames[callID] {
	case "spawn_agent":
		agentID := strings.TrimSpace(output.Get("agent_id").Str)
		if agentID == "" {
			return
		}
		b.agentSpawnCalls[agentID] = callID
		b.setCallSubagentSessionID(callID, codexSubagentSessionID(agentID))
	case "wait", "wait_agent":
		status := output.Get("status")
		if !status.Exists() || !status.IsObject() {
			return
		}
		status.ForEach(func(key, entry gjson.Result) bool {
			agentID := key.Str
			statusName, text := codexTerminalSubagentEvent(entry)
			if text == "" {
				return true
			}
			b.appendCallResultEvent(callID, ParsedToolResultEvent{
				ToolUseID:         callID,
				AgentID:           agentID,
				SubagentSessionID: codexSubagentSessionID(agentID),
				Source:            "wait_output",
				Status:            statusName,
				Content:           text,
				Timestamp:         ts,
			})
			return true
		})
	}
}

// setCallSubagentSessionID links a tool call to the session of
// the subagent it spawned. Callers must invoke this only after
// the originating function_call has been processed (which
// populates b.callRefs[callID]); otherwise the link is silently
// dropped. In real codex session files the spawn function_call
// always precedes both its function_call_output and the
// collab_agent_spawn_end event_msg.
func (b *codexSessionBuilder) setCallSubagentSessionID(
	callID, sessionID string,
) {
	if callID == "" || sessionID == "" {
		return
	}
	ref, ok := b.callRefs[callID]
	if !ok || ref.messageIndex < 0 || ref.messageIndex >= len(b.messages) {
		return
	}
	if ref.callIndex < 0 || ref.callIndex >= len(b.messages[ref.messageIndex].ToolCalls) {
		return
	}
	b.messages[ref.messageIndex].ToolCalls[ref.callIndex].SubagentSessionID = sessionID
}

func (b *codexSessionBuilder) handleSubagentNotification(
	content string, ts time.Time,
) bool {
	agentID, statusName, text := parseCodexSubagentNotification(content)
	if agentID == "" || text == "" {
		return false
	}
	if callID := b.agentWaitCalls[agentID]; callID != "" {
		b.appendCallResultEvent(callID, ParsedToolResultEvent{
			AgentID:           agentID,
			SubagentSessionID: codexSubagentSessionID(agentID),
			Source:            "subagent_notification",
			Status:            statusName,
			Content:           text,
			Timestamp:         ts,
		})
		return true
	}

	b.pendingAgentEvents[agentID] = append(
		b.pendingAgentEvents[agentID], codexPendingEvent{
			agentID:   agentID,
			source:    "subagent_notification",
			status:    statusName,
			text:      text,
			timestamp: ts,
			ordinal:   b.ordinal,
		},
	)
	b.ordinal++
	return true
}

func (b *codexSessionBuilder) appendCallResultEvent(
	callID string, ev ParsedToolResultEvent,
) {
	if callID == "" {
		return
	}
	ref, ok := b.callRefs[callID]
	if !ok || ref.messageIndex < 0 || ref.messageIndex >= len(b.messages) {
		return
	}
	if ref.callIndex < 0 || ref.callIndex >= len(b.messages[ref.messageIndex].ToolCalls) {
		return
	}
	tc := &b.messages[ref.messageIndex].ToolCalls[ref.callIndex]
	if ev.ToolUseID == "" {
		ev.ToolUseID = tc.ToolUseID
	}
	if ev.SubagentSessionID == "" && ev.AgentID != "" {
		ev.SubagentSessionID = codexSubagentSessionID(ev.AgentID)
	}
	if b.hasEquivalentCallResultEvent(tc.ResultEvents, ev) {
		return
	}
	tc.ResultEvents = append(tc.ResultEvents, ev)
}

func (b *codexSessionBuilder) hasEquivalentCallResultEvent(
	events []ParsedToolResultEvent, candidate ParsedToolResultEvent,
) bool {
	for _, existing := range events {
		if existing.AgentID == candidate.AgentID &&
			existing.Status == candidate.Status &&
			existing.Content == candidate.Content {
			return true
		}
	}
	return false
}

func (b *codexSessionBuilder) claimPendingAgentEvents(
	callID, agentID string,
) {
	pending := b.pendingAgentEvents[agentID]
	if len(pending) == 0 {
		return
	}
	for _, ev := range pending {
		b.appendCallResultEvent(callID, ParsedToolResultEvent{
			AgentID:           ev.agentID,
			SubagentSessionID: codexSubagentSessionID(ev.agentID),
			Source:            ev.source,
			Status:            ev.status,
			Content:           ev.text,
			Timestamp:         ev.timestamp,
		})
	}
	delete(b.pendingAgentEvents, agentID)
}

func (b *codexSessionBuilder) flushPendingAgentResults() {
	if len(b.pendingAgentEvents) == 0 {
		return
	}
	agentIDs := make([]string, 0, len(b.pendingAgentEvents))
	for agentID := range b.pendingAgentEvents {
		agentIDs = append(agentIDs, agentID)
	}
	sort.Strings(agentIDs)
	for _, agentID := range agentIDs {
		pending := b.pendingAgentEvents[agentID]
		switch {
		case b.agentWaitCalls[agentID] != "":
			b.claimPendingAgentEvents(b.agentWaitCalls[agentID], agentID)
		case b.agentSpawnCalls[agentID] != "":
			b.claimPendingAgentEvents(b.agentSpawnCalls[agentID], agentID)
		default:
			for _, ev := range pending {
				key := agentID + "\x00" + ev.status + "\x00" + ev.text
				if _, ok := b.orphanNotificationIx[key]; ok {
					continue
				}
				idx := b.insertMessage(ParsedMessage{
					Ordinal:       ev.ordinal,
					Role:          RoleUser,
					Content:       ev.text,
					Timestamp:     ev.timestamp,
					Model:         b.currentModel,
					ContentLength: len(ev.text),
				})
				b.orphanNotificationIx[key] = idx
			}
			delete(b.pendingAgentEvents, agentID)
		}
	}
}

func codexSubagentSessionID(agentID string) string {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return ""
	}
	if strings.HasPrefix(agentID, "codex:") {
		return agentID
	}
	return "codex:" + agentID
}

func (b *codexSessionBuilder) normalizeOrdinals() {
	sort.SliceStable(b.messages, func(i, j int) bool {
		if b.messages[i].Ordinal == b.messages[j].Ordinal {
			return i < j
		}
		return b.messages[i].Ordinal < b.messages[j].Ordinal
	})
	for i := range b.messages {
		b.messages[i].Ordinal = i
	}
}

func (b *codexSessionBuilder) insertMessage(msg ParsedMessage) int {
	idx := len(b.messages)
	for i, existing := range b.messages {
		if existing.Ordinal > msg.Ordinal ||
			(existing.Ordinal == msg.Ordinal &&
				!msg.Timestamp.IsZero() &&
				(existing.Timestamp.IsZero() ||
					msg.Timestamp.Before(existing.Timestamp))) {
			idx = i
			break
		}
	}
	b.messages = append(b.messages, ParsedMessage{})
	copy(b.messages[idx+1:], b.messages[idx:])
	b.messages[idx] = msg
	for callID, ref := range b.callRefs {
		if ref.messageIndex >= idx {
			ref.messageIndex++
			b.callRefs[callID] = ref
		}
	}
	return idx
}

func formatCodexFunctionCall(
	name string, payload gjson.Result,
) string {
	summary := sanitizeToolLabel(payload.Get("summary").Str)
	args, rawArgs := parseCodexFunctionArgs(payload)

	switch name {
	case "exec_command", "shell_command", "shell":
		return formatCodexBashCall(summary, args, rawArgs)
	case "write_stdin":
		return formatCodexWriteStdinCall(summary, args, rawArgs)
	case "apply_patch":
		return formatCodexApplyPatchCall(summary, args, rawArgs)
	case "spawn_agent":
		return formatCodexSpawnAgentCall(summary, args, rawArgs)
	}

	category := NormalizeToolCategory(name)
	if category == "Other" {
		header := formatToolHeader("Tool", name)
		if summary != "" {
			return header + "\n" + summary
		}
		if preview := codexArgPreview(args, rawArgs); preview != "" {
			return header + "\n" + preview
		}
		return header
	}

	detail := firstNonEmpty(summary,
		codexCategoryDetail(category, args))
	header := formatToolHeader(category, detail)
	if preview := codexArgPreview(args, rawArgs); preview != "" {
		return header + "\n" + preview
	}
	return header
}

func parseCodexFunctionArgs(
	payload gjson.Result,
) (gjson.Result, string) {
	for _, key := range []string{"arguments", "input"} {
		arg := payload.Get(key)
		if !arg.Exists() {
			continue
		}

		switch arg.Type {
		case gjson.String:
			s := strings.TrimSpace(arg.Str)
			if s == "" {
				continue
			}
			if gjson.Valid(s) {
				return gjson.Parse(s), ""
			}
			return gjson.Result{}, s
		default:
			if arg.IsObject() {
				if len(arg.Map()) == 0 {
					continue
				}
				return arg, ""
			}
			if arg.IsArray() {
				if len(arg.Array()) == 0 {
					continue
				}
				return arg, ""
			}
			raw := strings.TrimSpace(arg.Raw)
			if raw == "" {
				continue
			}
			if gjson.Valid(raw) {
				return gjson.Parse(raw), ""
			}
			return gjson.Result{}, raw
		}
	}
	return gjson.Result{}, ""
}

// extractCodexInputJSON returns the raw JSON string of the
// function call arguments from the payload. It checks
// "arguments" then "input", normalizing string-encoded JSON
// to an object string.
func extractCodexInputJSON(payload gjson.Result) string {
	for _, key := range []string{"arguments", "input"} {
		arg := payload.Get(key)
		if !arg.Exists() {
			continue
		}

		switch arg.Type {
		case gjson.String:
			s := strings.TrimSpace(arg.Str)
			if s == "" {
				continue
			}
			if gjson.Valid(s) {
				if s == "{}" || s == "[]" {
					continue
				}
				return s
			}
			return arg.Str
		default:
			raw := strings.TrimSpace(arg.Raw)
			if raw == "" || raw == "{}" || raw == "[]" {
				continue
			}
			return arg.Raw
		}
	}
	return ""
}

func formatCodexBashCall(
	summary string, args gjson.Result, rawArgs string,
) string {
	cmd := codexArgValue(args, "cmd", "command")
	if cmd == "" && rawArgs != "" && !gjson.Valid(rawArgs) {
		cmd = rawArgs
	}
	if cmd == "" && args.Type == gjson.String {
		cmd = strings.TrimSpace(args.Str)
	}

	header := formatToolHeader("Bash", summary)
	if cmd != "" {
		firstLine, _, hasMore := strings.Cut(cmd, "\n")
		if hasMore {
			return header + "\n$ " + firstLine
		}
		return header + "\n$ " + cmd
	}
	if preview := codexArgPreview(args, rawArgs); preview != "" {
		return header + "\n" + preview
	}
	return header
}

func formatCodexWriteStdinCall(
	summary string, args gjson.Result, rawArgs string,
) string {
	if summary == "" {
		if sid := codexArgValue(args, "session_id"); sid != "" {
			summary = "stdin -> " + sid
		} else {
			summary = "stdin"
		}
	}

	header := formatToolHeader("Bash", summary)
	chars := codexArgString(args, "chars")
	if chars != "" {
		quoted := strings.Trim(
			strconv.QuoteToASCII(chars), "\"",
		)
		return header + "\n" + truncate(quoted, 220)
	}

	if preview := codexArgPreview(args, rawArgs); preview != "" {
		return header + "\n" + preview
	}
	return header
}

func formatCodexApplyPatchCall(
	summary string, args gjson.Result, rawArgs string,
) string {
	patch := codexArgString(args, "patch")
	if patch == "" && strings.Contains(rawArgs, "*** Begin Patch") {
		patch = rawArgs
	}

	files := extractPatchedFiles(patch)
	if summary == "" {
		summary = summarizePatchedFiles(files)
	}

	header := formatToolHeader("Edit", summary)
	if len(files) > 1 {
		limit := min(len(files), 6)
		body := strings.Join(files[:limit], "\n")
		if len(files) > limit {
			body += fmt.Sprintf("\n+%d more files", len(files)-limit)
		}
		return header + "\n" + body
	}
	if preview := codexArgPreview(args, rawArgs); preview != "" &&
		len(files) == 0 {
		return header + "\n" + preview
	}
	return header
}

func formatCodexSpawnAgentCall(
	summary string, args gjson.Result, rawArgs string,
) string {
	if summary == "" {
		summary = firstNonEmpty(
			codexArgValue(args, "agent_type"),
			codexArgValue(args, "subagent_type"),
			"spawn_agent",
		)
	}

	header := formatToolHeader("Task", summary)
	prompt := firstNonEmpty(
		codexArgValue(args, "description"),
		codexArgValue(args, "message"),
		codexArgValue(args, "prompt"),
	)
	if prompt != "" {
		firstLine, _, _ := strings.Cut(prompt, "\n")
		return header + "\n" + truncate(firstLine, 220)
	}
	if preview := codexArgPreview(args, rawArgs); preview != "" {
		return header + "\n" + preview
	}
	return header
}

func extractPatchedFiles(patch string) []string {
	if patch == "" {
		return nil
	}

	var files []string
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(patch, "\n") {
		for _, prefix := range []string{
			"*** Add File: ",
			"*** Update File: ",
			"*** Delete File: ",
			"*** Move to: ",
		} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			file := strings.TrimSpace(
				strings.TrimPrefix(line, prefix),
			)
			if file == "" {
				continue
			}
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			files = append(files, file)
			break
		}
	}
	return files
}

func summarizePatchedFiles(files []string) string {
	switch len(files) {
	case 0:
		return ""
	case 1:
		return files[0]
	default:
		return fmt.Sprintf(
			"%s (+%d more)",
			files[0], len(files)-1,
		)
	}
}

func codexCategoryDetail(
	category string, args gjson.Result,
) string {
	switch category {
	case "Read", "Write", "Edit":
		return codexArgValue(args, "file_path", "path")
	case "Grep":
		return codexArgValue(args, "pattern")
	case "Glob":
		pattern := codexArgValue(args, "pattern")
		path := codexArgValue(args, "path")
		if pattern != "" && path != "" {
			return fmt.Sprintf("%s in %s", pattern, path)
		}
		return firstNonEmpty(pattern, path)
	case "Task", "Agent":
		desc := codexArgValue(args, "description")
		agent := codexArgValue(args, "subagent_type")
		if desc != "" && agent != "" {
			return fmt.Sprintf("%s (%s)", desc, agent)
		}
		return firstNonEmpty(desc, agent)
	default:
		return ""
	}
}

func codexArgString(
	args gjson.Result, path string,
) string {
	v := args.Get(path)
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.Str
	}
	raw := strings.TrimSpace(v.Raw)
	if raw == "" || raw == "null" {
		return ""
	}
	return raw
}

func codexArgValue(
	args gjson.Result, paths ...string,
) string {
	for _, path := range paths {
		v := strings.TrimSpace(codexArgString(args, path))
		if v != "" {
			return v
		}
	}
	return ""
}

func codexArgPreview(
	args gjson.Result, rawArgs string,
) string {
	if rawArgs != "" {
		flat := strings.Join(
			strings.Fields(rawArgs), " ",
		)
		return truncate(flat, 220)
	}
	if args.Exists() {
		flat := strings.Join(
			strings.Fields(args.Raw), " ",
		)
		if flat != "" {
			return truncate(flat, 220)
		}
	}
	return ""
}

func formatToolHeader(
	label, detail string,
) string {
	label = sanitizeToolLabel(label)
	if label == "" {
		label = "Tool"
	}
	detail = sanitizeToolLabel(detail)
	if detail != "" {
		return fmt.Sprintf("[%s: %s]", label, detail)
	}
	return fmt.Sprintf("[%s]", label)
}

func sanitizeToolLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "]", ")")
	return strings.Join(strings.Fields(s), " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func parseCodexFunctionOutput(
	payload gjson.Result,
) (gjson.Result, string) {
	out := payload.Get("output")
	if !out.Exists() {
		return gjson.Result{}, ""
	}

	switch out.Type {
	case gjson.String:
		s := strings.TrimSpace(out.Str)
		if s == "" {
			return gjson.Result{}, ""
		}
		if gjson.Valid(s) {
			return gjson.Parse(s), s
		}
		return gjson.Result{}, s
	default:
		raw := strings.TrimSpace(out.Raw)
		if raw == "" {
			return gjson.Result{}, ""
		}
		if gjson.Valid(raw) {
			return gjson.Parse(raw), raw
		}
		return gjson.Result{}, raw
	}
}

func codexWaitAgentIDs(args gjson.Result) []string {
	if !args.Exists() {
		return nil
	}
	ids := args.Get("ids")
	if !ids.Exists() {
		ids = args.Get("targets")
	}
	if !ids.Exists() || !ids.IsArray() {
		return nil
	}

	var out []string
	for _, item := range ids.Array() {
		id := strings.TrimSpace(item.Str)
		if id == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}

func isCodexWaitAgentCall(name string) bool {
	return name == "wait" || name == "wait_agent"
}

func parseCodexSubagentNotification(
	content string,
) (agentID, statusName, text string) {
	if !isCodexSubagentNotification(content) {
		return "", "", ""
	}
	body := strings.TrimSpace(content)
	body = strings.TrimPrefix(body, "<subagent_notification>")
	body = strings.TrimSuffix(body, "</subagent_notification>")
	body = strings.TrimSpace(body)
	if !gjson.Valid(body) {
		return "", "", ""
	}
	parsed := gjson.Parse(body)
	agentID = firstNonEmpty(
		parsed.Get("agent_id").Str,
		parsed.Get("agent_path").Str,
	)
	status := parsed.Get("status")
	statusName, text = codexTerminalSubagentEvent(status)
	return agentID, statusName, text
}

func codexTerminalSubagentEvent(status gjson.Result) (string, string) {
	if text := strings.TrimSpace(status.Get("completed").Str); text != "" {
		return "completed", text
	}
	if text := strings.TrimSpace(status.Get("errored").Str); text != "" {
		return "errored", text
	}
	if text := strings.TrimSpace(status.Get("running").Str); text != "" {
		return "running", text
	}
	return "", ""
}

func codexTerminalSubagentStatus(status gjson.Result) string {
	_, text := codexTerminalSubagentEvent(status)
	return text
}

func isCodexSubagentFunctionOutput(output gjson.Result) bool {
	if !output.Exists() {
		return false
	}
	if strings.TrimSpace(output.Get("agent_id").Str) != "" {
		return true
	}

	status := output.Get("status")
	if !status.Exists() || !status.IsObject() {
		return false
	}
	entries := status.Map()
	if len(entries) == 0 {
		return false
	}
	for agentID, entry := range entries {
		if strings.TrimSpace(agentID) == "" || !entry.IsObject() {
			return false
		}
		if codexTerminalSubagentStatus(entry) != "" {
			continue
		}
		if strings.TrimSpace(entry.Get("running").Str) != "" {
			continue
		}
		return false
	}
	return true
}

// extractCodexContent joins all text blocks from a Codex
// response item's content array.
func extractCodexContent(payload gjson.Result) string {
	var texts []string
	payload.Get("content").ForEach(
		func(_, block gjson.Result) bool {
			switch block.Get("type").Str {
			case "input_text", "output_text", "text":
				if t := block.Get("text").Str; t != "" {
					texts = append(texts, t)
				}
			}
			return true
		},
	)
	return strings.Join(texts, "\n")
}

// IsCodexExecSessionFile reports whether any session_meta
// line in a Codex JSONL file has originator=="codex_exec".
// The pre-bulk-sync parser called handleSessionMeta on every
// session_meta line and flagged the whole session as exec if
// any of them carried that originator, so a one-shot check
// of only the first session_meta would miss files that were
// originally skipped because a later session_meta set the
// originator. Scan all session_meta lines to match the old
// skip condition exactly.
func IsCodexExecSessionFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || !gjson.Valid(line) {
			continue
		}
		if gjson.Get(line, "type").Str != codexTypeSessionMeta {
			continue
		}
		if gjson.Get(line, "payload.originator").Str ==
			codexOriginatorExec {
			return true
		}
	}
	return false
}

// ParseCodexSession parses a Codex JSONL session file.
// The includeExec parameter is retained for backward
// compatibility; exec-originated sessions are now always
// parsed and imported.
func ParseCodexSession(
	path, machine string, includeExec bool,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	b := newCodexSessionBuilder(includeExec)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		if b.processLine(line) {
			return nil, nil, nil
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil,
			fmt.Errorf("reading codex %s: %w", path, err)
	}

	b.flushPendingAgentResults()
	b.normalizeOrdinals()

	sessionID := b.sessionID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(
			filepath.Base(path), ".jsonl",
		)
	}
	sessionID = "codex:" + sessionID

	userCount := 0
	for _, m := range b.messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:                sessionID,
		Project:           b.project,
		Machine:           machine,
		Agent:             AgentCodex,
		FirstMessage:      b.firstMessage,
		StartedAt:         b.startedAt,
		EndedAt:           b.endedAt,
		MessageCount:      len(b.messages),
		UserMessageCount:  userCount,
		TerminationStatus: classifyCodexTermination(b.lastTaskEvent),
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	accumulateMessageTokenUsage(sess, b.messages)

	return sess, b.messages, nil
}

// classifyCodexTermination maps the most recent task lifecycle
// event seen on a Codex session file to a TerminationStatus.
// Codex emits explicit task_started / task_complete / turn_aborted
// events, so the classification is unambiguous when any are
// present. Returns "" (unknown) for files where no task event
// was seen — typically very short or malformed sessions.
func classifyCodexTermination(lastTaskEvent string) TerminationStatus {
	switch lastTaskEvent {
	case "task_complete":
		return TerminationAwaitingUser
	case "task_started", "turn_aborted":
		// task_started without a matching task_complete after
		// means the agent was mid-turn when the file last
		// flushed — treat the same as an orphan tool call.
		// turn_aborted means the user interrupted; same shape.
		return TerminationToolCallPending
	}
	return ""
}

// codexIncrementalSeed carries the builder state recovered from the
// already-parsed prefix [0, offset) of a Codex JSONL file so an
// incremental parse resumes with the same view a full parse would
// have at that offset: the current model, the re-emitted-prompt
// dedup state, and the fork replay gate (#643).
type codexIncrementalSeed struct {
	model                    string
	firstUserContent         string
	sawUserTurnAfterFirst    bool
	mayReplayFirstUserPrompt bool
	forkGate                 codexForkGate
}

// seedCodexIncrementalState scans a Codex JSONL prefix [0, offset)
// and mirrors processLine's dispatch: every turn_context overwrites
// the model (including empty strings), user messages feed the
// re-emitted-prompt dedup exactly as handleResponseItem would, and
// the fork gate arms/opens on the same lines as a full parse. A gate
// still active at the end of the scan means the stored offset landed
// inside the replayed parent history of a forked rollout, so the
// incremental parse must keep suppressing appended replay lines.
func seedCodexIncrementalState(
	path string, offset int64,
) codexIncrementalSeed {
	var seed codexIncrementalSeed
	f, err := os.Open(path)
	if err != nil {
		return seed
	}
	defer f.Close()

	lr := newLineReader(io.LimitReader(f, offset), maxLineSize)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		lineType := gjson.Get(line, "type").Str
		payload := gjson.Get(line, "payload")
		if lineType == codexTypeSessionMeta {
			// Mirror processLine: the fork's own meta arms the
			// gate, and replayed parent metas are dropped while
			// it is active.
			if !seed.forkGate.active {
				seed.forkGate.armFromMeta(
					payload,
					parseTimestamp(gjson.Get(line, "timestamp").Str),
				)
			}
			continue
		}
		if seed.forkGate.suppresses(lineType, payload) {
			continue
		}
		switch lineType {
		case codexTypeTurnContext:
			seed.model = payload.Get("model").Str
		case codexTypeEventMsg:
			if payload.Get("type").Str == "turn_aborted" &&
				seed.firstUserContent != "" &&
				!seed.sawUserTurnAfterFirst {
				seed.mayReplayFirstUserPrompt = true
			}
		case codexTypeResponseItem:
			seed.observeUserMessage(payload)
		}
	}
	return seed
}

// observeUserMessage feeds one response_item into the
// re-emitted-prompt dedup state, mirroring handleResponseItem's
// user-message filtering and full-content matching.
func (s *codexIncrementalSeed) observeUserMessage(
	payload gjson.Result,
) {
	if payload.Get("role").Str != "user" {
		return
	}
	content := extractCodexContent(payload)
	if strings.TrimSpace(content) == "" {
		return
	}
	if isCodexTurnAbortedMessage(content) &&
		s.firstUserContent != "" &&
		!s.sawUserTurnAfterFirst {
		s.mayReplayFirstUserPrompt = true
	}
	if isCodexSystemMessage(content) {
		return
	}
	switch {
	case s.firstUserContent == "":
		s.firstUserContent = content
	case content == s.firstUserContent &&
		!s.sawUserTurnAfterFirst &&
		s.mayReplayFirstUserPrompt:
		s.mayReplayFirstUserPrompt = false
	case content == s.firstUserContent:
		s.sawUserTurnAfterFirst = true
		s.mayReplayFirstUserPrompt = false
	default:
		s.sawUserTurnAfterFirst = true
		s.mayReplayFirstUserPrompt = false
	}
}

// ParseCodexSessionFrom parses only new lines from a Codex
// JSONL file starting at the given byte offset. Returns only
// the newly parsed messages (with ordinals starting at
// startOrdinal) and the latest timestamp seen. Used for
// incremental re-parsing of large append-only session files.
func ParseCodexSessionFrom(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
) ([]ParsedMessage, time.Time, int64, error) {
	b := newCodexSessionBuilder(includeExec)
	b.ordinal = startOrdinal
	// Recover model, re-emitted-prompt dedup state, and the fork
	// replay gate from the already-parsed prefix so appended lines —
	// including a replay that spans the stored offset — are handled
	// just as a full parse would.
	seed := seedCodexIncrementalState(path, offset)
	b.currentModel = seed.model
	b.firstUserContent = seed.firstUserContent
	b.sawUserTurnAfterFirst = seed.sawUserTurnAfterFirst
	b.mayReplayFirstUserPrompt = seed.mayReplayFirstUserPrompt
	b.forkGate = seed.forkGate
	var fallbackErr error

	consumed, err := readJSONLFrom(
		path, offset, func(line string) {
			if fallbackErr != nil {
				return
			}
			// Skip session_meta — already processed in
			// the initial full parse.
			if gjson.Get(line, "type").Str ==
				codexTypeSessionMeta {
				return
			}
			if codexIncrementalNeedsFullParse(line) {
				fallbackErr = errCodexIncrementalNeedsFullParse
				return
			}
			b.processLine(line)
		},
	)
	if err != nil {
		return nil, time.Time{}, 0, fmt.Errorf(
			"reading codex %s from offset %d: %w",
			path, offset, err,
		)
	}
	if fallbackErr != nil {
		return nil, time.Time{}, 0, fallbackErr
	}

	b.flushPendingAgentResults()

	return b.messages, b.endedAt, consumed, nil
}

// IsIncrementalFullParseFallback reports whether an incremental
// parse error requires the caller to fall back to a full parse.
func IsIncrementalFullParseFallback(err error) bool {
	return errors.Is(err, errCodexIncrementalNeedsFullParse) ||
		errors.Is(err, ErrClaudeIncrementalNeedsFullParse)
}

func isCodexSystemMessage(content string) bool {
	trimmed := strings.TrimSpace(content)
	return strings.HasPrefix(content, "# AGENTS.md") ||
		strings.HasPrefix(content, "<environment_context>") ||
		strings.HasPrefix(content, "<INSTRUCTIONS>") ||
		isCodexTurnAbortedMessage(content) ||
		strings.HasPrefix(trimmed, "<skill>") ||
		isCodexSubagentNotification(content)
}

func isCodexTurnAbortedMessage(content string) bool {
	return strings.HasPrefix(
		strings.TrimSpace(content),
		"<turn_aborted>",
	)
}

func isCodexSubagentNotification(content string) bool {
	return strings.HasPrefix(
		strings.TrimSpace(content),
		"<subagent_notification>",
	)
}

func codexIncrementalNeedsFullParse(line string) bool {
	switch gjson.Get(line, "type").Str {
	case codexTypeEventMsg:
		return gjson.Get(line, "payload.type").Str ==
			"collab_agent_spawn_end"
	case codexTypeResponseItem:
	default:
		return false
	}

	payload := gjson.Get(line, "payload")
	switch payload.Get("type").Str {
	case "function_call":
		return isCodexWaitAgentCall(payload.Get("name").Str)
	case "function_call_output":
		output, _ := parseCodexFunctionOutput(payload)
		return isCodexSubagentFunctionOutput(output)
	default:
		role := payload.Get("role").Str
		if role != "user" {
			return false
		}
		agentID, _, text := parseCodexSubagentNotification(
			extractCodexContent(payload),
		)
		return agentID != "" && text != ""
	}
}
