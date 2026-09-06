package parser

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
)

// Copilot JSONL event types.
const (
	copilotEventSessionStart    = "session.start"
	copilotEventUserMessage     = "user.message"
	copilotEventAssistantMsg    = "assistant.message"
	copilotEventToolStart       = "tool.execution_start"
	copilotEventToolComplete    = "tool.execution_complete"
	copilotEventAssistantReason = "assistant.reasoning"
	copilotEventModelChange     = "session.model_change"
	copilotEventSessionShutdown = "session.shutdown"
	copilotReportedCostSource   = "copilot-reported"
)

var copilotUsageBasedPricingStartedAt = time.Date(
	2026, time.June, 1, 0, 0, 0, 0, time.UTC,
)

// copilotSessionBuilder accumulates state while scanning a
// Copilot JSONL session file line by line.
type copilotSessionBuilder struct {
	messages                []ParsedMessage
	usageEvents             []ParsedUsageEvent
	firstMessage            string
	startedAt               time.Time
	endedAt                 time.Time
	sessionID               string
	project                 string
	ordinal                 int
	currentModel            string
	shutdownCoveredMessages int
	usageCoveredAt          time.Time
	fallbackOutput          int
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
	case copilotEventToolStart:
		b.handleToolStart(data, ts)
	case copilotEventToolComplete:
		b.handleToolComplete(data, ts)
	case copilotEventAssistantReason:
		b.handleAssistantReasoning()
	case copilotEventModelChange:
		if v := data.Get("newModel"); v.Exists() {
			b.currentModel = normalizeCopilotModel(v.Str)
		}
	case copilotEventSessionShutdown:
		b.handleShutdown(data, ts)
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

	outputTokens := int(data.Get("outputTokens").Int())
	hasOutputTokens := data.Get("outputTokens").Exists()
	model := normalizeCopilotModel(data.Get("model").Str)
	if model == "" {
		model = b.currentModel
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:         b.ordinal,
		Role:            RoleAssistant,
		Content:         displayContent,
		Timestamp:       ts,
		HasThinking:     hasThinking,
		HasToolUse:      hasToolUse,
		ContentLength:   len(displayContent),
		ToolCalls:       toolCalls,
		Model:           model,
		OutputTokens:    outputTokens,
		HasOutputTokens: hasOutputTokens,
	})
	b.ordinal++
}

func (b *copilotSessionBuilder) handleToolStart(
	data gjson.Result, ts time.Time,
) {
	b.appendToolExecutionEvent(
		data.Get("toolCallId").Str, "started", "", ts,
	)
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
	status := "completed"
	if success := data.Get("success"); success.Exists() && !success.Bool() {
		status = "errored"
	}
	b.appendToolExecutionEvent(toolCallID, status, content, ts)
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

func (b *copilotSessionBuilder) appendToolExecutionEvent(
	toolCallID, status, content string, ts time.Time,
) {
	if toolCallID == "" {
		return
	}
	for _, v := range slices.Backward(b.messages) {
		for j := range v.ToolCalls {
			call := &v.ToolCalls[j]
			if call.ToolUseID != toolCallID {
				continue
			}
			call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{
				ToolUseID: toolCallID,
				Source:    "tool_execution",
				Status:    status,
				Content:   content,
				Timestamp: ts,
			})
			return
		}
	}
}

func (b *copilotSessionBuilder) handleAssistantReasoning() {
	// Mark the most recent assistant message as having
	// thinking, if one exists.
	for i, v := range slices.Backward(b.messages) {
		if v.Role == RoleAssistant {
			b.messages[i].HasThinking = true
			return
		}
	}
}

// handleShutdown extracts per-model token usage from the
// session.shutdown event's modelMetrics field.
func (b *copilotSessionBuilder) handleShutdown(
	data gjson.Result, ts time.Time,
) {
	useReportedCost := !b.startedAt.IsZero() &&
		!b.startedAt.Before(copilotUsageBasedPricingStartedAt)
	totalNanoAiu := data.Get("totalNanoAiu")
	hasReportedCost := useReportedCost && totalNanoAiu.Type == gjson.Number &&
		totalNanoAiu.Num >= 0

	// totalNanoAiu is cumulative. Keep its authoritative cost on only the
	// latest shutdown, including when that final value is zero.
	if hasReportedCost {
		for i := range b.usageEvents {
			if b.usageEvents[i].CostSource == copilotReportedCostSource {
				b.usageEvents[i].Cost = nil
				b.usageEvents[i].CostStatus = ""
				b.usageEvents[i].CostSource = ""
			}
		}
	}

	occurredAt := timeString(ts, b.startedAt)
	var events []ParsedUsageEvent
	data.Get("modelMetrics").ForEach(
		func(modelKey, metrics gjson.Result) bool {
			usage := metrics.Get("usage")
			totalInput := int(usage.Get("inputTokens").Int())
			cacheRead := int(usage.Get("cacheReadTokens").Int())
			cacheWrite := int(usage.Get("cacheWriteTokens").Int())
			output := int(usage.Get("outputTokens").Int())
			reasoning := int(usage.Get("reasoningTokens").Int())

			// Fresh input = total - cache_read - cache_write.
			freshInput := max(totalInput-cacheRead-cacheWrite, 0)

			if freshInput == 0 && output == 0 &&
				cacheRead == 0 && cacheWrite == 0 &&
				reasoning == 0 {
				return true
			}

			events = append(events, ParsedUsageEvent{
				Source:                   "shutdown",
				Model:                    normalizeCopilotModel(modelKey.Str),
				InputTokens:              freshInput,
				OutputTokens:             output,
				CacheCreationInputTokens: cacheWrite,
				CacheReadInputTokens:     cacheRead,
				ReasoningTokens:          reasoning,
				OccurredAt:               occurredAt,
			})
			return true
		},
	)
	if len(events) > 0 {
		// Transcript order identifies covered messages even without timestamps.
		b.shutdownCoveredMessages = len(b.messages)
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].Model < events[j].Model
	})

	if hasReportedCost {
		if len(events) == 0 {
			events = append(events, ParsedUsageEvent{
				Source:     "shutdown",
				Model:      "copilot",
				OccurredAt: occurredAt,
			})
		}
		total := totalNanoAiu.Int()
		microdollars := total / 100_000
		if total%100_000 >= 50_000 {
			microdollars++
		}
		cost := money.Money{Microdollars: microdollars}
		// Carry the session-wide total on exactly one stable row so storage
		// and sync remain row-oriented without multiplying it by model count.
		events[0].Cost = &cost
		events[0].CostStatus = "exact"
		events[0].CostSource = copilotReportedCostSource
	}
	b.usageEvents = append(b.usageEvents, events...)
}

func (b *copilotSessionBuilder) applyMessageUsageFallback() {
	for i := range b.messages {
		message := &b.messages[i]
		if i < b.shutdownCoveredMessages || message.Role != RoleAssistant || message.Model == "" ||
			!message.HasOutputTokens ||
			(!message.Timestamp.IsZero() &&
				!message.Timestamp.After(b.usageCoveredAt)) {
			continue
		}
		message.TokenUsage = jsontext.Value(
			fmt.Sprintf(`{"output_tokens":%d}`, message.OutputTokens),
		)
		b.fallbackOutput += message.OutputTokens
	}
}

func (b *copilotSessionBuilder) markUsageCoveredAt(occurredAt time.Time) {
	if occurredAt.After(b.usageCoveredAt) {
		b.usageCoveredAt = occurredAt
	}
}

// loadCopilotStoreUsage reads the CLI's observed per-request token data when
// available. Billing semantics for this undocumented store are not assumed.
func loadCopilotStoreUsage(
	storePath, rawSessionID string,
) ([]ParsedUsageEvent, error) {
	if storePath == "" || rawSessionID == "" {
		return nil, nil
	}
	if _, err := os.Stat(storePath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat copilot session store %s: %w", storePath, err)
	}
	store, err := sql.Open(
		"sqlite3",
		"file:"+sqliteURIPath(storePath)+"?mode=ro&_busy_timeout=3000",
	)
	if err != nil {
		return nil, fmt.Errorf("opening copilot session store %s: %w", storePath, err)
	}
	defer store.Close()

	rows, err := store.Query(`
		SELECT id, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_write_tokens, reasoning_tokens, created_at
		FROM assistant_usage_events
		WHERE session_id = ?
		ORDER BY id
	`, rawSessionID)
	if err != nil {
		return nil, fmt.Errorf("reading copilot session store %s: %w", storePath, err)
	}
	defer rows.Close()

	var events []ParsedUsageEvent
	for rows.Next() {
		var id int64
		var model, createdAt string
		var input, output, cacheRead, cacheWrite, reasoning sql.NullInt64
		if err := rows.Scan(
			&id, &model, &input, &output, &cacheRead, &cacheWrite, &reasoning,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scanning copilot session-store usage: %w", err)
		}
		events = append(events, ParsedUsageEvent{
			Source:                   "session-store",
			Model:                    normalizeCopilotModel(model),
			InputTokens:              max(int(input.Int64-cacheRead.Int64-cacheWrite.Int64), 0),
			OutputTokens:             int(output.Int64),
			CacheCreationInputTokens: int(cacheWrite.Int64),
			CacheReadInputTokens:     int(cacheRead.Int64),
			ReasoningTokens:          int(reasoning.Int64),
			OccurredAt:               createdAt,
			DedupKey:                 fmt.Sprintf("session-store:%d", id),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating copilot session-store usage: %w", err)
	}
	return events, nil
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

// normalizeCopilotModel converts the model identifier used in
// Copilot session events to the form used in the pricing catalog.
// Claude model IDs use dots in version numbers in Copilot events
// (e.g. "claude-sonnet-4.6") but hyphens in the pricing catalog
// (e.g. "claude-sonnet-4-6"). Other model families such as GPT
// already use dots in the catalog (e.g. "gpt-5.4"), so only
// claude-prefixed names are normalized.
func normalizeCopilotModel(model string) string {
	if strings.HasPrefix(model, "claude-") {
		return strings.ReplaceAll(model, ".", "-")
	}
	return model
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

// parseSession parses a Copilot JSONL session file into the session, messages,
// and usage events the provider consumes. Returns (nil, nil, nil, nil) if the
// file doesn't exist or contains no user/assistant messages. This is the
// provider-owned parse entrypoint; the package-level free function was folded
// onto the provider.
func (p *copilotProvider) parseSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	return p.parseSessionWithStore(path, machine, "")
}

func (p *copilotProvider) parseSessionWithStore(
	path, machine, storePath string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
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
		return nil, nil, nil,
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
		return nil, nil, nil, nil
	}
	rawSessionID := b.sessionID
	if rawSessionID == "" {
		rawSessionID = sessionIDFromPath(path)
	}
	usesStoreUsage := false
	if !b.startedAt.Before(copilotUsageBasedPricingStartedAt) {
		storeUsage, err := loadCopilotStoreUsage(storePath, rawSessionID)
		if err != nil {
			log.Printf(
				"copilot session store unavailable for %s: %v; retaining transcript usage",
				storePath, err,
			)
		}
		if len(storeUsage) > 0 {
			for _, event := range storeUsage {
				b.markUsageCoveredAt(parseTimestamp(event.OccurredAt))
			}
			for _, event := range b.usageEvents {
				if event.Cost == nil ||
					event.CostSource != copilotReportedCostSource {
					continue
				}
				// Store usage supplies richer observed tokens, but transcript
				// shutdown remains the authoritative reported-cost source.
				settlement := event
				settlement.InputTokens = 0
				settlement.OutputTokens = 0
				settlement.CacheCreationInputTokens = 0
				settlement.CacheReadInputTokens = 0
				settlement.ReasoningTokens = 0
				storeUsage = append(storeUsage, settlement)
				break
			}
			b.usageEvents = storeUsage
			// Only the selected token source determines message coverage.
			b.shutdownCoveredMessages = 0
			usesStoreUsage = true
		}
	}
	b.applyMessageUsageFallback()

	sessionID := "copilot:" + rawSessionID

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

	accumulateMessageTokenUsage(sess, b.messages)
	if usesStoreUsage {
		applyUsageEventTokenTotals(sess, b.usageEvents)
		if b.fallbackOutput > 0 {
			sess.HasTotalOutputTokens = true
			sess.TotalOutputTokens += b.fallbackOutput
		}
	}

	// Stamp the session ID on usage events (not known until here).
	// DedupKey encodes the event's position in the slice so that
	// multi-segment sessions (where the same model appears in
	// several shutdown events) each get a distinct key.
	for i := range b.usageEvents {
		b.usageEvents[i].SessionID = sessionID
		if b.usageEvents[i].DedupKey == "" {
			b.usageEvents[i].DedupKey = fmt.Sprintf(
				"shutdown:%s:%s:%d",
				sessionID,
				b.usageEvents[i].Model,
				i,
			)
		}
	}

	return sess, b.messages, b.usageEvents, nil
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
