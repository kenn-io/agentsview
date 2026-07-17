package parser

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	"codex incremental event requires full parse",
)

const codexGoalContextSourceAttr = `source="goal"`

var codexGoalContextSourceAttrRe = regexp.MustCompile(`(?:^|\s)` +
	regexp.QuoteMeta(codexGoalContextSourceAttr) + `(?:\s|/|$)`)

var codexSessionIndexCache = struct {
	mu      sync.Mutex
	entries map[string]codexSessionIndexEntry
}{
	entries: make(map[string]codexSessionIndexEntry),
}

type codexSessionIndexEntry struct {
	mtime          int64
	size           int64
	changeTime     int64
	changeTimeOkay bool
	titles         map[string]string
}

// codexSessionBuilder accumulates state while scanning a Codex
// JSONL session file line by line.
type codexSessionBuilder struct {
	codexCursorState
	messages     []ParsedMessage
	firstMessage string
	// firstMessagePlaceholder marks a firstMessage that stands in for an
	// encrypted inter-agent task, so title derivation can still prefer
	// the agent path over the placeholder text.
	firstMessagePlaceholder bool
	startedAt               time.Time
	endedAt                 time.Time
	sessionID               string
	parentSessionID         string
	relationshipType        RelationshipType
	project                 string
	ordinal                 int
	callNames               map[string]string
	callRefs                map[string]codexToolCallRef
	agentSpawnCalls         map[string]string
	agentWaitCalls          map[string]string
	pendingAgentEvents      map[string][]codexPendingEvent
	orphanNotificationIx    map[string]int
	unattachedTokenUsage    bool
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

func (b *codexSessionBuilder) incrementalSeed() codexIncrementalSeed {
	return b.codexCursorState
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
		b.model = payload.Get("model").Str
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
	b.agentPath = strings.TrimSpace(payload.Get("agent_path").Str)
	if b.agentPath == "" {
		b.agentPath = strings.TrimSpace(
			payload.Get("source.subagent.thread_spawn.agent_path").Str,
		)
	}
	b.parentSessionID = strings.TrimSpace(
		payload.Get("source.subagent.thread_spawn.parent_thread_id").Str,
	)
	if b.parentSessionID == "" &&
		payload.Get("thread_source").Str == "subagent" {
		b.parentSessionID = strings.TrimSpace(
			payload.Get("parent_thread_id").Str,
		)
	}
	if b.parentSessionID != "" {
		b.parentSessionID = codexSubagentSessionID(b.parentSessionID)
		b.relationshipType = RelSubagent
	}

	if cwd := payload.Get("cwd").Str; cwd != "" {
		b.cwd = cwd
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
	case "function_call", "custom_tool_call":
		b.handleFunctionCall(payload, ts)
		return
	case "function_call_output", "custom_tool_call_output":
		b.handleFunctionCallOutput(payload, ts)
		return
	case "agent_message":
		b.handleAgentMessage(payload, ts)
		return
	}

	role := payload.Get("role").Str
	if role != "user" && role != "assistant" {
		return
	}

	content := extractCodexContent(payload)
	if role == "user" && !b.firstUserSeen {
		content = extractCodexInitialUserContent(payload)
	}
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
		first, replay := b.observeUserPrompt(content)
		if first {
			b.firstMessage = truncate(
				strings.ReplaceAll(content, "\n", " "), 300,
			)
		}
		if replay {
			// Codex can re-emit the initial prompt verbatim after a
			// turn_aborted continuation signal. Drop only that positively
			// identified replay; otherwise an identical second prompt is
			// real transcript content. The digest covers the full content,
			// not the truncated first-message preview.
			return
		}
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleType(role),
		Content:       content,
		Timestamp:     ts,
		ContentLength: len(content),
		Model:         b.model,
	})
	b.ordinal++
}

func (b *codexSessionBuilder) handleAgentMessage(
	payload gjson.Result, ts time.Time,
) {
	msg := extractCodexInboundAgentMessage(payload, b.agentPath)
	if strings.TrimSpace(msg.content) == "" {
		return
	}
	first, replay := b.observeUserPrompt(msg.dedup)
	if first {
		b.firstMessage = truncate(
			strings.ReplaceAll(msg.content, "\n", " "), 300,
		)
		b.firstMessagePlaceholder = msg.placeholder
	}
	if replay {
		return
	}
	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleUser,
		Content:       msg.content,
		Timestamp:     ts,
		ContentLength: len(msg.content),
		Model:         b.model,
	})
	b.ordinal++
}

func (b *codexSessionBuilder) handleEventMsg(payload gjson.Result) {
	eventType := payload.Get("type").Str
	switch eventType {
	case "task_started", "task_complete", "turn_aborted":
		b.observeTaskEvent(eventType)
	case "token_count":
		b.handleTokenCountEvent(payload)
	case "collab_agent_spawn_end":
		b.handleCollabAgentSpawnEnd(payload)
	case "sub_agent_activity":
		b.handleSubagentActivity(payload)
	}
}

func (b *codexSessionBuilder) markFirstUserReplayPossible() {
	b.codexCursorState.markFirstUserReplayPossible()
}

func (b *codexSessionBuilder) handleTokenCountEvent(
	payload gjson.Result,
) {
	raw := payload.Get("info.last_token_usage").Raw
	if raw == "" || b.observeTokenUsage(raw) {
		return
	}

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
	b.unattachedTokenUsage = true
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

func (b *codexSessionBuilder) handleSubagentActivity(
	payload gjson.Result,
) {
	if payload.Get("kind").Str != "started" {
		return
	}
	callID := payload.Get("event_id").Str
	agentID := strings.TrimSpace(payload.Get("agent_thread_id").Str)
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
	// Redact before storing: InputJSON feeds the transcript UI, markdown
	// export, and resume rendering, which would otherwise re-expose
	// encrypted collab payloads that the formatted content filters out.
	// Scoped to collab tools so a legitimate Fernet token in another
	// tool's arguments is preserved.
	inputJSON := extractCodexInputJSON(payload)
	if IsCodexCollabTool(name) {
		inputJSON = RedactCodexEncryptedTokens(inputJSON)
	}
	skillName := inferCodexSkillNameWithBase(name, inputJSON, b.cwd)
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
		Model:         b.model,
		ToolCalls: []ParsedToolCall{{
			ToolUseID: callID,
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: inputJSON,
			SkillName: skillName,
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

	output, raw := parseCodexFunctionOutput(payload)
	if !output.Exists() {
		if strings.TrimSpace(raw) == "" {
			return
		}
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
	default:
		if text := strings.TrimSpace(raw); text != "" {
			source := "function_call_output"
			status := ""
			if payload.Get("type").Str == "custom_tool_call_output" {
				source = "custom_tool_call_output"
				status = payload.Get("status").Str
				if status == "" {
					status = "completed"
				}
			}
			b.appendCallResultEvent(callID, ParsedToolResultEvent{
				ToolUseID: callID,
				Source:    source,
				Status:    status,
				Content:   text,
				Timestamp: ts,
			})
		}
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
					Model:         b.model,
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

	argPreview := codexArgPreview
	if IsCodexCollabTool(name) {
		argPreview = codexCollabArgPreview
	}

	category := NormalizeToolCategory(name)
	if category == "Other" {
		header := formatToolHeader("Tool", name)
		if summary != "" {
			if IsCodexCollabTool(name) {
				summary = RedactCodexEncryptedTokens(summary)
			}
			return header + "\n" + summary
		}
		if preview := argPreview(args, rawArgs); preview != "" {
			return header + "\n" + preview
		}
		return header
	}

	detail := firstNonEmpty(summary,
		codexCategoryDetail(category, args))
	header := formatToolHeader(category, detail)
	if IsCodexCollabTool(name) {
		header = formatCodexCollabToolHeader(category, detail)
	}
	if preview := argPreview(args, rawArgs); preview != "" {
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
			codexArgValue(args, "task_name"),
			"spawn_agent",
		)
	}
	header := formatCodexCollabToolHeader("Task", summary)
	prompt := firstNonEmpty(
		codexArgValue(args, "description"),
		codexArgValue(args, "message"),
		codexArgValue(args, "prompt"),
	)
	if prompt != "" {
		if isCodexEncryptedToolContent(prompt) {
			return header + "\n[Encrypted message: content unavailable]"
		}
		prompt = RedactCodexEncryptedTokens(prompt)
		firstLine, _, _ := strings.Cut(prompt, "\n")
		return header + "\n" + truncate(firstLine, 220)
	}
	if preview := codexCollabArgPreview(args, rawArgs); preview != "" {
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

// codexCollabArgPreview is codexArgPreview for collab tools: encrypted
// payloads are redacted before the preview is truncated, since a
// clipped token would no longer validate and would leak a ciphertext
// prefix.
func codexCollabArgPreview(
	args gjson.Result, rawArgs string,
) string {
	if rawArgs == "" && args.Exists() {
		rawArgs = args.Raw
	}
	return codexArgPreview(
		gjson.Result{}, RedactCodexEncryptedTokens(rawArgs),
	)
}

// codexCollabTools is the canonical list of Codex multi-agent
// collaboration tools whose message arguments the Responses backend
// encrypts (openai/codex#26210). IsCodexCollabTool and every SQL IN-list
// that scopes redaction or repair derive from this slice
// (CodexCollabToolsSQL); do not restate the names elsewhere.
var codexCollabTools = []string{"spawn_agent", "send_message", "followup_task"}

// IsCodexCollabTool reports whether name is one of the Codex multi-agent
// collaboration tools whose message arguments the Responses backend
// encrypts (openai/codex#26210). Redaction is scoped to these tools so a
// legitimate Fernet token in any other tool's arguments is preserved.
// Exported for the resync copy path, which applies the same scope.
func IsCodexCollabTool(name string) bool {
	return slices.Contains(codexCollabTools, name)
}

// CodexCollabToolsSQL renders the collab tool list as a SQL IN-list, e.g.
// ('spawn_agent', 'send_message', 'followup_task'), so storage-side
// redaction scopes stay generated from the same source as
// IsCodexCollabTool. The names are compile-time literals with no quoting
// hazards.
func CodexCollabToolsSQL() string {
	quoted := make([]string, len(codexCollabTools))
	for i, name := range codexCollabTools {
		quoted[i] = "'" + name + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

// CodexCollabToolHeaderLikePatterns returns SQL LIKE patterns for formatted
// headers that any canonical collaboration tool can produce. Storage write
// guards use these patterns when legacy message metadata is incomplete. The
// returned slice is derived from codexCollabTools so newly categorized tools
// cannot silently fall outside the guard.
func CodexCollabToolHeaderLikePatterns() []string {
	seen := make(map[string]struct{})
	var patterns []string
	for _, name := range codexCollabTools {
		category := NormalizeToolCategory(name)
		candidates := []string{
			"[" + category + "]",
			"[" + category + ": %]",
		}
		if category == "Other" {
			candidates = []string{"[Tool: " + sanitizeToolLabel(name) + "]"}
		}
		for _, pattern := range candidates {
			if _, ok := seen[pattern]; ok {
				continue
			}
			seen[pattern] = struct{}{}
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// codexFernetTokenPattern over-matches candidates; each hit is verified
// with isCodexEncryptedToolContent before redaction so Fernet-looking
// plaintext survives.
var codexFernetTokenPattern = regexp.MustCompile(
	`gAAAAA[A-Za-z0-9_-]{60,}={0,2}`,
)

// codexClippedFernetTailPattern matches a Fernet-looking token cut off by
// the legacy preview truncation paths. Those paths appended an ellipsis, which
// is required here as evidence that the token-like plaintext was truncated.
// A clipped token no longer passes full Fernet validation, so migration code
// must recognize the parser artifact by its anchored tail shape instead.
var codexClippedFernetTailPattern = regexp.MustCompile(
	`gAAAAA[A-Za-z0-9_-]{40,}={0,2}\.\.\.$`,
)

// codexShortFernetTailPattern handles a token whose clip point fell within
// 40 characters of its start. The shape is too short to identify on its own,
// so callers apply it only at the exact legacy truncated-preview length.
var codexShortFernetTailPattern = regexp.MustCompile(
	`gAAAAA[A-Za-z0-9_-]*={0,2}\.\.\.$`,
)

const (
	// truncate(s, 300) retained 300 runes and appended "..." for session
	// first_message previews.
	codexStoredFirstMessageTruncatedRunes = 303
	// codexArgPreview retained 220 runes and appended "..." on the final
	// formatted tool-preview line.
	codexStoredToolPreviewTruncatedRunes = 223
)

// RedactCodexEncryptedTokens replaces server-side-encrypted multi-agent
// payloads (opaque Fernet tokens) embedded in a string, such as tool
// argument previews and stored input JSON. Redaction must run before any
// truncation: a truncated token no longer validates and would leak a
// ciphertext prefix. Exported for the resync copy path, which scrubs
// orphaned Codex rows preserved from archives written before ingest
// redacted these payloads.
func RedactCodexEncryptedTokens(s string) string {
	if !strings.Contains(s, "gAAAAA") {
		return s
	}
	return codexFernetTokenPattern.ReplaceAllStringFunc(
		s, func(token string) string {
			if !isCodexEncryptedToolContent(token) {
				return token
			}
			return "[encrypted]"
		},
	)
}

// RedactCodexStoredSubagentMessage scrubs an older parser's inbound encrypted
// agent turn only when the stored message consists entirely of validated
// Fernet tokens and whitespace. Current ingestion classifies encrypted_content
// blocks one at a time and preserves literal text, so a token quoted inside a
// sentence must not cause that sentence to be rewritten during migration.
func RedactCodexStoredSubagentMessage(content string) string {
	if !strings.Contains(content, "gAAAAA") {
		return content
	}
	found := false
	remainder := codexFernetTokenPattern.ReplaceAllStringFunc(
		content, func(token string) string {
			if !isCodexEncryptedToolContent(token) {
				return token
			}
			found = true
			return ""
		},
	)
	if !found || strings.TrimSpace(remainder) != "" {
		return content
	}
	return RedactCodexEncryptedTokens(content)
}

// RedactCodexStoredToolContent scrubs encrypted collab payloads from formatted
// Codex tool-call content written by older parsers. In addition to complete
// Fernet values it recognizes tokens clipped by the old 220-rune argument
// preview. Callers must scope this helper to formatted Codex collab tool rows;
// its clipped-tail fallback is intentionally not safe for arbitrary user text.
func RedactCodexStoredToolContent(content string) string {
	redacted := RedactCodexEncryptedTokens(content)
	redacted = codexClippedFernetTailPattern.ReplaceAllString(
		redacted, "[encrypted]",
	)
	lastLine := content[strings.LastIndexByte(content, '\n')+1:]
	if utf8.RuneCountInString(lastLine) == codexStoredToolPreviewTruncatedRunes {
		redacted = codexShortFernetTailPattern.ReplaceAllString(
			redacted, "[encrypted]",
		)
	}
	return redacted
}

// CodexStoredToolContentIsProvablyNonCollab reports whether every encrypted
// token in stored formatted tool content belongs to a tool block whose header
// cannot be produced by a Codex collaboration tool. Legacy certification uses
// this when tool-call rows may be incomplete: a surviving Bash row is not
// enough to vouch for a separate, lost [Task: ...] collaboration block.
func CodexStoredToolContentIsProvablyNonCollab(content string) bool {
	foundEncrypted := false
	insideNonCollabBlock := false
	for line := range strings.SplitSeq(content, "\n") {
		if label, detail, ok := codexStoredToolHeader(line); ok {
			insideNonCollabBlock = codexToolHeaderIsProvablyNonCollab(
				label, detail,
			)
		}
		if RedactCodexStoredToolContent(line) == line {
			continue
		}
		foundEncrypted = true
		if !insideNonCollabBlock {
			return false
		}
	}
	return foundEncrypted
}

// CodexStoredToolContentNeedsCollabRedaction reports whether formatted stored
// content contains an encrypted token in a block whose header could have been
// produced by a collaboration tool. Unlike
// CodexStoredToolContentIsProvablyNonCollab, a bare token with no formatted
// tool header is not enough: this predicate exists to recover collaboration
// scope when both has_tool_use and the tool_calls row are missing.
func CodexStoredToolContentNeedsCollabRedaction(content string) bool {
	insideFormattedBlock := false
	insideNonCollabBlock := false
	for line := range strings.SplitSeq(content, "\n") {
		if label, detail, ok := codexStoredToolHeader(line); ok {
			insideFormattedBlock = true
			insideNonCollabBlock = codexToolHeaderIsProvablyNonCollab(
				label, detail,
			)
		}
		if RedactCodexStoredToolContent(line) != line &&
			insideFormattedBlock && !insideNonCollabBlock {
			return true
		}
	}
	return false
}

func codexStoredToolHeader(line string) (label, detail string, ok bool) {
	if len(line) < 2 || line[0] != '[' || line[len(line)-1] != ']' {
		return "", "", false
	}
	inner := line[1 : len(line)-1]
	label, detail, hasDetail := strings.Cut(inner, ": ")
	if !hasDetail {
		detail = ""
	}
	if label == "" {
		return "", "", false
	}
	return label, detail, true
}

func codexToolHeaderIsProvablyNonCollab(label, detail string) bool {
	switch label {
	case "Read", "Edit", "Write", "Bash", "Grep", "Glob", "Task":
	case "Tool":
		if detail == "" {
			return false
		}
	default:
		return false
	}
	for _, name := range codexCollabTools {
		category := NormalizeToolCategory(name)
		if category == "Other" {
			if label == "Tool" && detail == sanitizeToolLabel(name) {
				return false
			}
			continue
		}
		// A categorized tool's detail is a summary rather than its name, so
		// matching the label alone is enough to make the block ambiguous.
		if label == category {
			return false
		}
	}
	return true
}

// RedactCodexStoredSubagentPreview scrubs encrypted task deliveries from a
// copied Codex subagent first_message. Callers must scope it to subagent
// sessions: a root preview is user-authored and may legitimately quote a
// Fernet token.
func RedactCodexStoredSubagentPreview(preview string) string {
	if !strings.Contains(preview, "gAAAAA") {
		return preview
	}
	candidate := RedactCodexEncryptedTokens(preview)
	if strings.HasSuffix(preview, "...") {
		candidate = codexClippedFernetTailPattern.ReplaceAllString(
			candidate, "[encrypted]",
		)
	}
	if utf8.RuneCountInString(preview) == codexStoredFirstMessageTruncatedRunes {
		candidate = codexShortFernetTailPattern.ReplaceAllString(
			candidate, "[encrypted]",
		)
	}
	withoutPlaceholders := strings.ReplaceAll(candidate, "[encrypted]", "")
	if candidate != preview && strings.TrimSpace(withoutPlaceholders) == "" {
		return candidate
	}

	// Preserve complete tokens embedded in literal preview text, while still
	// retaining the old clipped-tail repair for parser-truncated previews.
	redacted := preview
	if strings.HasSuffix(preview, "...") {
		redacted = codexClippedFernetTailPattern.ReplaceAllString(
			redacted, "[encrypted]",
		)
	}
	if redacted == preview &&
		utf8.RuneCountInString(preview) == codexStoredFirstMessageTruncatedRunes {
		redacted = codexShortFernetTailPattern.ReplaceAllString(
			redacted, "[encrypted]",
		)
	}
	return redacted
}

func formatToolHeader(
	label, detail string,
) string {
	return formatSanitizedToolHeader(
		sanitizeToolLabel(label), sanitizeToolLabel(detail),
	)
}

func formatCodexCollabToolHeader(
	label, detail string,
) string {
	return formatSanitizedToolHeader(
		sanitizeToolLabel(label),
		RedactCodexEncryptedTokens(sanitizeToolLabel(detail)),
	)
}

func formatSanitizedToolHeader(label, detail string) string {
	if label == "" {
		label = "Tool"
	}
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

func extractCodexTextBlocks(payload gjson.Result) []string {
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
	return texts
}

// extractCodexContent joins all text blocks from a Codex
// response item's content array.
func extractCodexContent(payload gjson.Result) string {
	return strings.Join(extractCodexTextBlocks(payload), "\n")
}

// codexInboundAgentMessage is an inter-agent message addressed to this
// session's agent. content is the transcript text (a placeholder when the
// payload is encrypted); dedup is the replay-digest input. It includes every
// plaintext and ciphertext delivery block so distinct tasks cannot shadow
// each other in post-abort replay detection, while a verbatim re-emission
// still matches. Ciphertext is never included in content.
type codexInboundAgentMessage struct {
	content     string
	dedup       string
	placeholder bool
}

func codexInboundAgentMessageDedup(texts, ciphertexts []string) string {
	var dedup strings.Builder
	writeValues := func(values []string) {
		dedup.WriteString(strconv.Itoa(len(values)))
		dedup.WriteByte(';')
		for _, value := range values {
			dedup.WriteString(strconv.Itoa(len(value)))
			dedup.WriteByte(':')
			dedup.WriteString(value)
		}
	}
	writeValues(texts)
	writeValues(ciphertexts)
	return dedup.String()
}

func extractCodexInboundAgentMessage(
	payload gjson.Result, agentPath string,
) codexInboundAgentMessage {
	if agentPath == "" ||
		strings.TrimSpace(payload.Get("recipient").Str) != agentPath {
		return codexInboundAgentMessage{}
	}
	var texts, ciphertexts []string
	payload.Get("content").ForEach(
		func(_, block gjson.Result) bool {
			if block.Get("type").Str == "encrypted_content" {
				text := block.Get("encrypted_content").Str
				if text == "" {
					return true
				}
				if isCodexEncryptedToolContent(text) {
					ciphertexts = append(ciphertexts, text)
					return true
				}
				texts = append(texts, text)
			}
			return true
		},
	)
	if len(texts) == 0 && len(ciphertexts) > 0 {
		// Multi-agent v2 task payloads are encrypted server-side; the
		// plaintext never exists on this machine, so the turn records
		// the delivery instead of vanishing from the transcript.
		content := "[Encrypted agent message: content unavailable]"
		if author := strings.TrimSpace(payload.Get("author").Str); author != "" {
			content = fmt.Sprintf(
				"[Encrypted agent message from %s: content unavailable]",
				author,
			)
		}
		return codexInboundAgentMessage{
			content:     content,
			dedup:       codexInboundAgentMessageDedup(texts, ciphertexts),
			placeholder: true,
		}
	}
	content := strings.Join(texts, "\n")
	return codexInboundAgentMessage{
		content: content,
		dedup:   codexInboundAgentMessageDedup(texts, ciphertexts),
	}
}

// isCodexEncryptedToolContent recognizes the URL-safe base64 envelope used
// by Fernet without attempting to decrypt it. Current Codex multi-agent tools
// can emit either plaintext or an opaque Fernet value in encrypted_content,
// so the content type alone is not enough to decide whether it is displayable.
func isCodexEncryptedToolContent(content string) bool {
	if !strings.HasPrefix(content, "gAAAAA") {
		return false
	}
	decoded, err := base64.URLEncoding.DecodeString(content)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(content)
	}
	if err != nil || len(decoded) < 73 || decoded[0] != 0x80 {
		return false
	}
	const fernetEnvelopeBytes = 1 + 8 + 16 + 32
	ciphertextBytes := len(decoded) - fernetEnvelopeBytes
	return ciphertextBytes >= 16 && ciphertextBytes%16 == 0
}

func codexAgentPathLeaf(agentPath string) string {
	trimmed := strings.Trim(agentPath, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// extractCodexInitialUserContent filters the synthetic blocks bundled with
// Codex's recommended-plugins injection while retaining user-authored blocks
// from the same response item.
func extractCodexInitialUserContent(payload gjson.Result) string {
	texts := extractCodexTextBlocks(payload)
	if len(texts) == 0 {
		return strings.Join(texts, "\n")
	}

	stripped := stripCodexRecommendedPlugins(texts[0])
	if stripped == texts[0] {
		return strings.Join(texts, "\n")
	}
	texts[0] = stripped
	kept := texts[:0]
	for _, text := range texts {
		text = stripCodexInitialSystemPrefix(text)
		if strings.TrimSpace(text) == "" || isCodexSystemMessage(text) {
			continue
		}
		kept = append(kept, text)
	}
	return strings.Join(kept, "\n")
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

// parseSession parses a Codex JSONL session file into a session and its
// messages. The includeExec parameter is retained for backward compatibility;
// exec-originated sessions are now always parsed and imported. This is the
// provider-owned parse entrypoint; the package-level free function was folded
// onto the provider.
func (p *codexProvider) parseSession(
	path, machine string, includeExec bool,
) (*ParsedSession, []ParsedMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	return p.parseSessionSnapshot(
		path, machine, includeExec, f, info,
	)
}

// parseSessionSnapshot parses exactly the raw-size snapshot captured from f.
// Limiting the reader prevents an append racing the scan from being folded into
// a cursor keyed by the earlier size.
func (p *codexProvider) parseSessionSnapshot(
	path, machine string,
	includeExec bool,
	f *os.File,
	info os.FileInfo,
) (*ParsedSession, []ParsedMessage, error) {
	lr := newLineReader(io.LimitReader(f, info.Size()), maxLineSize)
	defer releaseLineReader(lr)
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
	inode, device := sourceFileIdentity(info)
	if safe, safeErr := codexSafeResumeOffsetFile(f, info.Size()); safeErr == nil && safe {
		p.cursorCache.Put(
			path,
			info.Size(),
			inode,
			device,
			b.incrementalSeed(),
		)
	}

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

	mtime := info.ModTime().UnixNano()
	// Include session_index.jsonl mtime so renames trigger a re-parse.
	if idxPath := codexSessionIndexPath(path); idxPath != "" {
		if idxInfo, err := os.Stat(idxPath); err == nil {
			if idxMtime := idxInfo.ModTime().UnixNano(); idxMtime > mtime {
				mtime = idxMtime
			}
		}
	}

	sessionName := LookupCodexThreadName(path, b.sessionID)
	if sessionName == "" &&
		(b.firstMessage == "" || b.firstMessagePlaceholder) &&
		b.relationshipType == RelSubagent {
		sessionName = codexAgentPathLeaf(b.agentPath)
	}

	sess := &ParsedSession{
		ID:                sessionID,
		Project:           b.project,
		Machine:           machine,
		Agent:             AgentCodex,
		ParentSessionID:   b.parentSessionID,
		RelationshipType:  b.relationshipType,
		Cwd:               b.cwd,
		FirstMessage:      b.firstMessage,
		SessionName:       sessionName,
		StartedAt:         b.startedAt,
		EndedAt:           b.endedAt,
		MessageCount:      len(b.messages),
		UserMessageCount:  userCount,
		TerminationStatus: classifyCodexTermination(b.lastTaskEvent),
		File: FileInfo{
			Path:   path,
			Size:   info.Size(),
			Mtime:  mtime,
			Inode:  int64(inode),
			Device: int64(device),
		},
	}

	accumulateMessageTokenUsage(sess, b.messages)

	return sess, b.messages, nil
}

// CodexSessionIndexFilename is the name of the Codex index file that maps
// session UUIDs to their (renameable) thread titles. It sits next to the
// sessions/ and archived_sessions/ directories.
const CodexSessionIndexFilename = "session_index.jsonl"

// CodexSessionIndexTitles returns the session UUID to thread-title map from
// a Codex session_index.jsonl file, or nil when it cannot be read. The
// underlying read is cached by path, mtime, and size.
func CodexSessionIndexTitles(indexPath string) map[string]string {
	titles, err := loadCodexSessionIndex(indexPath)
	if err != nil {
		return nil
	}
	return titles
}

// EvictCodexSessionIndex removes one cached session_index.jsonl entry. Callers
// use it when an explicit change event makes the sidecar stat tuple insufficient
// and when transient hydrated indexes should not outlive their parse.
func EvictCodexSessionIndex(indexPath string) {
	codexSessionIndexCache.mu.Lock()
	delete(codexSessionIndexCache.entries, indexPath)
	codexSessionIndexCache.mu.Unlock()
}

// EvictAllCodexSessionIndexes removes every cached session_index.jsonl entry.
// Watcher overflow recovery uses this before a force-reverification pass,
// because the individual index paths that changed were deliberately coalesced.
func EvictAllCodexSessionIndexes() {
	codexSessionIndexCache.mu.Lock()
	clear(codexSessionIndexCache.entries)
	codexSessionIndexCache.mu.Unlock()
}

// EvictCodexSessionIndexForSession removes the cached sidecar associated with
// one Codex transcript. Explicit full-parse callers use this when an external
// event says the sidecar changed even if its stat tuple did not.
func EvictCodexSessionIndexForSession(sessionPath string) {
	if indexPath := codexSessionIndexPath(sessionPath); indexPath != "" {
		EvictCodexSessionIndex(indexPath)
	}
}

// LookupCodexThreadName returns the current Codex thread name for a session
// from the session_index.jsonl file next to the session root.
func LookupCodexThreadName(sessionPath, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	indexPath := codexSessionIndexPath(sessionPath)
	if indexPath == "" {
		return ""
	}
	titles, err := loadCodexSessionIndex(indexPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(titles[sessionID])
}

// CodexEffectiveMtime returns the effective mtime for a Codex session file,
// incorporating session_index.jsonl so renames invalidate the cache.
func CodexEffectiveMtime(sessionPath string, fileMtime int64) int64 {
	if idxPath := codexSessionIndexPath(sessionPath); idxPath != "" {
		if si, err := os.Stat(idxPath); err == nil {
			if idxMtime := si.ModTime().UnixNano(); idxMtime > fileMtime {
				return idxMtime
			}
		}
	}
	return fileMtime
}

// CodexSessionIndexPath returns the local session_index.jsonl path associated
// with a Codex transcript, or an empty string when the transcript is outside a
// recognized Codex session directory.
func CodexSessionIndexPath(sessionPath string) string {
	return codexSessionIndexPath(sessionPath)
}

func codexSessionIndexPath(sessionPath string) string {
	dir := filepath.Dir(sessionPath)
	for dir != "" {
		base := filepath.Base(dir)
		if base == "sessions" || base == "archived_sessions" {
			return filepath.Join(
				filepath.Dir(dir), CodexSessionIndexFilename,
			)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func loadCodexSessionIndex(indexPath string) (map[string]string, error) {
	info, err := os.Stat(indexPath)
	if err != nil {
		return nil, err
	}

	mtime := info.ModTime().UnixNano()
	size := info.Size()
	changeTime, changeTimeOkay := codexIndexChangeTime(indexPath, info)

	codexSessionIndexCache.mu.Lock()
	if entry, ok := codexSessionIndexCache.entries[indexPath]; ok &&
		entry.mtime == mtime && entry.size == size &&
		(!changeTimeOkay ||
			(entry.changeTimeOkay && entry.changeTime == changeTime)) {
		codexSessionIndexCache.mu.Unlock()
		return entry.titles, nil
	}
	codexSessionIndexCache.mu.Unlock()

	f, err := os.Open(indexPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	titles, err := ParseCodexSessionIndexTitles(f)
	if err != nil {
		return nil, err
	}

	codexSessionIndexCache.mu.Lock()
	codexSessionIndexCache.entries[indexPath] = codexSessionIndexEntry{
		mtime:          mtime,
		size:           size,
		changeTime:     changeTime,
		changeTimeOkay: changeTimeOkay,
		titles:         titles,
	}
	codexSessionIndexCache.mu.Unlock()

	return titles, nil
}

// ParseCodexSessionIndexTitles reads a Codex session_index.jsonl stream and
// returns session UUIDs mapped to non-empty thread titles.
func ParseCodexSessionIndexTitles(r io.Reader) (map[string]string, error) {
	titles := make(map[string]string)
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for s.Scan() {
		line := s.Text()
		if !gjson.Valid(line) {
			continue
		}
		id := gjson.Get(line, "id").Str
		title := strings.TrimSpace(gjson.Get(line, "thread_name").Str)
		if id == "" || title == "" {
			continue
		}
		titles[id] = title
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return titles, nil
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
// dedup state, task lifecycle marker, and the fork replay gate (#643).
type codexIncrementalSeed = codexCursorState

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
) (codexIncrementalSeed, error) {
	f, err := os.Open(path)
	if err != nil {
		return codexIncrementalSeed{}, fmt.Errorf(
			"open codex prefix %s: %w", path, err,
		)
	}
	defer f.Close()
	seed, err := seedCodexIncrementalStateFromReader(
		io.LimitReader(f, offset),
	)
	if err != nil {
		return codexIncrementalSeed{}, fmt.Errorf(
			"read codex prefix %s: %w", path, err,
		)
	}
	return seed, nil
}

func seedCodexIncrementalStateFromReader(
	r io.Reader,
) (codexIncrementalSeed, error) {
	var seed codexIncrementalSeed
	lr := newLineReader(r, maxLineSize)
	defer releaseLineReader(lr)
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
			// gate and supplies cwd, and replayed parent metas are
			// dropped while it is active.
			if !seed.forkGate.active {
				if cwd := payload.Get("cwd").Str; cwd != "" {
					seed.cwd = cwd
				}
				seed.agentPath = strings.TrimSpace(
					payload.Get("agent_path").Str,
				)
				if seed.agentPath == "" {
					seed.agentPath = strings.TrimSpace(payload.Get(
						"source.subagent.thread_spawn.agent_path",
					).Str)
				}
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
			eventType := payload.Get("type").Str
			seed.observeTaskEvent(eventType)
			if eventType == "token_count" {
				raw := payload.Get("info.last_token_usage").Raw
				if raw != "" {
					seed.observeTokenUsage(raw)
				}
			}
		case codexTypeResponseItem:
			observeCodexIncrementalUserMessage(&seed, payload)
		}
	}
	if err := lr.Err(); err != nil {
		return codexIncrementalSeed{}, err
	}
	return seed, nil
}

// observeUserMessage feeds one response_item into the
// re-emitted-prompt dedup state, mirroring handleResponseItem's
// user-message filtering and full-content matching.
func observeCodexIncrementalUserMessage(
	s *codexIncrementalSeed,
	payload gjson.Result,
) {
	if payload.Get("type").Str == "agent_message" {
		msg := extractCodexInboundAgentMessage(payload, s.agentPath)
		if strings.TrimSpace(msg.content) != "" {
			s.observeUserPrompt(msg.dedup)
		}
		return
	}
	if payload.Get("role").Str != "user" {
		return
	}
	content := extractCodexContent(payload)
	if !s.firstUserSeen {
		content = extractCodexInitialUserContent(payload)
	}
	if strings.TrimSpace(content) == "" {
		return
	}
	if isCodexTurnAbortedMessage(content) {
		s.markFirstUserReplayPossible()
	}
	if isCodexSystemMessage(content) {
		return
	}
	s.observeUserPrompt(content)
}

// CodexTranscriptConsumedSize returns the byte offset after the last complete,
// valid JSON line in a Codex transcript. Bytes after this offset are ignored by
// the Codex JSONL parser, so partial trailing writes are not part of the parsed
// source snapshot.
func CodexTranscriptConsumedSize(path string) (int64, error) {
	consumed, err := readCodexJSONLFrom(path, 0, func(line string) {})
	if errors.Is(err, errCodexIncrementalNeedsFullParse) {
		return consumed, nil
	}
	return consumed, err
}

// readCodexJSONLFrom is the Codex-specific conservative tail reader. Codex may
// expose syntactically valid JSON before the writer appends its record newline;
// such an EOF record requires a full-parse fallback and is never staged as a
// safe continuation cursor.
func readCodexJSONLFrom(
	path string,
	offset int64,
	fn func(line string),
) (consumed int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return readCodexJSONLSection(f, offset, info.Size(), fn)
}

// readCodexJSONLSection applies the conservative Codex JSONL rules to the
// exact byte range [offset, limit). A SectionReader makes the captured limit a
// real EOF even if the underlying descriptor has since grown.
func readCodexJSONLSection(
	f *os.File,
	offset int64,
	limit int64,
	fn func(line string),
) (consumed int64, err error) {
	if offset < 0 || limit < offset {
		return 0, fmt.Errorf(
			"invalid codex JSONL section [%d,%d)", offset, limit,
		)
	}
	section := io.NewSectionReader(f, offset, limit-offset)
	return readCodexJSONLReader(section, section, fn)
}

func readCodexJSONLReader(
	r io.Reader,
	at io.ReaderAt,
	fn func(line string),
) (consumed int64, err error) {
	lr := newLineReader(r, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		var terminator [1]byte
		if _, err := at.ReadAt(terminator[:], lr.bytesRead-1); err != nil {
			return consumed, err
		}
		if terminator[0] != '\n' {
			if gjson.Valid(line) {
				return consumed, errCodexIncrementalNeedsFullParse
			}
			break
		}
		if gjson.Valid(line) {
			fn(line)
			consumed = lr.bytesRead
		}
	}
	return consumed, lr.Err()
}

// codexSafeResumeOffset checks in O(1) that offset starts at a physical line
// boundary. Offset zero is always safe; every nonzero offset must immediately
// follow a newline byte.
func codexSafeResumeOffset(path string, offset int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return codexSafeResumeOffsetFile(f, offset)
}

func codexSafeResumeOffsetFile(f *os.File, offset int64) (bool, error) {
	if offset == 0 {
		return true, nil
	}
	if offset < 0 {
		return false, nil
	}
	var previous [1]byte
	if _, err := f.ReadAt(previous[:], offset-1); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return previous[0] == '\n', nil
}

type codexIncrementalParseResult struct {
	messages      []ParsedMessage
	endedAt       time.Time
	consumedBytes int64
	initialCursor codexCursorState
	cursor        codexCursorState
	inode         uint64
	device        uint64
}

// parseSessionFromDetailed parses only new lines from a Codex JSONL file. It
// resumes from an exact cached cursor when one exists and otherwise reconstructs
// the same state by scanning the prefix. A successful result carries the exact
// cursor the provider may stage at offset+consumed; the prior offset remains
// eligible until the caller persists the new offset.
func (p *codexProvider) parseSessionFromDetailed(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
) (codexIncrementalParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return codexIncrementalParseResult{}, fmt.Errorf(
			"open codex %s: %w", path, err,
		)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return codexIncrementalParseResult{}, fmt.Errorf(
			"stat codex %s: %w", path, err,
		)
	}
	return p.parseSessionFromSnapshot(
		path, offset, startOrdinal, includeExec, f, info, info.Size(),
	)
}

func (p *codexProvider) parseSessionFromSnapshot(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
	f *os.File,
	info os.FileInfo,
	limit int64,
) (codexIncrementalParseResult, error) {
	return p.parseSessionFromWithSources(
		path,
		offset,
		startOrdinal,
		includeExec,
		info,
		func(fn func(string)) (int64, error) {
			return readCodexJSONLSection(f, offset, limit, fn)
		},
		func() (codexIncrementalSeed, error) {
			return seedCodexIncrementalStateFromReader(
				io.NewSectionReader(f, 0, offset),
			)
		},
	)
}

func (p *codexProvider) parseSessionFromWithReader(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
	readLines func(string, int64, func(string)) (int64, error),
) (codexIncrementalParseResult, error) {
	return p.parseSessionFromWithReaders(
		path,
		offset,
		startOrdinal,
		includeExec,
		readLines,
		seedCodexIncrementalState,
	)
}

func (p *codexProvider) parseSessionFromWithReaders(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
	readLines func(string, int64, func(string)) (int64, error),
	readSeed func(string, int64) (codexIncrementalSeed, error),
) (codexIncrementalParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return codexIncrementalParseResult{}, fmt.Errorf(
			"stat codex %s: %w", path, err,
		)
	}
	return p.parseSessionFromWithSources(
		path,
		offset,
		startOrdinal,
		includeExec,
		info,
		func(fn func(string)) (int64, error) {
			return readLines(path, offset, fn)
		},
		func() (codexIncrementalSeed, error) {
			return readSeed(path, offset)
		},
	)
}

func (p *codexProvider) parseSessionFromWithSources(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
	info os.FileInfo,
	readLines func(func(string)) (int64, error),
	readSeed func() (codexIncrementalSeed, error),
) (codexIncrementalParseResult, error) {
	inode, device := sourceFileIdentity(info)
	seed, cacheHit := p.cursorCache.Get(path, offset, inode, device)
	if !cacheHit {
		var err error
		seed, err = readSeed()
		if err != nil {
			return codexIncrementalParseResult{}, fmt.Errorf(
				"seed codex %s at offset %d: %w",
				path, offset, err,
			)
		}
	}

	b := newCodexSessionBuilder(includeExec)
	b.ordinal = startOrdinal
	b.codexCursorState = seed
	var fallbackErr error

	consumed, err := readLines(
		func(line string) {
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
			if b.unattachedTokenUsage {
				fallbackErr = errCodexIncrementalNeedsFullParse
				return
			}
		},
	)
	if err != nil {
		return codexIncrementalParseResult{}, fmt.Errorf(
			"reading codex %s from offset %d: %w",
			path, offset, err,
		)
	}
	if fallbackErr != nil {
		return codexIncrementalParseResult{}, fallbackErr
	}

	b.flushPendingAgentResults()
	result := codexIncrementalParseResult{
		messages:      b.messages,
		endedAt:       b.endedAt,
		consumedBytes: consumed,
		initialCursor: seed,
		cursor:        b.incrementalSeed(),
		inode:         inode,
		device:        device,
	}
	return result, nil
}

// parseSessionFrom preserves the legacy test-helper and parser signature while
// the provider facade consumes the detailed cursor result internally.
func (p *codexProvider) parseSessionFrom(
	path string,
	offset int64,
	startOrdinal int,
	includeExec bool,
) ([]ParsedMessage, time.Time, int64, error) {
	result, err := p.parseSessionFromWithReader(
		path,
		offset,
		startOrdinal,
		includeExec,
		readJSONLFrom,
	)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	return result.messages, result.endedAt, result.consumedBytes, nil
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
		isCodexSubagentNotification(content) ||
		isCodexGoalContext(content)
}

// stripCodexRecommendedPlugins removes the plugin-discovery envelope
// that recent Codex versions prepend to the synthetic context item at the
// start of a session. It is called only while looking for the first genuine
// user turn, so a later user message that quotes the envelope is preserved.
func stripCodexRecommendedPlugins(content string) string {
	const (
		openTag  = "<recommended_plugins>"
		closeTag = "</recommended_plugins>"
	)
	start := strings.Index(content, openTag)
	if start < 0 {
		return content
	}
	relativeEnd := strings.Index(content[start+len(openTag):], closeTag)
	if relativeEnd < 0 {
		return content
	}
	end := start + len(openTag) + relativeEnd + len(closeTag)
	prefix := content[:start]
	suffix := strings.TrimLeft(content[end:], "\r\n")
	if strings.TrimSpace(prefix) == "" {
		return suffix
	}
	return prefix + suffix
}

// stripCodexInitialSystemPrefix removes complete synthetic envelopes from the
// start of an initial text block. A genuine prompt may follow an injected
// envelope in the same block, so only text through the first matching close
// tag is removed.
func stripCodexInitialSystemPrefix(content string) string {
	for {
		trimmed := strings.TrimLeft(content, "\r\n")
		var closeTag string
		switch {
		case strings.HasPrefix(trimmed, "# AGENTS.md"):
			closeTag = "</INSTRUCTIONS>"
		case strings.HasPrefix(trimmed, "<environment_context>"):
			closeTag = "</environment_context>"
		case strings.HasPrefix(trimmed, "<INSTRUCTIONS>"):
			closeTag = "</INSTRUCTIONS>"
		default:
			return content
		}

		_, after, ok := strings.Cut(trimmed, closeTag)
		if !ok {
			return content
		}
		content = strings.TrimLeft(after, "\r\n")
	}
}

// isCodexGoalContext reports whether content is a Codex /goal
// continuation envelope. These are harness-injected as role=user
// records to keep the model working toward an active thread goal, but
// they are not user-authored turns and should be treated as system
// content. Current sessions wrap the body in
// <codex_internal_context source="goal">; older sessions used
// <goal_context>. Detection is scoped to the structured wrapper (and,
// for the modern form, the goal source specifically) so that other
// internal-context envelopes and real user messages quoting the goal
// text are left untouched.
func isCodexGoalContext(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "<goal_context>") {
		return true
	}
	if strings.HasPrefix(trimmed, "<codex_internal_context") {
		openTag, _, ok := strings.Cut(trimmed, ">")
		return ok && codexGoalContextSourceAttrRe.MatchString(openTag)
	}
	return false
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
		payload := gjson.Get(line, "payload")
		switch payload.Get("type").Str {
		case "collab_agent_spawn_end":
			return true
		case "sub_agent_activity":
			return payload.Get("kind").Str == "started"
		default:
			return false
		}
	case codexTypeResponseItem:
	default:
		return false
	}

	payload := gjson.Get(line, "payload")
	switch payload.Get("type").Str {
	case "function_call", "custom_tool_call":
		return isCodexWaitAgentCall(payload.Get("name").Str)
	case "function_call_output", "custom_tool_call_output":
		output, raw := parseCodexFunctionOutput(payload)
		return isCodexSubagentFunctionOutput(output) ||
			strings.TrimSpace(raw) != ""
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
