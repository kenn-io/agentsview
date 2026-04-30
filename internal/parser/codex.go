package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	messages             []ParsedMessage
	firstMessage         string
	startedAt            time.Time
	endedAt              time.Time
	sessionID            string
	project              string
	ordinal              int
	currentModel         string
	callNames            map[string]string
	callRefs             map[string]codexToolCallRef
	agentSpawnCalls      map[string]string
	agentWaitCalls       map[string]string
	pendingAgentEvents   map[string][]codexPendingEvent
	orphanNotificationIx map[string]int
	lastTokenUsageRaw    string // dedup streaming duplicates

	// Most recent task lifecycle event seen on the file. Used to
	// classify termination_status — task_complete maps to
	// "awaiting user input" while task_started in flight (no
	// matching task_complete after) means the agent was working
	// when the file was last written.
	lastTaskEvent string
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
		return b.handleSessionMeta(payload)
	case codexTypeTurnContext:
		b.currentModel = payload.Get("model").Str
	case codexTypeResponseItem:
		b.handleResponseItem(payload, ts)
	case codexTypeEventMsg:
		b.handleEventMsg(payload)
	}
	return false
}

func (b *codexSessionBuilder) handleSessionMeta(
	payload gjson.Result,
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

	if role == "user" && isCodexSystemMessage(content) {
		return
	}

	if role == "user" && b.firstMessage == "" {
		b.firstMessage = truncate(
			strings.ReplaceAll(content, "\n", " "), 300,
		)
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

func (b *codexSessionBuilder) handleEventMsg(
	payload gjson.Result,
) {
	switch payload.Get("type").Str {
	case "task_started", "task_complete", "turn_aborted":
		b.lastTaskEvent = payload.Get("type").Str
	}
	if payload.Get("type").Str != "token_count" {
		return
	}
	raw := payload.Get("info.last_token_usage").Raw
	if raw == "" || raw == b.lastTokenUsageRaw {
		return
	}
	b.lastTokenUsageRaw = raw

	// Find last assistant message without usage in the current
	// turn. Stop at user message boundary so we don't cross
	// turns.
	for i := len(b.messages) - 1; i >= 0; i-- {
		if b.messages[i].Role == RoleUser {
			break
		}
		if b.messages[i].Role == RoleAssistant &&
			b.messages[i].TokenUsage == nil {
			b.applyCodexTokenUsage(&b.messages[i], raw)
			return
		}
	}
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
	if name == "wait" && callID != "" {
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

	if name == "wait" && callID != "" {
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
	case "wait":
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
	agentID = strings.TrimSpace(parsed.Get("agent_id").Str)
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

// readCodexModelAtOffset scans a Codex JSONL file from the
// start up to the given byte offset and returns the model
// from the most recent turn_context entry. Returns "" when
// no turn_context is found before the offset. Used to seed
// currentModel for incremental parses that resume past turn
// boundaries.
// readCodexModelAtOffset scans a Codex JSONL file from the
// start up to the given byte offset and returns the model
// from the most recent turn_context entry. Mirrors the full
// parser: every turn_context unconditionally overwrites the
// model, including empty strings. Returns "" when no
// turn_context is found before the offset.
func readCodexModelAtOffset(
	path string, offset int64,
) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	lr := newLineReader(
		io.LimitReader(f, offset), maxLineSize,
	)
	var model string
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		if gjson.Get(line, "type").Str != codexTypeTurnContext {
			continue
		}
		model = gjson.Get(line, "payload.model").Str
	}
	return model
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
	b.currentModel = readCodexModelAtOffset(path, offset)
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
// Codex parse error requires the caller to fall back to a full parse.
func IsIncrementalFullParseFallback(err error) bool {
	return errors.Is(err, errCodexIncrementalNeedsFullParse)
}

func isCodexSystemMessage(content string) bool {
	trimmed := strings.TrimSpace(content)
	return strings.HasPrefix(content, "# AGENTS.md") ||
		strings.HasPrefix(content, "<environment_context>") ||
		strings.HasPrefix(content, "<INSTRUCTIONS>") ||
		strings.HasPrefix(trimmed, "<turn_aborted>") ||
		strings.HasPrefix(trimmed, "<skill>") ||
		isCodexSubagentNotification(content)
}

func isCodexSubagentNotification(content string) bool {
	return strings.HasPrefix(
		strings.TrimSpace(content),
		"<subagent_notification>",
	)
}

func codexIncrementalNeedsFullParse(line string) bool {
	if gjson.Get(line, "type").Str != codexTypeResponseItem {
		return false
	}

	payload := gjson.Get(line, "payload")
	switch payload.Get("type").Str {
	case "function_call":
		return payload.Get("name").Str == "wait"
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
