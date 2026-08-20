package parser

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
)

type grokSummaryFields struct {
	Summary            string
	FirstPrompt        string
	ModelID            string
	CreatedAt          string
	UpdatedAt          string
	LastActiveAt       string
	Hostname           string
	NumMessages        int
	WorktreeLabel      string
	GitRootDir         string
	Cwd                string
	HeadBranch         string
	ParentSessionID    string
	SourceWorkspaceDir string
}

type grokSignalMetrics struct {
	TotalOutputTokens int
	PeakContextTokens int
	UserMessageCount  int
	HasUserMessages   bool
}

type grokPromptContext struct {
	IsNonInteractive bool `json:"is_non_interactive"`
}

func ParseGrokSummary(
	path, projectHint, machine string,
) (ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !gjson.ValidBytes(data) {
		return ParseResult{}, fmt.Errorf("decode %s: invalid json", path)
	}

	summary := decodeGrokSummary(data)
	rawID := filepath.Base(filepath.Dir(path))
	if !IsValidSessionID(rawID) {
		return ParseResult{}, fmt.Errorf("invalid grok session id for %s", path)
	}
	sessionDir := filepath.Dir(path)
	signals, err := parseGrokSignals(filepath.Join(sessionDir, "signals.json"))
	if err != nil {
		return ParseResult{}, err
	}
	promptContext, err := parseGrokPromptContext(
		filepath.Join(sessionDir, "prompt_context.json"),
	)
	if err != nil {
		return ParseResult{}, err
	}
	sessionKind := ""
	if promptContext.IsNonInteractive {
		sessionKind = SessionKindNonInteractive
	}

	project, cwd := grokProjectAndCwd(summary, projectHint)
	startedAt := grokParseTime(summary.CreatedAt)
	endedAt := grokEndedAt(summary)
	parentSessionID := strings.TrimSpace(summary.ParentSessionID)
	relationshipType := RelNone
	if parentSessionID != "" {
		parentSessionID = "grok:" + parentSessionID
		relationshipType = RelFork
	}

	messages, malformed, transcriptErr := parseGrokChatHistory(
		filepath.Join(sessionDir, "chat_history.jsonl"),
	)
	if transcriptErr != nil && !os.IsNotExist(transcriptErr) {
		return ParseResult{}, transcriptErr
	}
	timestampAnchors, timestampErr := parseGrokUpdateTimestampAnchors(
		filepath.Join(sessionDir, "updates.jsonl"),
	)
	if timestampErr != nil && !errors.Is(timestampErr, os.ErrNotExist) {
		return ParseResult{}, timestampErr
	}
	enrichGrokMessageTimestamps(messages, timestampAnchors)
	enrichGrokToolResultEvents(messages, timestampAnchors)

	firstPrompt := ""
	for _, msg := range messages {
		if msg.Role == RoleUser && strings.TrimSpace(msg.Content) != "" {
			firstPrompt = strings.TrimSpace(msg.Content)
			break
		}
	}
	if firstPrompt == "" {
		firstPrompt = strings.TrimSpace(summary.FirstPrompt)
	}
	// Current Grok Build stores the searchable prompt text in
	// session_summary / generated_title rather than firstPrompt.
	if firstPrompt == "" {
		firstPrompt = strings.TrimSpace(summary.Summary)
	}

	userMessageCount := 0
	messageCount := 0
	countsAuthoritative := false
	transcriptFidelity := TranscriptFidelityFull
	sourceVersion := "grok-chat-v1"

	if len(messages) > 0 {
		for _, msg := range messages {
			messageCount++
			// Tool-result carrier rows are RoleUser with empty content so the
			// sync engine can pair them onto tool calls and then drop them.
			if msg.Role == RoleUser && !msg.IsSystem &&
				strings.TrimSpace(msg.Content) != "" {
				userMessageCount++
			}
		}
	} else {
		// Fall back to summary-only when chat_history is missing/empty.
		transcriptFidelity = TranscriptFidelitySummary
		sourceVersion = "grok-summary-v1"
		countsAuthoritative = true
		switch {
		case signals.HasUserMessages:
			userMessageCount = signals.UserMessageCount
		case firstPrompt != "":
			userMessageCount = 1
		}
		messageCount = max(summary.NumMessages, userMessageCount)
		if firstPrompt != "" {
			messages = []ParsedMessage{{
				Role:      RoleUser,
				Content:   firstPrompt,
				Timestamp: startedAt,
			}}
		}
	}

	result := ParseResult{
		Session: ParsedSession{
			ID:                 "grok:" + rawID,
			Project:            project,
			Machine:            machine,
			Agent:              AgentGrok,
			SessionKind:        sessionKind,
			ParentSessionID:    parentSessionID,
			RelationshipType:   relationshipType,
			Cwd:                cwd,
			GitBranch:          summary.HeadBranch,
			SourceSessionID:    rawID,
			SourceVersion:      sourceVersion,
			TranscriptFidelity: transcriptFidelity,
			MalformedLines:     malformed,
			FirstMessage: truncate(
				strings.ReplaceAll(firstPrompt, "\n", " "),
				300,
			),
			SessionName:         strings.TrimSpace(summary.Summary),
			StartedAt:           startedAt,
			EndedAt:             endedAt,
			MessageCount:        messageCount,
			UserMessageCount:    userMessageCount,
			CountsAuthoritative: countsAuthoritative,
			File: FileInfo{
				Path:  path,
				Size:  info.Size(),
				Mtime: info.ModTime().UnixNano(),
			},
		},
		Messages: messages,
	}
	if signals.TotalOutputTokens > 0 {
		result.Session.TotalOutputTokens = signals.TotalOutputTokens
		result.Session.HasTotalOutputTokens = true
	}
	if signals.PeakContextTokens > 0 {
		result.Session.PeakContextTokens = signals.PeakContextTokens
		result.Session.HasPeakContextTokens = true
	}
	result.UsageEvents, err = parseGrokUsageEvents(
		filepath.Join(sessionDir, "updates.jsonl"),
		result.Session.ID, summary.ModelID, startedAt, endedAt,
	)
	if err != nil {
		return ParseResult{}, err
	}
	totalOutput, hasOutput, _, _ := UsageEventTokenAggregate(result.UsageEvents)
	if hasOutput {
		result.Session.TotalOutputTokens = totalOutput
		result.Session.HasTotalOutputTokens = true
	}
	result.Session.aggregateTokenPresenceKnown =
		result.Session.HasTotalOutputTokens ||
			result.Session.HasPeakContextTokens
	return result, nil
}

func parseGrokPromptContext(path string) (grokPromptContext, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return grokPromptContext{}, nil
	}
	if err != nil {
		return grokPromptContext{}, fmt.Errorf("read %s: %w", path, err)
	}
	var context grokPromptContext
	if err := json.Unmarshal(data, &context); err != nil {
		return grokPromptContext{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return context, nil
}

// Grok Build writes one usage payload per completed turn, and those
// payloads are per-turn measurements — not cumulative session snapshots
// (cachedReadTokens is non-monotonic across a session's turns, and
// modelCalls resets each turn). Emit one event per turn and model so
// session totals and daily bucketing sum correctly; keeping only the
// final payload undercounts multi-turn sessions by orders of magnitude
// and attributes everything to the session's end date (#1227).
func parseGrokUsageEvents(
	path, sessionID, summaryModel string,
	startedAt, endedAt time.Time,
) ([]ParsedUsageEvent, error) {
	sessionFallback := timeString(endedAt, startedAt)
	var events []ParsedUsageEvent
	// Last-wins in-memory dedupe: a re-emitted payload for the same turn
	// and model (retry/replay lines share a prompt_id) is a rewrite of
	// that turn, and duplicate DedupKeys would violate the DB's unique
	// (session_id, source, dedup_key) index and roll back the replace.
	eventIndex := map[string]int{}
	emit := func(ev ParsedUsageEvent) {
		if i, ok := eventIndex[ev.DedupKey]; ok {
			events[i] = ev
			return
		}
		eventIndex[ev.DedupKey] = len(events)
		events = append(events, ev)
	}
	turn := 0
	var costErr error
	_, err := readJSONLFrom(path, 0, func(line string) {
		if costErr != nil {
			return
		}
		usage := gjson.Get(line, "params.update.usage")
		if !usage.Exists() || !usage.IsObject() {
			return
		}
		turn++
		// prompt_id is unique per turn and stable across re-parses; the
		// ordinal fallback covers payloads that predate prompt_id.
		turnKey := gjson.Get(line, "params.update.prompt_id").String()
		if turnKey == "" {
			turnKey = fmt.Sprintf("turn-%d", turn)
		}
		occurredAt := timeString(
			hermesUnixTime(gjson.Get(line, "timestamp").Float()), time.Time{},
		)
		if occurredAt == "" {
			occurredAt = sessionFallback
		}
		modelUsage := usage.Get("modelUsage")
		emitted := false
		if modelUsage.IsObject() {
			modelUsage.ForEach(func(model, modelData gjson.Result) bool {
				event, eventErr := grokUsageEvent(
					sessionID, model.Str, turnKey, modelData, occurredAt,
				)
				if eventErr != nil {
					costErr = eventErr
					return false
				}
				emit(event)
				emitted = true
				return true
			})
		}
		if costErr != nil {
			return
		}
		if !emitted {
			model := summaryModel
			if model == "" {
				model = "grok-summary"
			}
			event, eventErr := grokUsageEvent(
				sessionID, model, turnKey, usage, occurredAt,
			)
			if eventErr != nil {
				costErr = eventErr
				return
			}
			emit(event)
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if costErr != nil {
		return nil, costErr
	}
	return events, nil
}

func grokUsageEvent(
	sessionID, model, turnKey string, usage gjson.Result, occurredAt string,
) (ParsedUsageEvent, error) {
	input := int(usage.Get("inputTokens").Int())
	cachedRead := int(usage.Get("cachedReadTokens").Int())
	cost, err := grokUsageCost(usage)
	if err != nil {
		return ParsedUsageEvent{}, err
	}
	return ParsedUsageEvent{
		SessionID:            sessionID,
		Source:               "session",
		Model:                model,
		InputTokens:          max(input-cachedRead, 0),
		OutputTokens:         int(usage.Get("outputTokens").Int()),
		CacheReadInputTokens: cachedRead,
		ReasoningTokens:      int(usage.Get("reasoningTokens").Int()),
		Cost:                 cost,
		OccurredAt:           occurredAt,
		DedupKey:             "session:" + sessionID + ":" + turnKey + ":" + model,
	}, nil
}

func grokUsageCost(usage gjson.Result) (*money.Money, error) {
	ticks := usage.Get("costUsdTicks")
	if !ticks.Exists() {
		return nil, nil
	}
	if strings.HasPrefix(ticks.Raw, "-") {
		return nil, fmt.Errorf("parsing Grok cost ticks: %w", money.ErrNegative)
	}
	microdollars, err := money.ParseScaledDecimal(ticks.Raw+"e-10", 6)
	if err != nil {
		return nil, fmt.Errorf("parsing Grok cost ticks: %w", err)
	}
	cost := money.Money{Microdollars: microdollars}
	return &cost, nil
}

func parseGrokChatHistory(path string) ([]ParsedMessage, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)

	var (
		messages         []ParsedMessage
		malformed        int
		pendingThink     string
		hasPending       bool
		ordinal          int
		seenBackendTools = make(map[string]struct{})
	)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !gjson.Valid(line) {
			malformed++
			continue
		}
		root := gjson.Parse(line)
		switch grokChatRowKind(root) {
		case "system":
			// System prompts are vendor boilerplate; skip them.
			continue

		case "user":
			pendingThink = ""
			hasPending = false
			reason := strings.TrimSpace(root.Get("synthetic_reason").Str)
			if reason != "" && reason != "interjection" {
				continue
			}
			content := grokUserContent(root.Get("content"))
			if content == "" {
				// Meta-only injections (user_info / system-reminder /
				// skills catalog) are not real user turns.
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				ContentLength: len(content),
			})
			ordinal++

		case "reasoning":
			text := grokReasoningText(root)
			if text == "" {
				continue
			}
			if hasPending {
				pendingThink += "\n\n" + text
			} else {
				pendingThink = text
				hasPending = true
			}

		case "backend_tool_call":
			msg, ok := grokBackendToolMessage(root, ordinal)
			if !ok {
				continue
			}
			messages = append(messages, msg)
			ordinal++
			if len(msg.ToolCalls) > 0 {
				id := strings.TrimSpace(msg.ToolCalls[0].ToolUseID)
				if id != "" {
					seenBackendTools[id] = struct{}{}
				}
			}

		case "assistant":
			for _, backendMsg := range grokRawOutputBackendTools(
				root, seenBackendTools,
			) {
				backendMsg.Ordinal = ordinal
				messages = append(messages, backendMsg)
				ordinal++
			}
			content := strings.TrimSpace(root.Get("content").Str)
			toolCalls := grokToolCalls(root.Get("tool_calls"))
			thinking := pendingThink
			if inline := grokAssistantReasoning(root); inline != "" {
				thinking = inline
			}
			pendingThink = ""
			hasPending = false
			if content == "" && len(toolCalls) == 0 && thinking == "" {
				continue
			}
			msg := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				ContentLength: len(content),
				Model:         strings.TrimSpace(root.Get("model_id").Str),
				ToolCalls:     toolCalls,
				HasToolUse:    len(toolCalls) > 0,
			}
			if thinking != "" {
				msg.HasThinking = true
				msg.ThinkingText = thinking
				msg.Content = "[Thinking]\n" + thinking + "\n[/Thinking]\n" + content
				msg.ContentLength = len(thinking) + len(content)
			}
			messages = append(messages, msg)
			ordinal++

		case "tool_result":
			pendingThink = ""
			hasPending = false
			toolCallID := strings.TrimSpace(root.Get("tool_call_id").Str)
			if toolCallID == "" {
				continue
			}
			content := root.Get("content")
			contentRaw := content.Raw
			contentLen := toolResultContentLength(content)
			if content.Type == gjson.String {
				// Preserve tool output as JSON-quoted string so
				// pairToolResults / DecodeContent can surface it.
				quoted, _ := json.Marshal(content.Str)
				contentRaw = string(quoted)
				contentLen = len(content.Str)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				ContentLength: contentLen,
				ToolResults: []ParsedToolResult{{
					ToolUseID:     toolCallID,
					ContentRaw:    contentRaw,
					ContentLength: contentLen,
				}},
			})
			ordinal++

		default:
			// Unknown entry types are ignored but not treated as malformed.
			continue
		}
	}
	if err := lr.Err(); err != nil {
		return nil, malformed, fmt.Errorf("reading %s: %w", path, err)
	}
	return messages, malformed, nil
}

type grokTimestampAnchor struct {
	role         RoleType
	content      string
	timestamp    time.Time
	toolCallIDs  []string
	toolResultID string
	toolStatus   string
}

type grokTimestampAnchorBuilder struct {
	anchors     []grokTimestampAnchor
	role        RoleType
	content     strings.Builder
	timestamp   time.Time
	toolCallIDs []string
}

func (b *grokTimestampAnchorBuilder) flush() {
	if b.role == "" {
		return
	}
	b.anchors = append(b.anchors, grokTimestampAnchor{
		role:        b.role,
		content:     b.content.String(),
		timestamp:   b.timestamp,
		toolCallIDs: append([]string(nil), b.toolCallIDs...),
	})
	b.role = ""
	b.content.Reset()
	b.timestamp = time.Time{}
	b.toolCallIDs = b.toolCallIDs[:0]
}

func (b *grokTimestampAnchorBuilder) start(role RoleType, timestamp time.Time) {
	if b.role != role {
		b.flush()
		b.role = role
	}
	if b.timestamp.IsZero() && !timestamp.IsZero() {
		b.timestamp = timestamp
	}
}

func (b *grokTimestampAnchorBuilder) addUserText(text string) {
	if text == "" {
		return
	}
	if b.content.Len() > 0 {
		b.content.WriteByte('\n')
	}
	b.content.WriteString(text)
}

func parseGrokUpdateTimestampAnchors(
	path string,
) ([]grokTimestampAnchor, error) {
	var builder grokTimestampAnchorBuilder
	_, err := readJSONLFrom(path, 0, func(line string) {
		if !gjson.Valid(line) {
			return
		}
		root := gjson.Parse(line)
		update := root.Get("params.update")
		if !update.Exists() {
			update = root.Get("update")
		}
		if !update.Exists() {
			return
		}
		timestamp := hermesUnixTime(root.Get("timestamp").Float())
		switch update.Get("sessionUpdate").Str {
		case "user_message_chunk":
			if update.Get("_meta.hostTurn").Bool() {
				builder.flush()
				return
			}
			builder.start(RoleUser, timestamp)
			if update.Get("content.type").Str == "text" {
				builder.addUserText(update.Get("content.text").Str)
			}

		case "agent_message_chunk":
			if update.Get("_meta.hostTurn").Bool() {
				builder.flush()
				return
			}
			builder.start(RoleAssistant, timestamp)
			if update.Get("content.type").Str == "text" {
				builder.content.WriteString(update.Get("content.text").Str)
			}

		case "tool_call":
			builder.flush()
			anchor := grokTimestampAnchor{
				role:      RoleAssistant,
				timestamp: timestamp,
			}
			if id := strings.TrimSpace(update.Get("toolCallId").Str); id != "" {
				anchor.toolCallIDs = []string{id}
			}
			builder.anchors = append(builder.anchors, anchor)

		case "tool_call_update":
			status := update.Get("status").Str
			if status != "completed" && status != "failed" {
				return
			}
			builder.flush()
			if id := strings.TrimSpace(update.Get("toolCallId").Str); id != "" {
				if status == "failed" {
					status = "errored"
				}
				builder.anchors = append(builder.anchors, grokTimestampAnchor{
					timestamp:    timestamp,
					toolResultID: id,
					toolStatus:   status,
				})
			}

		case "compaction_checkpoint":
			builder = grokTimestampAnchorBuilder{}
		}
	})
	builder.flush()
	return builder.anchors, err
}

func enrichGrokMessageTimestamps(
	messages []ParsedMessage, anchors []grokTimestampAnchor,
) {
	anchorIndex := 0
	for i := range messages {
		for j := anchorIndex; j < len(anchors); j++ {
			if !grokTimestampAnchorMatches(messages[i], anchors[j]) {
				continue
			}
			messages[i].Timestamp = anchors[j].timestamp
			anchorIndex = j + 1
			break
		}
	}
}

func enrichGrokToolResultEvents(
	messages []ParsedMessage, anchors []grokTimestampAnchor,
) {
	calls := make(map[string]*ParsedToolCall)
	results := make(map[string]string)
	for i := range messages {
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			if id := strings.TrimSpace(call.ToolUseID); id != "" {
				calls[id] = call
			}
		}
		for _, result := range messages[i].ToolResults {
			if id := strings.TrimSpace(result.ToolUseID); id != "" {
				results[id] = DecodeContent(result.ContentRaw)
			}
		}
	}
	for _, anchor := range anchors {
		for _, id := range anchor.toolCallIDs {
			call := calls[id]
			if call == nil {
				continue
			}
			call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{
				ToolUseID: id,
				Source:    "tool_execution",
				Status:    "started",
				Timestamp: anchor.timestamp,
			})
		}
		if anchor.toolResultID == "" {
			continue
		}
		call := calls[anchor.toolResultID]
		if call == nil {
			continue
		}
		call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{
			ToolUseID: anchor.toolResultID,
			Source:    "tool_execution",
			Status:    anchor.toolStatus,
			Content:   results[anchor.toolResultID],
			Timestamp: anchor.timestamp,
		})
	}
}

func grokTimestampAnchorMatches(
	message ParsedMessage, anchor grokTimestampAnchor,
) bool {
	if message.Role != anchor.role {
		return false
	}
	if len(message.ToolResults) > 0 {
		for _, result := range message.ToolResults {
			if result.ToolUseID != "" && result.ToolUseID == anchor.toolResultID {
				return true
			}
		}
		return false
	}
	if len(message.ToolCalls) > 0 && len(anchor.toolCallIDs) > 0 {
		hasComparableIDs := false
		for _, call := range message.ToolCalls {
			for _, id := range anchor.toolCallIDs {
				if call.ToolUseID == "" || id == "" {
					continue
				}
				hasComparableIDs = true
				if call.ToolUseID == id {
					return true
				}
			}
		}
		if hasComparableIDs {
			return false
		}
	}
	switch message.Role {
	case RoleUser:
		return message.Content == grokNormalizeUserText(anchor.content)
	case RoleAssistant:
		content := message.Content
		if message.HasThinking {
			content = strings.TrimPrefix(
				content,
				"[Thinking]\n"+message.ThinkingText+"\n[/Thinking]\n",
			)
		}
		return content == strings.TrimSpace(anchor.content)
	default:
		return false
	}
}

func grokChatRowKind(root gjson.Result) string {
	switch kind := strings.TrimSpace(root.Get("type").Str); kind {
	case "system", "user", "reasoning", "backend_tool_call", "assistant", "tool_result":
		return kind
	}
	switch strings.TrimSpace(root.Get("role").Str) {
	case "system":
		return "system"
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool_result"
	default:
		return ""
	}
}

func grokBackendToolMessage(
	root gjson.Result, ordinal int,
) (ParsedMessage, bool) {
	payload := root.Get("kind")
	rowType := strings.TrimSpace(root.Get("type").Str)
	if !payload.Exists() {
		payload = root
	}
	toolName := strings.TrimSpace(payload.Get("tool_type").Str)
	if toolName == "" {
		switch rowType {
		case "web_search_call":
			toolName = "web_search"
		case "custom_tool_call":
			toolName = "x_search"
		case "code_interpreter_call":
			toolName = "code_interpreter"
		}
	}
	if toolName == "" {
		return ParsedMessage{}, false
	}
	id := strings.TrimSpace(payload.Get("id").Str)
	action := payload.Get("action")
	inputJSON := action.Raw
	if inputJSON == "" {
		if input := payload.Get("input"); input.Type == gjson.String {
			inputJSON = input.Str
		} else {
			inputJSON = input.Raw
		}
	}
	if inputJSON == "" {
		inputJSON = payload.Raw
	}
	content := grokBackendToolSummary(toolName, payload)
	call := ParsedToolCall{
		ToolUseID: id,
		ToolName:  toolName,
		Category:  NormalizeToolCategory(toolName),
		InputJSON: inputJSON,
	}
	return ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleAssistant,
		Content:       content,
		ContentLength: len(content),
		HasToolUse:    true,
		ToolCalls:     []ParsedToolCall{call},
	}, true
}

func grokRawOutputBackendTools(
	root gjson.Result, seen map[string]struct{},
) []ParsedMessage {
	var messages []ParsedMessage
	rawOutput := root.Get("raw_output")
	if !rawOutput.IsArray() {
		return nil
	}
	rawOutput.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").Str {
		case "web_search_call", "custom_tool_call", "code_interpreter_call":
		default:
			return true
		}
		id := strings.TrimSpace(item.Get("id").Str)
		if _, exists := seen[id]; id != "" && exists {
			return true
		}
		message, ok := grokBackendToolMessage(item, 0)
		if !ok {
			return true
		}
		messages = append(messages, message)
		if id != "" {
			seen[id] = struct{}{}
		}
		return true
	})
	return messages
}

func grokBackendToolSummary(toolName string, payload gjson.Result) string {
	action := payload.Get("action")
	switch toolName {
	case "web_search":
		switch action.Get("type").Str {
		case "search":
			return "[backend web_search] search: " +
				truncate(strings.TrimSpace(action.Get("query").Str), 300)
		case "open", "open_page":
			return "[backend web_search] open: " +
				truncate(strings.TrimSpace(action.Get("url").Str), 300)
		case "find", "find_in_page":
			return "[backend web_search] find: " +
				truncate(strings.TrimSpace(action.Get("pattern").Str), 300)
		default:
			return "[backend web_search]"
		}
	case "x_search":
		return "[backend x_search] " +
			truncate(strings.TrimSpace(payload.Get("input").Str), 300)
	case "code_interpreter":
		return "[backend code_interpreter] " +
			truncate(strings.TrimSpace(payload.Get("code").Str), 300)
	default:
		return "[backend " + toolName + "]"
	}
}

func grokUserContent(content gjson.Result) string {
	var text string
	switch {
	case content.Type == gjson.String:
		text = content.Str
	case content.IsArray():
		var parts []string
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").Str == "text" {
				if t := part.Get("text").Str; t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		text = strings.Join(parts, "\n")
	default:
		return ""
	}
	return grokNormalizeUserText(text)
}

func grokNormalizeUserText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Prefer the explicit user query when Grok wraps prompts.
	if extracted := extractUserQuery(strings.Split(text, "\n")); extracted != text {
		return extracted
	}
	if strings.Contains(text, "<user_query>") {
		return extractUserQuery(strings.Split(text, "\n"))
	}
	// Strip injected context blocks; keep any remaining real prompt text.
	// Meta-only payloads collapse to empty and are skipped by the caller.
	return grokStripMetaUserBlocks(text)
}

// grokStripMetaUserBlocks removes recognized Grok context-injection blocks
// (user_info, git_status, system-reminder, agent_skills, mcp_servers) while
// preserving any surrounding user text. Mixed payloads therefore keep the
// real prompt instead of being discarded wholesale.
func grokStripMetaUserBlocks(text string) string {
	for _, tag := range []string{
		"user_info",
		"git_status",
		"system-reminder",
		"agent_skills",
		"mcp_servers",
	} {
		text = grokStripXMLTagBlock(text, tag)
	}
	return strings.TrimSpace(text)
}

// grokStripXMLTagBlock removes every <tag>...</tag> span from text. An
// unclosed opening tag drops the remainder of the string from that point.
func grokStripXMLTagBlock(text, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return text
		}
		rest := text[start+len(open):]
		endRel := strings.Index(rest, close)
		if endRel < 0 {
			return strings.TrimSpace(text[:start])
		}
		end := start + len(open) + endRel + len(close)
		text = text[:start] + text[end:]
	}
}

func grokReasoningText(root gjson.Result) string {
	var parts []string
	for _, path := range []string{"summary", "content"} {
		content := root.Get(path)
		if !content.IsArray() {
			continue
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if text := strings.TrimSpace(part.Get("text").Str); text != "" {
				parts = append(parts, text)
			}
			return true
		})
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(root.Get("content").Str)
}

func grokAssistantReasoning(root gjson.Result) string {
	if text := strings.TrimSpace(root.Get("reasoning.text").Str); text != "" {
		return text
	}
	var parts []string
	rawOutput := root.Get("raw_output")
	if rawOutput.IsArray() {
		rawOutput.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").Str != "reasoning" {
				return true
			}
			if text := grokReasoningText(item); text != "" {
				parts = append(parts, text)
			}
			return true
		})
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(root.Get("reasoning_content").Str)
}

func grokToolCalls(arr gjson.Result) []ParsedToolCall {
	if !arr.IsArray() {
		return nil
	}
	var out []ParsedToolCall
	arr.ForEach(func(_, tc gjson.Result) bool {
		name := firstNonEmptyJSONLString(
			tc.Get("name").Str,
			tc.Get("function.name").Str,
		)
		if name == "" {
			return true
		}
		inputJSON := grokToolCallInputJSON(tc)
		out = append(out, ParsedToolCall{
			ToolUseID: firstNonEmptyJSONLString(tc.Get("id").Str, tc.Get("tool_call_id").Str),
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: inputJSON,
			SkillName: inferToolSkillName(name, inputJSON),
		})
		return true
	})
	return out
}

// grokToolCallInputJSON picks the first present arguments field and normalizes
// JSON-encoded string values (OpenAI-style function.arguments) to the raw
// object JSON so path extraction and skill inference can read them.
func grokToolCallInputJSON(tc gjson.Result) string {
	for _, path := range []string{"arguments", "function.arguments", "input"} {
		args := tc.Get(path)
		if !args.Exists() {
			continue
		}
		if args.Type == gjson.String {
			// Unwrap JSON-encoded strings (OpenAI-style); plain text stays as-is.
			return args.Str
		}
		if raw := args.Raw; raw != "" && raw != "null" {
			return raw
		}
	}
	return ""
}

// grokSummaryMessageCount prefers the chat-transcript count over the broader
// event counter. Current Grok Build stores both: num_chat_messages is the
// transcript-shaped total AgentsView should surface, while num_messages also
// includes non-chat events and would inflate summary-only sessions.
func grokSummaryMessageCount(root gjson.Result) int {
	for _, path := range []string{
		"num_chat_messages",
		"num_messages",
		"numMessages",
	} {
		if v := root.Get(path); v.Exists() {
			return int(v.Int())
		}
	}
	return 0
}

func decodeGrokSummary(data []byte) grokSummaryFields {
	root := gjson.ParseBytes(data)
	return grokSummaryFields{
		Summary: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("generated_title").String()),
			strings.TrimSpace(root.Get("session_summary").String()),
			strings.TrimSpace(root.Get("summary").String()),
		),
		FirstPrompt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("firstPrompt").String()),
			strings.TrimSpace(root.Get("first_prompt").String()),
		),
		ModelID: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("current_model_id").String()),
			strings.TrimSpace(root.Get("modelId").String()),
			strings.TrimSpace(root.Get("model_id").String()),
		),
		CreatedAt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("created_at").String()),
			strings.TrimSpace(root.Get("createdAt").String()),
		),
		UpdatedAt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("updated_at").String()),
			strings.TrimSpace(root.Get("updatedAt").String()),
		),
		LastActiveAt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("last_active_at").String()),
			strings.TrimSpace(root.Get("lastActiveAt").String()),
		),
		Hostname:    strings.TrimSpace(root.Get("hostname").String()),
		NumMessages: grokSummaryMessageCount(root),
		WorktreeLabel: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("worktreeLabel").String()),
			strings.TrimSpace(root.Get("worktree_label").String()),
		),
		GitRootDir: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("git_root_dir").String()),
			strings.TrimSpace(root.Get("gitRootDir").String()),
		),
		Cwd: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("info.cwd").String()),
			strings.TrimSpace(root.Get("cwd").String()),
		),
		HeadBranch: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("head_branch").String()),
			strings.TrimSpace(root.Get("headBranch").String()),
			strings.TrimSpace(root.Get("git.branch").String()),
		),
		ParentSessionID: strings.TrimSpace(
			root.Get("parent_session_id").String(),
		),
		SourceWorkspaceDir: strings.TrimSpace(
			root.Get("source_workspace_dir").String(),
		),
	}
}

func grokProjectAndCwd(
	summary grokSummaryFields, projectHint string,
) (project, cwd string) {
	cwd = firstNonEmptyJSONLString(
		strings.TrimSpace(summary.Cwd),
		strings.TrimSpace(summary.GitRootDir),
	)
	projectCwd := firstNonEmptyJSONLString(
		strings.TrimSpace(summary.SourceWorkspaceDir),
		cwd,
	)
	if projectCwd != "" {
		if p := ExtractProjectFromCwdWithBranch(projectCwd, summary.HeadBranch); p != "" {
			return p, cwd
		}
	}

	// Prefer the vendor-provided worktree label when present (legacy
	// camelCase summary schema). Fall back to the path-derived hint.
	if label := strings.TrimSpace(summary.WorktreeLabel); label != "" {
		return label, cwd
	}

	hint := strings.TrimSpace(projectHint)
	if decoded, err := url.PathUnescape(hint); err == nil && decoded != "" {
		hint = decoded
	}
	if hint != "" {
		if p := ExtractProjectFromCwdWithBranch(hint, summary.HeadBranch); p != "" {
			return p, cwd
		}
		if p := GetProjectName(hint); p != "" {
			return p, cwd
		}
	}
	return "", cwd
}

func parseGrokSignals(path string) (grokSignalMetrics, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return grokSignalMetrics{}, nil
	}
	if err != nil {
		return grokSignalMetrics{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !gjson.ValidBytes(data) {
		return grokSignalMetrics{}, fmt.Errorf("decode %s: invalid json", path)
	}
	root := gjson.ParseBytes(data)
	metrics := grokSignalMetrics{
		TotalOutputTokens: grokFirstPositiveInt(
			data,
			"tokenUsage.totalOutputTokens",
			"usage.totalOutputTokens",
			"outputTokens",
			"totalOutputTokens",
		),
		PeakContextTokens: grokFirstPositiveInt(
			data,
			"tokenUsage.peakContextTokens",
			"usage.peakContextTokens",
			"peakContextTokens",
		),
	}
	if userCount := root.Get("userMessageCount"); userCount.Exists() {
		metrics.HasUserMessages = true
		if n := int(userCount.Int()); n > 0 {
			metrics.UserMessageCount = n
		}
	}
	return metrics, nil
}

func grokFirstPositiveInt(data []byte, paths ...string) int {
	for _, path := range paths {
		value := gjson.GetBytes(data, path)
		if !value.Exists() {
			continue
		}
		if n := int(value.Int()); n > 0 {
			return n
		}
	}
	return 0
}

func grokParseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func grokEndedAt(summary grokSummaryFields) time.Time {
	for _, value := range []string{
		summary.LastActiveAt,
		summary.UpdatedAt,
		summary.CreatedAt,
	} {
		if ts := grokParseTime(value); !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}
