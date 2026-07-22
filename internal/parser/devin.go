package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
)

const devinDBFilename = "sessions.db"

// DevinSessionMeta is lightweight metadata for a Devin session row.
type DevinSessionMeta struct {
	RawSessionID string
	VirtualPath  string
	Title        string
	CWD          string
	Model        string
	CreatedAt    time.Time
	LastActivity time.Time
	UpdatedAt    time.Time
	FileMtime    int64
}

// devinDBPath returns <root>/cli/sessions.db when present.
func devinDBPath(root string) string {
	if root == "" {
		return ""
	}
	path := filepath.Join(root, "cli", devinDBFilename)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

// ListDevinSessionMeta returns lightweight metadata for all non-hidden Devin
// sessions without parsing transcripts.
func ListDevinSessionMeta(dbPath string) ([]DevinSessionMeta, error) {
	var metas []DevinSessionMeta
	err := ForEachDevinSessionMeta(context.Background(), dbPath, func(meta DevinSessionMeta) error {
		metas = append(metas, meta)
		return nil
	})
	return metas, err
}

func ForEachDevinSessionMeta(
	ctx context.Context, dbPath string, yield func(DevinSessionMeta) error,
) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	db, err := openDevinDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT id,
		       COALESCE(title, ''),
		       COALESCE(working_directory, ''),
		       COALESCE(model, ''),
		       COALESCE(created_at, 0),
		       last_activity_at,
		       COALESCE(last_activity_at, created_at, 0)
		  FROM sessions
		 WHERE COALESCE(hidden, 0) <> 1
		 ORDER BY COALESCE(last_activity_at, created_at, 0) DESC, id DESC
	`)
	if err != nil {
		return fmt.Errorf("listing devin sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var meta DevinSessionMeta
		var createdAtMS int64
		var lastActivityMS sql.NullInt64
		var updatedAtMS int64
		if err := rows.Scan(
			&meta.RawSessionID,
			&meta.Title,
			&meta.CWD,
			&meta.Model,
			&createdAtMS,
			&lastActivityMS,
			&updatedAtMS,
		); err != nil {
			return fmt.Errorf("scanning devin session meta: %w", err)
		}
		meta.VirtualPath = VirtualSourcePath(dbPath, meta.RawSessionID)
		meta.CreatedAt = devinUnixMilli(createdAtMS)
		if lastActivityMS.Valid {
			meta.LastActivity = devinUnixMilli(lastActivityMS.Int64)
		}
		meta.UpdatedAt = devinUnixMilli(updatedAtMS)
		meta.FileMtime = updatedAtMS * 1_000_000
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(meta); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating devin sessions: %w", err)
	}
	return nil
}

func openDevinDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening devin db %s: %w", dbPath, err)
	}
	return db, nil
}

func getDevinSessionMeta(
	dbPath, rawSessionID string,
) (*DevinSessionMeta, error) {
	db, err := openDevinDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var meta DevinSessionMeta
	var createdAtMS int64
	var lastActivityMS sql.NullInt64
	var updatedAtMS int64
	err = db.QueryRow(`
		SELECT id,
		       COALESCE(title, ''),
		       COALESCE(working_directory, ''),
		       COALESCE(model, ''),
		       COALESCE(created_at, 0),
		       last_activity_at,
		       COALESCE(last_activity_at, created_at, 0)
		  FROM sessions
		 WHERE COALESCE(hidden, 0) <> 1
		   AND id = ?
	`, rawSessionID).Scan(
		&meta.RawSessionID,
		&meta.Title,
		&meta.CWD,
		&meta.Model,
		&createdAtMS,
		&lastActivityMS,
		&updatedAtMS,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("loading devin session meta: %w", err)
	}

	meta.VirtualPath = VirtualSourcePath(dbPath, meta.RawSessionID)
	meta.CreatedAt = devinUnixMilli(createdAtMS)
	if lastActivityMS.Valid {
		meta.LastActivity = devinUnixMilli(lastActivityMS.Int64)
	}
	meta.UpdatedAt = devinUnixMilli(updatedAtMS)
	meta.FileMtime = updatedAtMS * 1_000_000
	return &meta, nil
}

func devinUnixMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

type devinTranscriptError struct {
	op    string
	cause error
}

func (e *devinTranscriptError) Error() string {
	msg := fmt.Sprintf(
		"%s devin transcript %s for session %s",
		e.op,
		devinRedactedTranscriptPath(),
		devinRedactedSessionID(),
	)
	if e.cause != nil {
		return msg + ": " + devinTranscriptCauseMessage(e.cause)
	}
	return msg
}

func (e *devinTranscriptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newDevinTranscriptError(op string, cause error) error {
	return &devinTranscriptError{op: op, cause: cause}
}

func devinTranscriptCauseMessage(cause error) string {
	var pathErr *os.PathError
	if errors.As(cause, &pathErr) {
		if pathErr.Op != "" && pathErr.Err != nil {
			return pathErr.Op + ": " + pathErr.Err.Error()
		}
		if pathErr.Err != nil {
			return pathErr.Err.Error()
		}
		return pathErr.Op
	}
	return cause.Error()
}

func devinRedactedTranscriptPath() string {
	return filepath.Join("cli", "transcripts", "<redacted-session-id>.json")
}

func devinRedactedSessionID() string {
	return "<redacted-session-id>"
}

func parseDevinSession(dbPath, rawSessionID, machine string) (*ParsedSession, []ParsedMessage, error) {
	meta, err := getDevinSessionMeta(dbPath, rawSessionID)
	if err != nil {
		return nil, nil, err
	}
	if meta == nil {
		return nil, nil, sql.ErrNoRows
	}

	transcriptPath := filepath.Join(filepath.Dir(dbPath), "transcripts", rawSessionID+".json")
	info, err := os.Stat(transcriptPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, newDevinTranscriptError("stat", err)
		}
		fallbackErr := newDevinTranscriptError("missing", nil)
		sess, msgs, ok, err := parseDevinSessionFromMessageNodes(dbPath, rawSessionID, machine, meta)
		if err == nil && ok {
			return sess, msgs, nil
		}
		if err != nil {
			return nil, nil, fallbackErr
		}
		return nil, nil, fallbackErr
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil, nil, newDevinTranscriptError("read", err)
	}
	if !gjson.ValidBytes(data) {
		return nil, nil, newDevinTranscriptError("invalid", nil)
	}

	root := gjson.ParseBytes(data)
	steps := root.Get("steps")
	if !steps.IsArray() {
		return nil, nil, newDevinTranscriptError("missing steps array in", nil)
	}

	model := firstNonEmpty(root.Get("agent.model_name").Str, metaValue(meta, func(m *DevinSessionMeta) string { return m.Model }))
	cwd := firstNonEmpty(
		metaValue(meta, func(m *DevinSessionMeta) string { return m.CWD }),
		devinTranscriptCWD(root),
	)

	var (
		messages      []ParsedMessage
		firstMessage  string
		firstStepAt   time.Time
		lastStepAt    time.Time
		rootStartedAt = parseTimestamp(root.Get("created_at").Str)
		rootEndedAt   = parseTimestamp(root.Get("updated_at").Str)
		userMsgCount  int
		stepOrdinal   int
	)

	steps.ForEach(func(_, step gjson.Result) bool {
		msg, ok := parseDevinStep(step, stepOrdinal, model)
		stepOrdinal++
		if !ok {
			return true
		}
		messages = append(messages, msg)
		if firstStepAt.IsZero() && !msg.Timestamp.IsZero() {
			firstStepAt = msg.Timestamp
		}
		if msg.Timestamp.After(lastStepAt) {
			lastStepAt = msg.Timestamp
		}
		if msg.Role == RoleUser && strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
			if firstMessage == "" {
				firstMessage = truncate(strings.ReplaceAll(msg.Content, "\n", " "), 300)
			}
		}
		return true
	})

	startedAt := firstNonZeroTime(
		metaTime(meta, func(m *DevinSessionMeta) time.Time { return m.CreatedAt }),
		rootStartedAt,
		firstStepAt,
	)
	endedAt := firstNonZeroTime(
		metaTime(meta, func(m *DevinSessionMeta) time.Time { return m.LastActivity }),
		lastStepAt,
		rootEndedAt,
		startedAt,
	)

	fileInfo := devinBaseFileInfo(dbPath, rawSessionID)
	fileInfo.Size = info.Size()
	fileInfo.Mtime = info.ModTime().UnixNano()
	devinApplyFileInfoTimes(&fileInfo, meta, endedAt)

	sess := buildDevinParsedSession(meta, rawSessionID, machine, cwd, firstMessage, startedAt, endedAt, userMsgCount, messages, fileInfo)
	accumulateMessageTokenUsage(sess, messages)
	applyDevinFinalMetrics(sess, root.Get("final_metrics"))
	return sess, messages, nil
}

func parseDevinSessionFromMessageNodes(
	dbPath, rawSessionID, machine string,
	meta *DevinSessionMeta,
) (*ParsedSession, []ParsedMessage, bool, error) {
	rows, err := listDevinMessageNodes(dbPath, rawSessionID)
	if err != nil {
		return nil, nil, false, err
	}
	if len(rows) == 0 {
		return nil, nil, false, nil
	}

	model := metaValue(meta, func(m *DevinSessionMeta) string { return m.Model })
	cwd := metaValue(meta, func(m *DevinSessionMeta) string { return m.CWD })

	var (
		messages     []ParsedMessage
		firstMessage string
		firstStepAt  time.Time
		lastStepAt   time.Time
		userMsgCount int
	)
	for _, row := range rows {
		msg, ok, err := parseDevinDBMessageNode(row, len(messages), model)
		if err != nil {
			return nil, nil, false, err
		}
		if !ok {
			continue
		}
		messages = append(messages, msg)
		if firstStepAt.IsZero() && !msg.Timestamp.IsZero() {
			firstStepAt = msg.Timestamp
		}
		if msg.Timestamp.After(lastStepAt) {
			lastStepAt = msg.Timestamp
		}
		if msg.Role == RoleUser && !msg.IsSystem && strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
			if firstMessage == "" {
				firstMessage = truncate(strings.ReplaceAll(msg.Content, "\n", " "), 300)
			}
		}
	}
	if len(messages) == 0 {
		return nil, nil, false, nil
	}

	startedAt := firstNonZeroTime(
		metaTime(meta, func(m *DevinSessionMeta) time.Time { return m.CreatedAt }),
		firstStepAt,
	)
	endedAt := firstNonZeroTime(
		metaTime(meta, func(m *DevinSessionMeta) time.Time { return m.LastActivity }),
		lastStepAt,
		startedAt,
	)

	fileInfo := devinBaseFileInfo(dbPath, rawSessionID)
	devinApplyFileInfoTimes(&fileInfo, meta, endedAt)
	sess := buildDevinParsedSession(meta, rawSessionID, machine, cwd, firstMessage, startedAt, endedAt, userMsgCount, messages, fileInfo)
	return sess, messages, true, nil
}

type devinMessageNodeRow struct {
	RowID        int64
	NodeID       int64
	ParentNodeID sql.NullInt64
	ChatMessage  string
	CreatedAtMS  int64
}

func listDevinMessageNodes(dbPath, rawSessionID string) ([]devinMessageNodeRow, error) {
	db, err := openDevinDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT row_id,
		       node_id,
		       parent_node_id,
		       chat_message,
		       created_at
		  FROM message_nodes
		 WHERE session_id = ?
		 ORDER BY created_at ASC, row_id ASC
	`, rawSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []devinMessageNodeRow
	for rows.Next() {
		var row devinMessageNodeRow
		if err := rows.Scan(&row.RowID, &row.NodeID, &row.ParentNodeID, &row.ChatMessage, &row.CreatedAtMS); err != nil {
			return nil, err
		}
		nodes = append(nodes, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

func parseDevinDBMessageNode(
	row devinMessageNodeRow,
	ordinal int,
	model string,
) (ParsedMessage, bool, error) {
	if !gjson.Valid(row.ChatMessage) {
		return ParsedMessage{}, false, errors.New("invalid devin message_nodes chat_message")
	}
	root := gjson.Parse(row.ChatMessage)
	role, isSystem, ok := devinDBRole(root.Get("role").Str)
	if !ok {
		return ParsedMessage{}, false, nil
	}

	content, thinking, hasThinking, hasToolUse, toolCalls, toolResults := ExtractTextContent(root.Get("content"))
	topThinking := strings.TrimSpace(root.Get("thinking").Str)
	if topThinking != "" && topThinking != thinking {
		thinking = joinNonEmpty(thinking, topThinking)
		content = joinNonEmpty(content, "[Thinking]\n"+topThinking+"\n[/Thinking]")
		hasThinking = true
	}

	topLevelToolCalls, topLevelToolText := parseDevinDBToolCalls(root.Get("tool_calls"))
	if len(topLevelToolCalls) > 0 {
		toolCalls = append(toolCalls, topLevelToolCalls...)
		hasToolUse = true
		content = joinNonEmpty(content, topLevelToolText)
	}

	if role == RoleTool {
		if toolResult, ok := parseDevinDBToolResult(root.Get("tool_call_id"), root.Get("content")); ok {
			toolResults = append(toolResults, toolResult)
		}
	}

	if strings.TrimSpace(content) == "" && len(toolCalls) == 0 && len(toolResults) == 0 {
		return ParsedMessage{}, false, nil
	}

	msg := ParsedMessage{
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		ThinkingText:  thinking,
		Timestamp:     devinUnixMilli(row.CreatedAtMS),
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse || len(toolCalls) > 0,
		IsSystem:      isSystem,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
		ToolResults:   toolResults,
		Model:         model,
		SourceUUID:    fmt.Sprintf("%d", row.NodeID),
	}
	if row.ParentNodeID.Valid {
		msg.SourceParentUUID = fmt.Sprintf("%d", row.ParentNodeID.Int64)
	}
	return msg, true, nil
}

func parseDevinDBToolCalls(toolCalls gjson.Result) ([]ParsedToolCall, string) {
	if !toolCalls.IsArray() {
		return nil, ""
	}
	var (
		parsed []ParsedToolCall
		parts  []string
	)
	toolCalls.ForEach(func(_, tc gjson.Result) bool {
		parsedCall, ok := parseDevinDBToolCall(tc)
		if ok {
			parsed = append(parsed, parsedCall)
			if text := formatDevinDBToolCall(parsedCall); text != "" {
				parts = append(parts, text)
			}
		}
		return true
	})
	return parsed, strings.Join(parts, "\n")
}

func parseDevinDBToolCall(tc gjson.Result) (ParsedToolCall, bool) {
	if parsed, ok := parseToolCall(tc); ok {
		return parsed, true
	}
	name := firstNonEmpty(tc.Get("function.name").Str, tc.Get("name").Str)
	if name == "" {
		return ParsedToolCall{}, false
	}
	input := tc.Get("function.arguments")
	inputJSON := input.Raw
	if input.Type == gjson.String {
		inputJSON = input.Str
	}
	if inputJSON == "" {
		input = toolCallInput(tc)
		inputJSON = input.Raw
	}
	return ParsedToolCall{
		ToolUseID: tc.Get("id").Str,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: inputJSON,
	}, true
}

func formatDevinDBToolCall(tc ParsedToolCall) string {
	block := map[string]any{"name": tc.ToolName}
	if strings.TrimSpace(tc.InputJSON) != "" {
		var input any
		if json.Unmarshal([]byte(tc.InputJSON), &input) == nil {
			block["input"] = input
		}
	}
	if raw, err := json.Marshal(block); err == nil {
		return strings.TrimSpace(formatToolUse(gjson.ParseBytes(raw)))
	}
	return ""
}

func parseDevinDBToolResult(toolCallID, content gjson.Result) (ParsedToolResult, bool) {
	id := strings.TrimSpace(toolCallID.Str)
	if id == "" {
		return ParsedToolResult{}, false
	}
	return ParsedToolResult{
		ToolUseID:     id,
		ContentLength: toolResultContentLength(content),
		ContentRaw:    content.Raw,
	}, true
}

func devinDBRole(role string) (RoleType, bool, bool) {
	switch role {
	case "user":
		return RoleUser, false, true
	case "assistant", "agent":
		return RoleAssistant, false, true
	case "system":
		return RoleSystem, true, true
	case "tool":
		return RoleTool, false, true
	default:
		return "", false, false
	}
}

func devinBaseFileInfo(dbPath, rawSessionID string) FileInfo {
	return FileInfo{Path: VirtualSourcePath(dbPath, rawSessionID)}
}

func devinApplyFileInfoTimes(fileInfo *FileInfo, meta *DevinSessionMeta, endedAt time.Time) {
	if fileInfo == nil {
		return
	}
	if meta != nil && meta.FileMtime > 0 && meta.FileMtime > fileInfo.Mtime {
		fileInfo.Mtime = meta.FileMtime
	}
	if !endedAt.IsZero() && endedAt.UnixNano() > fileInfo.Mtime {
		fileInfo.Mtime = endedAt.UnixNano()
	}
}

func buildDevinParsedSession(
	meta *DevinSessionMeta,
	rawSessionID, machine, cwd, firstMessage string,
	startedAt, endedAt time.Time,
	userMsgCount int,
	messages []ParsedMessage,
	fileInfo FileInfo,
) *ParsedSession {
	sessionName := firstNonEmpty(
		metaValue(meta, func(m *DevinSessionMeta) string { return m.Title }),
		firstMessage,
	)
	return &ParsedSession{
		ID:               "devin:" + rawSessionID,
		Project:          ExtractProjectFromCwd(cwd),
		Machine:          machine,
		Agent:            AgentDevin,
		Cwd:              cwd,
		SourceSessionID:  rawSessionID,
		SessionName:      sessionName,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userMsgCount,
		File:             fileInfo,
	}
}

func metaValue[T any](meta *DevinSessionMeta, fn func(*DevinSessionMeta) T) T {
	var zero T
	if meta == nil {
		return zero
	}
	return fn(meta)
}

func metaTime(meta *DevinSessionMeta, fn func(*DevinSessionMeta) time.Time) time.Time {
	if meta == nil {
		return time.Time{}
	}
	return fn(meta)
}

func firstNonZeroTime(times ...time.Time) time.Time {
	for _, ts := range times {
		if !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}

func devinTranscriptCWD(root gjson.Result) string {
	if cwd := firstNonEmpty(
		root.Get("working_directory").Str,
		root.Get("cwd").Str,
		root.Get("agent.working_directory").Str,
		root.Get("agent.cwd").Str,
	); cwd != "" {
		return cwd
	}

	workspaceDirs := root.Get("workspace_dirs")
	if !workspaceDirs.IsArray() {
		return ""
	}
	first := workspaceDirs.Array()
	if len(first) == 0 {
		return ""
	}
	return firstNonEmpty(first[0].Str, first[0].Get("root_path").Str, first[0].Get("path").Str)
}

func applyDevinFinalMetrics(sess *ParsedSession, metrics gjson.Result) {
	if !metrics.Exists() || metrics.Type != gjson.JSON {
		return
	}

	if output, ok := firstPositiveGJSONInt(metrics,
		"output_tokens", "total_completion_tokens"); ok {
		sess.TotalOutputTokens = output
		sess.HasTotalOutputTokens = true
	}

	if !sess.HasPeakContextTokens {
		context := 0
		hasContext := false
		for _, key := range []string{
			"input_tokens",
			"cache_creation_input_tokens",
			"cache_read_input_tokens",
		} {
			if value, ok := positiveGJSONInt(metrics.Get(key)); ok {
				hasContext = true
				context += value
			}
		}
		if !hasContext {
			if value, ok := positiveGJSONInt(metrics.Get("total_prompt_tokens")); ok {
				hasContext = true
				context = value
			}
		}
		if !hasContext {
			if value, ok := positiveGJSONInt(metrics.Get("total_cached_tokens")); ok {
				hasContext = true
				context = value
			}
		}
		if !hasContext {
			if value, ok := positiveGJSONInt(metrics.Get("context_tokens")); ok {
				hasContext = true
				context = value
			}
		}
		if hasContext {
			sess.PeakContextTokens = context
			sess.HasPeakContextTokens = true
		}
	}
	sess.aggregateTokenPresenceKnown =
		sess.HasTotalOutputTokens || sess.HasPeakContextTokens
}

func firstPositiveGJSONInt(root gjson.Result, keys ...string) (int, bool) {
	for _, key := range keys {
		if value, ok := positiveGJSONInt(root.Get(key)); ok {
			return value, true
		}
	}
	return 0, false
}

func positiveGJSONInt(value gjson.Result) (int, bool) {
	if !value.Exists() {
		return 0, false
	}
	if n := int(value.Int()); n > 0 {
		return n, true
	}
	return 0, false
}

func parseDevinStep(step gjson.Result, ordinal int, model string) (ParsedMessage, bool) {
	role, isSystem, ok := devinRoleForSource(step.Get("source").Str)
	if !ok {
		return ParsedMessage{}, false
	}

	content, thinking, hasThinking, hasToolUse, toolCalls, toolResults :=
		ExtractTextContent(step.Get("message"))
	topLevelToolText, topLevelToolCalls := formatTopLevelToolUses(step.Get("tool_use"))
	if topLevelToolText != "" {
		content = joinNonEmpty(content, topLevelToolText)
		hasToolUse = true
	}
	toolCalls = append(toolCalls, topLevelToolCalls...)
	toolResults = append(toolResults, extractTopLevelToolResults(step.Get("tool_result"))...)

	if strings.TrimSpace(content) == "" && len(toolCalls) == 0 && len(toolResults) == 0 {
		return ParsedMessage{}, false
	}
	if role != RoleUser && strings.TrimSpace(content) == "" && len(toolCalls) == 0 && len(toolResults) > 0 {
		role = RoleTool
		isSystem = false
	}
	tokenUsage, contextTokens, outputTokens, hasContextTokens, hasOutputTokens :=
		devinTokenUsageFromMetrics(step.Get("metrics"))
	messageModel := firstNonEmpty(
		step.Get("extra.generation_model").Str,
		step.Get("model_name").Str,
		model,
	)

	return ParsedMessage{
		Ordinal:          ordinal,
		Role:             role,
		Content:          content,
		ThinkingText:     thinking,
		Timestamp:        devinStepTimestamp(step),
		HasThinking:      hasThinking,
		HasToolUse:       hasToolUse || len(toolCalls) > 0,
		IsSystem:         isSystem,
		ContentLength:    len(content),
		ToolCalls:        toolCalls,
		ToolResults:      toolResults,
		Model:            messageModel,
		TokenUsage:       tokenUsage,
		ContextTokens:    contextTokens,
		OutputTokens:     outputTokens,
		HasContextTokens: hasContextTokens,
		HasOutputTokens:  hasOutputTokens,
		SourceUUID:       devinStepID(step.Get("step_id")),
	}, true
}

func devinTokenUsageFromMetrics(metrics gjson.Result) (
	json.RawMessage, int, int, bool, bool,
) {
	if !metrics.Exists() || metrics.Type != gjson.JSON {
		return nil, 0, 0, false, false
	}

	prompt, hasPrompt := nonNegativeGJSONInt(metrics.Get("prompt_tokens"))
	completion, hasCompletion := nonNegativeGJSONInt(metrics.Get("completion_tokens"))
	cached, hasCached := nonNegativeGJSONInt(metrics.Get("cached_tokens"))
	if !hasPrompt && !hasCompletion && !hasCached {
		return nil, 0, 0, false, false
	}

	input := prompt
	if hasPrompt && hasCached {
		input -= cached
	}
	if input < 0 {
		input = 0
	}

	payload := make(map[string]int, 3)
	if hasPrompt {
		payload["input_tokens"] = input
	}
	if hasCompletion {
		payload["output_tokens"] = completion
	}
	if hasCached {
		payload["cache_read_input_tokens"] = cached
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, 0, false, false
	}

	context := input + cached
	return raw, context, completion, hasPrompt || hasCached, hasCompletion
}

func nonNegativeGJSONInt(value gjson.Result) (int, bool) {
	if !value.Exists() {
		return 0, false
	}
	n := int(value.Int())
	if n < 0 {
		return 0, true
	}
	return n, true
}

func devinStepID(stepID gjson.Result) string {
	switch stepID.Type {
	case gjson.String:
		return stepID.Str
	case gjson.Number, gjson.True, gjson.False, gjson.JSON:
		return stepID.Raw
	default:
		return ""
	}
}

func formatTopLevelToolUses(toolUses gjson.Result) (string, []ParsedToolCall) {
	if !toolUses.IsArray() {
		return "", nil
	}
	var (
		parts []string
		calls []ParsedToolCall
	)
	toolUses.ForEach(func(_, toolUse gjson.Result) bool {
		if text := strings.TrimSpace(formatToolUse(toolUse)); text != "" {
			parts = append(parts, text)
		}
		if tc, ok := parseToolCall(toolUse); ok {
			calls = append(calls, tc)
		}
		return true
	})
	return strings.Join(parts, "\n"), calls
}

func extractTopLevelToolResults(toolResults gjson.Result) []ParsedToolResult {
	if !toolResults.IsArray() {
		return nil
	}
	var parsed []ParsedToolResult
	toolResults.ForEach(func(_, toolResult gjson.Result) bool {
		if tr, ok := parseToolResult(toolResult); ok {
			parsed = append(parsed, tr)
		}
		return true
	})
	return parsed
}

func devinRoleForSource(source string) (RoleType, bool, bool) {
	switch source {
	case "user":
		return RoleUser, false, true
	case "agent":
		return RoleAssistant, false, true
	case "system":
		return RoleSystem, true, true
	default:
		return "", false, false
	}
}

func devinStepTimestamp(step gjson.Result) time.Time {
	return parseTimestamp(firstNonEmpty(
		step.Get("timestamp").Str,
		step.Get("created_at").Str,
		step.Get("createdAt").Str,
		step.Get("updated_at").Str,
		step.Get("updatedAt").Str,
	))
}

func joinNonEmpty(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "\n")
}
