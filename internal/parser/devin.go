package parser

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
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
	// MainChainID is the leaf node of the session's main conversation
	// chain (sessions.main_chain_id). message_nodes is a forest of
	// retries and edits; walking parents from this leaf yields the one
	// linear chain whose per-message metrics must be summed. Only
	// loaded on the per-session read path, not the discovery listing.
	MainChainID sql.NullInt64
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
		var createdAt int64
		var lastActivity sql.NullInt64
		var updatedAt int64
		if err := rows.Scan(
			&meta.RawSessionID,
			&meta.Title,
			&meta.CWD,
			&meta.Model,
			&createdAt,
			&lastActivity,
			&updatedAt,
		); err != nil {
			return fmt.Errorf("scanning devin session meta: %w", err)
		}
		meta.VirtualPath = VirtualSourcePath(dbPath, meta.RawSessionID)
		meta.CreatedAt = devinUnixSec(createdAt)
		if lastActivity.Valid {
			meta.LastActivity = devinUnixSec(lastActivity.Int64)
		}
		meta.UpdatedAt = devinUnixSec(updatedAt)
		meta.FileMtime = devinFileMtimeNS(updatedAt)
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
	var createdAt int64
	var lastActivity sql.NullInt64
	var updatedAt int64
	const (
		queryPrefix = `
		SELECT id,
		       COALESCE(title, ''),
		       COALESCE(working_directory, ''),
		       COALESCE(model, ''),
		       COALESCE(created_at, 0),
		       last_activity_at,
		       COALESCE(last_activity_at, created_at, 0),
		       `
		querySuffix = `
		  FROM sessions
		 WHERE COALESCE(hidden, 0) <> 1
		   AND id = ?
	`
		currentQuery = queryPrefix + "main_chain_id" + querySuffix
		legacyQuery  = queryPrefix + "NULL" + querySuffix
	)
	query := func(statement string) error {
		return db.QueryRow(statement, rawSessionID).Scan(
			&meta.RawSessionID,
			&meta.Title,
			&meta.CWD,
			&meta.Model,
			&createdAt,
			&lastActivity,
			&updatedAt,
			&meta.MainChainID,
		)
	}
	err = query(currentQuery)
	if err != nil && err != sql.ErrNoRows &&
		devinSessionsTablePredatesMainChainID(db) {
		meta.MainChainID = sql.NullInt64{}
		err = query(legacyQuery)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("loading devin session meta: %w", err)
	}

	meta.VirtualPath = VirtualSourcePath(dbPath, meta.RawSessionID)
	meta.CreatedAt = devinUnixSec(createdAt)
	if lastActivity.Valid {
		meta.LastActivity = devinUnixSec(lastActivity.Int64)
	}
	meta.UpdatedAt = devinUnixSec(updatedAt)
	meta.FileMtime = devinFileMtimeNS(updatedAt)
	return &meta, nil
}

// devinSessionsTablePredatesMainChainID distinguishes the known legacy schema
// from other query failures without relying on SQLite's error text. It runs
// only after the current metadata query fails, so current databases keep the
// single-query read path.
func devinSessionsTablePredatesMainChainID(db *sql.DB) bool {
	var tableExists, columnExists int
	err := db.QueryRow(`
		SELECT EXISTS (
		           SELECT 1
		             FROM sqlite_schema
		            WHERE type = 'table'
		              AND name = 'sessions'
		       ),
		       EXISTS (
		           SELECT 1
		             FROM pragma_table_info('sessions')
		            WHERE name = 'main_chain_id'
		       )
	`).Scan(&tableExists, &columnExists)
	return err == nil && tableExists == 1 && columnExists == 0
}

// devinMaxEpochSec is the largest epoch-second value whose nanosecond form
// still fits in int64 (year 2262). Anything larger is not a plausible Devin
// timestamp and signals a unit mismatch -- a 13-digit millisecond value, for
// example. Rejecting it keeps a bad column from silently overflowing into a
// far-future mtime, which would wedge change detection: devinApplyFileInfoTimes
// only ever raises Mtime, so a wrapped value can never be superseded and the
// session would stop resyncing.
const devinMaxEpochSec = math.MaxInt64 / int64(time.Second)

// devinPlausibleEpochSec reports whether sec is a usable Devin epoch-second
// timestamp. Devin stores created_at, last_activity_at, and
// message_nodes.created_at as Unix seconds.
func devinPlausibleEpochSec(sec int64) bool {
	return sec > 0 && sec <= devinMaxEpochSec
}

func devinUnixSec(sec int64) time.Time {
	if !devinPlausibleEpochSec(sec) {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// devinFileMtimeNS converts a Devin epoch-second timestamp to the nanosecond
// mtime the sync layer compares against. Implausible values yield 0, which
// callers already treat as "no synthetic mtime available".
func devinFileMtimeNS(sec int64) int64 {
	if !devinPlausibleEpochSec(sec) {
		return 0
	}
	return sec * int64(time.Second)
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
	if pathErr, ok := errors.AsType[*os.PathError](cause); ok {
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

	chain := devinMainChainRows(rows, meta)

	var (
		messages     []ParsedMessage
		firstMessage string
		firstStepAt  time.Time
		lastStepAt   time.Time
		userMsgCount int
	)
	for _, row := range chain {
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
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages, true, nil
}

// devinMainChainRows returns the message-node rows along the session's main
// conversation chain, ordered root-to-leaf. message_nodes is a forest:
// retries and edits create alternate branches, so summing every row would
// double-count token metrics. Starting at sessions.main_chain_id (the leaf)
// and walking parent_node_id links isolates the single chain that matches the
// exported transcript. When main_chain_id is missing or dangling, or the walk
// cannot reach a root (a cycle), it falls back to every row in the original
// created_at order so short/legacy sessions still parse.
func devinMainChainRows(
	rows []devinMessageNodeRow, meta *DevinSessionMeta,
) []devinMessageNodeRow {
	if meta == nil || !meta.MainChainID.Valid {
		return rows
	}
	byNodeID := make(map[int64]int, len(rows))
	for i := range rows {
		byNodeID[rows[i].NodeID] = i
	}
	leaf, ok := byNodeID[meta.MainChainID.Int64]
	if !ok {
		return rows
	}

	reversed := make([]devinMessageNodeRow, 0, len(rows))
	cur := rows[leaf]
	for {
		if len(reversed) == len(rows) {
			return rows // cycle: fall back rather than loop forever
		}
		reversed = append(reversed, cur)
		if !cur.ParentNodeID.Valid {
			break
		}
		parent, ok := byNodeID[cur.ParentNodeID.Int64]
		if !ok {
			return rows
		}
		cur = rows[parent]
	}

	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

type devinMessageNodeRow struct {
	RowID        int64
	NodeID       int64
	ParentNodeID sql.NullInt64
	ChatMessage  string
	CreatedAt    int64
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
		if err := rows.Scan(&row.RowID, &row.NodeID, &row.ParentNodeID, &row.ChatMessage, &row.CreatedAt); err != nil {
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

	tokenUsage, contextTokens, outputTokens, hasContextTokens, hasOutputTokens :=
		devinTokenUsageFromNodeMetrics(root.Get("metadata.metrics"))

	// The per-message generation model is the authoritative model for both
	// display and pricing: the session-level sessions.model column is often
	// empty or a coarse alias, and each request records the concrete model it
	// used at metadata.generation_model. Mirror the transcript path, which
	// prefers extra.generation_model over the session model.
	messageModel := firstNonEmpty(root.Get("metadata.generation_model").Str, model)

	// Keep otherwise-empty assistant turns that still consumed tokens (for
	// example a "stop" turn with no text and no tool calls): dropping them
	// would silently discard their prompt/completion usage and undercount
	// the session. Content-empty turns without any token metrics remain
	// skipped so genuinely empty nodes do not create blank messages.
	if strings.TrimSpace(content) == "" && len(toolCalls) == 0 && len(toolResults) == 0 &&
		!hasContextTokens && !hasOutputTokens {
		return ParsedMessage{}, false, nil
	}

	msg := ParsedMessage{
		Ordinal:          ordinal,
		Role:             role,
		Content:          content,
		ThinkingText:     thinking,
		Timestamp:        devinUnixSec(row.CreatedAt),
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
		SourceUUID:       fmt.Sprintf("%d", row.NodeID),
	}
	if row.ParentNodeID.Valid {
		msg.SourceParentUUID = fmt.Sprintf("%d", row.ParentNodeID.Int64)
	}
	return msg, true, nil
}

// devinTokenUsageFromNodeMetrics reads the per-assistant-message token counters
// that Devin records at chat_message -> metadata.metrics in message_nodes.
// Unlike transcript step metrics (prompt_tokens/completion_tokens/cached_tokens
// aggregates), node metrics expose the raw request fields separately, so cache
// creation and cache read are priced distinctly:
//
//	total_prompt_tokens = input_tokens + cache_read_tokens + cache_creation_tokens
//	total_completion    = output_tokens
//	total_cached        = cache_read_tokens
//
// Fields may be null (serialized as JSON null); those are treated as zero and
// do not by themselves establish presence.
func devinTokenUsageFromNodeMetrics(metrics gjson.Result) (
	jsontext.Value, int, int, bool, bool,
) {
	if !metrics.Exists() || metrics.Type != gjson.JSON {
		return nil, 0, 0, false, false
	}

	input, hasInput := devinMetricInt(metrics.Get("input_tokens"))
	output, hasOutput := devinMetricInt(metrics.Get("output_tokens"))
	cacheRead, hasCacheRead := devinMetricInt(metrics.Get("cache_read_tokens"))
	cacheCreation, hasCacheCreation := devinMetricInt(metrics.Get("cache_creation_tokens"))

	hasContext := hasInput || hasCacheRead || hasCacheCreation
	if !hasContext && !hasOutput {
		return nil, 0, 0, false, false
	}

	payload := make(map[string]int, 4)
	if hasInput {
		payload["input_tokens"] = input
	}
	if hasOutput {
		payload["output_tokens"] = output
	}
	if hasCacheRead {
		payload["cache_read_input_tokens"] = cacheRead
	}
	if hasCacheCreation {
		payload["cache_creation_input_tokens"] = cacheCreation
	}
	raw, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return nil, 0, 0, false, false
	}

	context := input + cacheRead + cacheCreation
	return raw, context, output, hasContext, hasOutput
}

// devinMetricInt reads a non-negative token counter from a metrics field.
// Presence requires an actual JSON number: absent fields and explicit null
// (both common in metadata.metrics) report not-present and contribute zero.
func devinMetricInt(value gjson.Result) (int, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	if n := int(value.Int()); n > 0 {
		return n, true
	}
	return 0, true
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
	jsontext.Value, int, int, bool, bool,
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
	raw, err := json.Marshal(payload, json.Deterministic(true))
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
