package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
)

type gooseSessionRow struct {
	id                     string
	name                   string
	description            string
	sessionType            string
	workingDir             string
	createdAt              string
	updatedAt              string
	providerName           string
	modelConfigJSON        string
	projectID              string
	parentSessionID        string
	accumulatedInput       int64
	accumulatedOutput      int64
	accumulatedTotal       int64
	accumulatedCacheRead   int64
	accumulatedCacheWrite  int64
	accumulatedCostDollars sql.NullFloat64
}

type gooseMessageRow struct {
	id               int64
	role             string
	contentJSON      string
	metadataJSON     string
	createdTimestamp int64
	timestamp        string
}

type gooseUsageRow struct {
	id               int64
	sessionID        string
	createdTimestamp int64
	model            string
	inputTokens      int64
	outputTokens     int64
	totalTokens      int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	cost             sql.NullFloat64
	costSource       string
	isCompaction     bool
}

func forEachGooseSessionMeta(
	ctx context.Context,
	dbPath string,
	yield func(dbBackedSessionMeta) error,
) error {
	if !IsRegularFile(dbPath) {
		return nil
	}
	db, err := openGooseDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	columns, err := gooseSessionColumns(ctx, db)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, gooseSessionSelect(columns, "", true))
	if err != nil {
		return fmt.Errorf("listing goose sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanGooseSessionRow(rows)
		if err != nil {
			return err
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(dbBackedSessionMeta{
			SessionID:   row.id,
			VirtualPath: GooseSQLiteVirtualPath(dbPath, row.id),
			FileMtime:   gooseSessionFileMtime(dbPath, db, row),
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func gooseSessionMeta(
	ctx context.Context, dbPath, sessionID string,
) (dbBackedSessionMeta, bool, error) {
	if !IsRegularFile(dbPath) {
		return dbBackedSessionMeta{}, false, nil
	}
	db, err := openGooseDB(dbPath)
	if err != nil {
		return dbBackedSessionMeta{}, false, err
	}
	defer db.Close()
	row, found, err := loadGooseSessionRow(ctx, db, sessionID)
	if err != nil || !found {
		return dbBackedSessionMeta{}, found, err
	}
	return dbBackedSessionMeta{
		SessionID:   row.id,
		VirtualPath: GooseSQLiteVirtualPath(dbPath, row.id),
		FileMtime:   gooseSessionFileMtime(dbPath, db, row),
	}, true, nil
}

func parseGooseSession(
	dbPath, sessionID, machine string,
) (*ParseResult, error) {
	db, err := openGooseDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	row, found, err := loadGooseSessionRow(context.Background(), db, sessionID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, sql.ErrNoRows
	}
	result, err := buildGooseParseResult(dbPath, machine, row, db)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func loadGooseSessionRow(
	ctx context.Context, db *sql.DB, sessionID string,
) (gooseSessionRow, bool, error) {
	columns, err := gooseSessionColumns(ctx, db)
	if err != nil {
		return gooseSessionRow{}, false, err
	}
	row := db.QueryRowContext(ctx, gooseSessionSelect(columns, " WHERE id = ?", false), sessionID)
	result, err := scanGooseSessionRowScanner(row)
	if errors.Is(err, sql.ErrNoRows) {
		return gooseSessionRow{}, false, nil
	}
	if err != nil {
		return gooseSessionRow{}, false, fmt.Errorf("loading goose session %s: %w", sessionID, err)
	}
	return result, true, nil
}

type gooseRowScanner interface {
	Scan(...any) error
}

func scanGooseSessionRow(rows *sql.Rows) (gooseSessionRow, error) {
	row, err := scanGooseSessionRowScanner(rows)
	if err != nil {
		return gooseSessionRow{}, fmt.Errorf("scanning goose session metadata: %w", err)
	}
	return row, nil
}

func scanGooseSessionRowScanner(scanner gooseRowScanner) (gooseSessionRow, error) {
	var row gooseSessionRow
	err := scanner.Scan(
		&row.id,
		&row.name,
		&row.description,
		&row.sessionType,
		&row.workingDir,
		&row.createdAt,
		&row.updatedAt,
		&row.providerName,
		&row.modelConfigJSON,
		&row.projectID,
		&row.parentSessionID,
		&row.accumulatedInput,
		&row.accumulatedOutput,
		&row.accumulatedTotal,
		&row.accumulatedCacheRead,
		&row.accumulatedCacheWrite,
		&row.accumulatedCostDollars,
	)
	return row, err
}

func gooseSessionColumns(
	ctx context.Context, db *sql.DB,
) (map[string]bool, error) {
	columns, err := gooseTableColumns(ctx, db, "sessions")
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"id", "working_dir", "created_at", "updated_at"} {
		if !columns[required] {
			return nil, fmt.Errorf("unsupported goose sessions schema: missing sessions.%s", required)
		}
	}
	hasMessages, err := gooseTableExists(ctx, db, "messages")
	if err != nil {
		return nil, err
	}
	if !hasMessages {
		return nil, fmt.Errorf("unsupported goose sessions schema: missing messages table")
	}
	return columns, nil
}

func gooseSessionSelect(
	columns map[string]bool, suffix string, order bool,
) string {
	expressions := []string{
		"id",
		gooseTextColumn(columns, "name", ""),
		gooseTextColumn(columns, "description", ""),
		gooseTextColumn(columns, "session_type", "user"),
		gooseTextColumn(columns, "working_dir", ""),
		gooseTextColumn(columns, "created_at", ""),
		gooseTextColumn(columns, "updated_at", ""),
		gooseTextColumn(columns, "provider_name", ""),
		gooseTextColumn(columns, "model_config_json", ""),
		gooseTextColumn(columns, "project_id", ""),
		gooseTextColumn(columns, "parent_session_id", ""),
		gooseIntColumn(columns, "accumulated_input_tokens"),
		gooseIntColumn(columns, "accumulated_output_tokens"),
		gooseIntColumn(columns, "accumulated_total_tokens"),
		gooseIntColumn(columns, "accumulated_cache_read_tokens"),
		gooseIntColumn(columns, "accumulated_cache_write_tokens"),
		gooseNullableColumn(columns, "accumulated_cost"),
	}
	query := "SELECT " + strings.Join(expressions, ", ") + " FROM sessions" + suffix
	if order {
		query += " ORDER BY id"
	}
	return query
}

func gooseTextColumn(columns map[string]bool, name, fallback string) string {
	if !columns[name] {
		return gooseSQLString(fallback)
	}
	return "CAST(COALESCE(" + name + ", " + gooseSQLString(fallback) + ") AS TEXT)"
}

func gooseSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func gooseIntColumn(columns map[string]bool, name string) string {
	if !columns[name] {
		return "0"
	}
	return "COALESCE(" + name + ", 0)"
}

func gooseNullableColumn(columns map[string]bool, name string) string {
	if !columns[name] {
		return "NULL"
	}
	return name
}

func buildGooseParseResult(
	dbPath, machine string, row gooseSessionRow, db *sql.DB,
) (ParseResult, error) {
	messages, err := loadGooseMessages(db, row.id, gooseSessionModel(row))
	if err != nil {
		return ParseResult{}, err
	}
	startedAt := gooseParseTime(row.createdAt)
	endedAt := gooseParseTime(row.updatedAt)
	for _, message := range messages {
		if startedAt.IsZero() || (!message.Timestamp.IsZero() && message.Timestamp.Before(startedAt)) {
			startedAt = message.Timestamp
		}
		if message.Timestamp.After(endedAt) {
			endedAt = message.Timestamp
		}
	}
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	sessionName := strings.TrimSpace(row.name)
	if sessionName == "" {
		sessionName = strings.TrimSpace(row.description)
	}
	firstMessage := gooseFirstMessage(messages)
	if firstMessage == "" {
		firstMessage = truncate(strings.ReplaceAll(sessionName, "\n", " "), 300)
	}
	project := ExtractProjectFromCwd(row.workingDir)
	if project == "" {
		if projectID := strings.TrimSpace(row.projectID); projectID != "" {
			project = "project-" + projectID
		} else {
			project = "unknown"
		}
	}

	userMessages := 0
	for _, message := range messages {
		if message.Role == RoleUser && len(message.ToolResults) == 0 {
			userMessages++
		}
	}
	schemaVersion, err := gooseSchemaVersion(context.Background(), db)
	if err != nil {
		return ParseResult{}, err
	}
	session := ParsedSession{
		ID:               "goose:" + row.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentGoose,
		Cwd:              row.workingDir,
		SourceSessionID:  row.id,
		SourceVersion:    fmt.Sprintf("goose-sqlite-v%d", schemaVersion),
		FirstMessage:     firstMessage,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userMessages,
		File: FileInfo{
			Path:  GooseSQLiteVirtualPath(dbPath, row.id),
			Mtime: gooseSessionFileMtime(dbPath, db, row),
		},
	}
	if parentID := strings.TrimSpace(row.parentSessionID); parentID != "" {
		session.ParentSessionID = "goose:" + parentID
		session.RelationshipType = RelSubagent
	}
	usageEvents, err := listGooseUsageEvents(db, row, startedAt, endedAt)
	if err != nil {
		return ParseResult{}, err
	}
	for _, event := range usageEvents {
		if eventTime := gooseParseTime(event.OccurredAt); eventTime.After(session.EndedAt) {
			session.EndedAt = eventTime
		}
	}
	applyUsageEventTokenTotals(&session, usageEvents)
	return ParseResult{
		Session:     session,
		Messages:    messages,
		UsageEvents: usageEvents,
	}, nil
}

func loadGooseMessages(
	db *sql.DB, sessionID, sessionModel string,
) ([]ParsedMessage, error) {
	rows, err := db.Query(`
		SELECT id,
		       COALESCE(role, ''),
		       COALESCE(content_json, '[]'),
		       COALESCE(metadata_json, ''),
		       COALESCE(created_timestamp, 0),
		       CAST(COALESCE(timestamp, '') AS TEXT)
		  FROM messages
		 WHERE session_id = ?
		 ORDER BY created_timestamp, id
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing goose messages for %s: %w", sessionID, err)
	}
	defer rows.Close()
	parsed := make([]ParsedMessage, 0)
	for rows.Next() {
		var row gooseMessageRow
		if err := rows.Scan(
			&row.id, &row.role, &row.contentJSON, &row.metadataJSON,
			&row.createdTimestamp, &row.timestamp,
		); err != nil {
			return nil, fmt.Errorf("scanning goose message row: %w", err)
		}
		message, ok, err := buildGooseMessage(len(parsed), row, sessionModel)
		if err != nil {
			return nil, err
		}
		if ok {
			parsed = append(parsed, message)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return parsed, nil
}

func buildGooseMessage(
	ordinal int, row gooseMessageRow, sessionModel string,
) (ParsedMessage, bool, error) {
	if visible := gjson.Get(row.metadataJSON, "userVisible"); visible.Exists() && !visible.Bool() {
		return ParsedMessage{}, false, nil
	}
	role, ok := normalizeGooseRole(row.role)
	if !ok {
		return ParsedMessage{}, false, nil
	}
	content := gjson.Parse(row.contentJSON)
	if !gjson.Valid(row.contentJSON) || !content.IsArray() {
		return ParsedMessage{}, false, fmt.Errorf(
			"parsing goose message %d content_json: expected JSON array", row.id,
		)
	}
	message := ParsedMessage{
		Ordinal:   ordinal,
		Role:      role,
		Timestamp: gooseMessageTime(row.createdTimestamp, row.timestamp),
	}
	if role == RoleAssistant {
		message.Model = sessionModel
	}
	var texts []string
	var thinking []string
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			if text := strings.TrimSpace(block.Get("text").Str); text != "" {
				texts = append(texts, text)
			}
		case "thinking":
			message.HasThinking = true
			if text := strings.TrimSpace(block.Get("thinking").Str); text != "" {
				thinking = append(thinking, text)
				texts = append(texts, "[Thinking]\n"+text+"\n[/Thinking]")
			}
		case "redactedThinking":
			message.HasThinking = true
		case "toolRequest", "frontendToolRequest":
			message.HasToolUse = true
			if call, ok := gooseParseToolCall(block); ok {
				message.ToolCalls = append(message.ToolCalls, call)
			}
		case "toolResponse":
			if result, ok := gooseParseToolResult(block); ok {
				message.ToolResults = append(message.ToolResults, result)
			}
		case "image":
			texts = append(texts, "[Image]")
		case "toolConfirmationRequest", "actionRequired", "systemNotification":
			if text := gooseVisibleBlockText(block); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	message.Content = strings.Join(texts, "\n")
	message.ThinkingText = strings.Join(thinking, "\n\n")
	message.ContentLength = len(message.Content)
	return message, true, nil
}

func normalizeGooseRole(role string) (RoleType, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return RoleUser, true
	case "assistant":
		return RoleAssistant, true
	default:
		return "", false
	}
}

func gooseParseToolCall(block gjson.Result) (ParsedToolCall, bool) {
	toolUseID := strings.TrimSpace(block.Get("id").Str)
	envelope := block.Get("toolCall")
	if toolUseID == "" || envelope.Get("status").Str != "success" {
		return ParsedToolCall{}, false
	}
	value := envelope.Get("value")
	name := strings.TrimSpace(value.Get("name").Str)
	if name == "" {
		return ParsedToolCall{}, false
	}
	arguments := value.Get("arguments")
	inputJSON := arguments.Raw
	if inputJSON == "" || inputJSON == "null" {
		inputJSON = "{}"
	}
	call := ParsedToolCall{
		ToolUseID: toolUseID,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: inputJSON,
	}
	if strings.EqualFold(name, "skill") {
		call.SkillName = arguments.Get("skill").Str
		if call.SkillName == "" {
			call.SkillName = arguments.Get("name").Str
		}
	} else {
		call.SkillName = inferToolSkillName(name, inputJSON)
	}
	return call, true
}

func gooseParseToolResult(block gjson.Result) (ParsedToolResult, bool) {
	toolUseID := strings.TrimSpace(block.Get("id").Str)
	if toolUseID == "" {
		return ParsedToolResult{}, false
	}
	envelope := block.Get("toolResult")
	switch envelope.Get("status").Str {
	case "error":
		text := envelope.Get("error").Str
		return ParsedToolResult{
			ToolUseID:     toolUseID,
			ContentLength: len(text),
			ContentRaw:    strconv.Quote(text),
		}, true
	case "success":
		value := envelope.Get("value")
		content := value.Get("content")
		if !content.Exists() && value.IsArray() {
			content = value
		}
		if !content.Exists() || content.Type == gjson.Null {
			return ParsedToolResult{ToolUseID: toolUseID, ContentRaw: "null"}, true
		}
		return ParsedToolResult{
			ToolUseID:     toolUseID,
			ContentLength: toolResultContentLength(content),
			ContentRaw:    content.Raw,
		}, true
	default:
		return ParsedToolResult{}, false
	}
}

func gooseVisibleBlockText(block gjson.Result) string {
	for _, path := range []string{"text", "message", "prompt", "reason"} {
		value := block.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.String {
			return strings.TrimSpace(value.Str)
		}
		if text := strings.TrimSpace(decodeContent(value)); text != "" {
			return text
		}
	}
	return ""
}

func gooseFirstMessage(messages []ParsedMessage) string {
	for _, message := range messages {
		if message.Role != RoleUser {
			continue
		}
		text := strings.TrimSpace(message.Content)
		if text == "" || text == "[Image]" {
			continue
		}
		return truncate(strings.ReplaceAll(text, "\n", " "), 300)
	}
	return ""
}

func gooseSessionModel(row gooseSessionRow) string {
	for _, path := range []string{"model_name", "modelName", "model"} {
		if model := strings.TrimSpace(gjson.Get(row.modelConfigJSON, path).Str); model != "" {
			return model
		}
	}
	return ""
}

func listGooseUsageEvents(
	db *sql.DB,
	row gooseSessionRow,
	startedAt, endedAt time.Time,
) ([]ParsedUsageEvent, error) {
	hasLedger, err := gooseTableExists(context.Background(), db, "usage_ledger")
	if err != nil {
		return nil, err
	}
	if !hasLedger {
		return gooseAggregateUsageFallback(row, startedAt, endedAt), nil
	}
	rows, err := db.Query(`
		SELECT id,
		       session_id,
		       COALESCE(created_timestamp, 0),
		       COALESCE(model, ''),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(total_tokens, 0),
		       COALESCE(cache_read_tokens, 0),
		       COALESCE(cache_write_tokens, 0),
		       cost,
		       COALESCE(cost_source, ''),
		       COALESCE(is_compaction, 0)
		  FROM usage_ledger
		 WHERE session_id = ?
		 ORDER BY created_timestamp, id
	`, row.id)
	if err != nil {
		return nil, fmt.Errorf("listing goose usage ledger for %s: %w", row.id, err)
	}
	defer rows.Close()
	events := make([]ParsedUsageEvent, 0)
	for rows.Next() {
		var usage gooseUsageRow
		if err := rows.Scan(
			&usage.id, &usage.sessionID, &usage.createdTimestamp,
			&usage.model, &usage.inputTokens, &usage.outputTokens,
			&usage.totalTokens, &usage.cacheReadTokens,
			&usage.cacheWriteTokens, &usage.cost, &usage.costSource,
			&usage.isCompaction,
		); err != nil {
			return nil, fmt.Errorf("scanning goose usage row: %w", err)
		}
		events = append(events, gooseUsageEvent(usage, gooseSessionModel(row), startedAt, endedAt))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return gooseAggregateUsageFallback(row, startedAt, endedAt), nil
	}
	return events, nil
}

func gooseUsageEvent(
	row gooseUsageRow, fallbackModel string, startedAt, endedAt time.Time,
) ParsedUsageEvent {
	model := strings.TrimSpace(row.model)
	if model == "" {
		model = fallbackModel
	}
	event := ParsedUsageEvent{
		SessionID:                "goose:" + row.sessionID,
		Source:                   "message",
		Model:                    model,
		InputTokens:              nonnegativeGooseToken(row.inputTokens),
		OutputTokens:             nonnegativeGooseToken(row.outputTokens),
		CacheCreationInputTokens: nonnegativeGooseToken(row.cacheWriteTokens),
		CacheReadInputTokens:     nonnegativeGooseToken(row.cacheReadTokens),
		OccurredAt: timeString(
			gooseUnixTimestamp(row.createdTimestamp), maxGooseTime(endedAt, startedAt),
		),
		DedupKey: gooseUsageDedupKey(row),
	}
	if row.cost.Valid && row.cost.Float64 >= 0 && !math.IsNaN(row.cost.Float64) &&
		!math.IsInf(row.cost.Float64, 0) {
		if parsed, err := money.FromFloatDollars(row.cost.Float64); err == nil {
			event.Cost = &parsed
			event.CostStatus, event.CostSource = gooseCostProvenance(row.costSource)
		}
	}
	return event
}

func gooseAggregateUsageFallback(
	row gooseSessionRow, startedAt, endedAt time.Time,
) []ParsedUsageEvent {
	if row.accumulatedInput <= 0 && row.accumulatedOutput <= 0 &&
		row.accumulatedCacheRead <= 0 && row.accumulatedCacheWrite <= 0 &&
		(!row.accumulatedCostDollars.Valid || row.accumulatedCostDollars.Float64 < 0) {
		return nil
	}
	event := ParsedUsageEvent{
		SessionID:                "goose:" + row.id,
		Source:                   "session",
		Model:                    gooseSessionModel(row),
		InputTokens:              nonnegativeGooseToken(row.accumulatedInput),
		OutputTokens:             nonnegativeGooseToken(row.accumulatedOutput),
		CacheCreationInputTokens: nonnegativeGooseToken(row.accumulatedCacheWrite),
		CacheReadInputTokens:     nonnegativeGooseToken(row.accumulatedCacheRead),
		OccurredAt:               timeString(endedAt, startedAt),
		DedupKey:                 "session:goose:" + row.id + "|aggregate",
	}
	if row.accumulatedCostDollars.Valid && row.accumulatedCostDollars.Float64 >= 0 {
		if parsed, err := money.FromFloatDollars(row.accumulatedCostDollars.Float64); err == nil {
			event.Cost = &parsed
			event.CostStatus = "unknown"
			event.CostSource = "goose-accumulated"
		}
	}
	return []ParsedUsageEvent{event}
}

func gooseCostProvenance(source string) (string, string) {
	switch strings.TrimSpace(source) {
	case "provider_reported":
		return "exact", "goose-provider-reported"
	case "estimated":
		return "estimated", "goose-estimated"
	case "carried_forward":
		return "unknown", "goose-carried-forward"
	case "":
		return "unknown", "goose"
	default:
		return "unknown", "goose-" + strings.TrimSpace(source)
	}
}

func gooseUsageDedupKey(row gooseUsageRow) string {
	return strings.Join([]string{
		"session:goose:" + row.sessionID,
		"ledger_id=" + strconv.FormatInt(row.id, 10),
		"created_timestamp=" + strconv.FormatInt(row.createdTimestamp, 10),
		"model=" + row.model,
		"input_tokens=" + strconv.FormatInt(row.inputTokens, 10),
		"output_tokens=" + strconv.FormatInt(row.outputTokens, 10),
		"total_tokens=" + strconv.FormatInt(row.totalTokens, 10),
		"cache_read_tokens=" + strconv.FormatInt(row.cacheReadTokens, 10),
		"cache_write_tokens=" + strconv.FormatInt(row.cacheWriteTokens, 10),
		"cost=" + gooseNullFloatString(row.cost),
		"cost_source=" + row.costSource,
		"is_compaction=" + strconv.FormatBool(row.isCompaction),
	}, "|")
}

func gooseNullFloatString(value sql.NullFloat64) string {
	if !value.Valid {
		return ""
	}
	return strconv.FormatFloat(value.Float64, 'g', -1, 64)
}

func nonnegativeGooseToken(value int64) int {
	if value <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func gooseParseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if timestamp := parseTimestamp(raw); !timestamp.IsZero() {
		return timestamp
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return gooseUnixTimestamp(value)
}

func gooseUnixTimestamp(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value > 10_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func gooseMessageTime(createdTimestamp int64, fallback string) time.Time {
	if timestamp := gooseUnixTimestamp(createdTimestamp); !timestamp.IsZero() {
		return timestamp
	}
	return gooseParseTime(fallback)
}

func gooseSessionFileMtime(
	dbPath string, db *sql.DB, row gooseSessionRow,
) int64 {
	maxTime := maxGooseTime(gooseParseTime(row.updatedAt), gooseParseTime(row.createdAt))
	var messageTimestamp sql.NullInt64
	if err := db.QueryRow(
		"SELECT MAX(created_timestamp) FROM messages WHERE session_id = ?", row.id,
	).Scan(&messageTimestamp); err == nil && messageTimestamp.Valid {
		maxTime = maxGooseTime(maxTime, gooseUnixTimestamp(messageTimestamp.Int64))
	}
	if hasUsage, err := gooseTableExists(context.Background(), db, "usage_ledger"); err == nil && hasUsage {
		var usageTimestamp sql.NullInt64
		if err := db.QueryRow(
			"SELECT MAX(created_timestamp) FROM usage_ledger WHERE session_id = ?", row.id,
		).Scan(&usageTimestamp); err == nil && usageTimestamp.Valid {
			maxTime = maxGooseTime(maxTime, gooseUnixTimestamp(usageTimestamp.Int64))
		}
	}
	if !maxTime.IsZero() {
		return maxTime.UnixNano()
	}
	mtime, _ := sqliteDBCompositeMtime(dbPath, []string{"", "-wal"})
	return mtime
}

func maxGooseTime(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if value.After(result) {
			result = value
		}
	}
	return result
}

func gooseSessionFingerprint(
	ctx context.Context, dbPath, sessionID string,
) (string, bool, error) {
	db, err := openGooseDB(dbPath)
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	row, found, err := loadGooseSessionRow(ctx, db, sessionID)
	if err != nil || !found {
		return "", found, err
	}
	hasher := sha256.New()
	for _, value := range []string{
		row.id, row.name, row.description, row.sessionType, row.workingDir,
		row.createdAt, row.updatedAt, row.providerName, row.modelConfigJSON,
		row.projectID, row.parentSessionID,
		strconv.FormatInt(row.accumulatedInput, 10),
		strconv.FormatInt(row.accumulatedOutput, 10),
		strconv.FormatInt(row.accumulatedTotal, 10),
		strconv.FormatInt(row.accumulatedCacheRead, 10),
		strconv.FormatInt(row.accumulatedCacheWrite, 10),
		gooseNullFloatString(row.accumulatedCostDollars),
	} {
		gooseWriteFingerprintField(hasher, value)
	}
	messageRows, err := db.QueryContext(ctx, `
		SELECT CAST(id AS TEXT), COALESCE(message_id, ''), COALESCE(role, ''),
		       COALESCE(content_json, ''), CAST(COALESCE(created_timestamp, 0) AS TEXT),
		       CAST(COALESCE(timestamp, '') AS TEXT), CAST(COALESCE(tokens, 0) AS TEXT),
		       COALESCE(metadata_json, '')
		  FROM messages WHERE session_id = ? ORDER BY id
	`, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("fingerprinting goose messages: %w", err)
	}
	for messageRows.Next() {
		var values [8]string
		if err := messageRows.Scan(
			&values[0], &values[1], &values[2], &values[3],
			&values[4], &values[5], &values[6], &values[7],
		); err != nil {
			_ = messageRows.Close()
			return "", false, fmt.Errorf("scanning goose fingerprint message: %w", err)
		}
		for _, value := range values {
			gooseWriteFingerprintField(hasher, value)
		}
	}
	if err := messageRows.Err(); err != nil {
		_ = messageRows.Close()
		return "", false, err
	}
	if err := messageRows.Close(); err != nil {
		return "", false, err
	}

	hasUsage, err := gooseTableExists(ctx, db, "usage_ledger")
	if err != nil {
		return "", false, err
	}
	if hasUsage {
		usageRows, err := db.QueryContext(ctx, `
			SELECT CAST(id AS TEXT), session_id, CAST(created_timestamp AS TEXT),
			       COALESCE(model, ''), CAST(COALESCE(input_tokens, 0) AS TEXT),
			       CAST(COALESCE(output_tokens, 0) AS TEXT),
			       CAST(COALESCE(total_tokens, 0) AS TEXT),
			       CAST(COALESCE(cache_read_tokens, 0) AS TEXT),
			       CAST(COALESCE(cache_write_tokens, 0) AS TEXT),
			       CAST(COALESCE(cost, '') AS TEXT), COALESCE(cost_source, ''),
			       CAST(COALESCE(is_compaction, 0) AS TEXT)
			  FROM usage_ledger WHERE session_id = ? ORDER BY id
		`, sessionID)
		if err != nil {
			return "", false, fmt.Errorf("fingerprinting goose usage: %w", err)
		}
		for usageRows.Next() {
			values := make([]string, 12)
			destinations := make([]any, len(values))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := usageRows.Scan(destinations...); err != nil {
				_ = usageRows.Close()
				return "", false, fmt.Errorf("scanning goose fingerprint usage: %w", err)
			}
			for _, value := range values {
				gooseWriteFingerprintField(hasher, value)
			}
		}
		if err := usageRows.Err(); err != nil {
			_ = usageRows.Close()
			return "", false, err
		}
		if err := usageRows.Close(); err != nil {
			return "", false, err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

func gooseWriteFingerprintField(hasher hash.Hash, value string) {
	_, _ = hasher.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(value))
}

func gooseSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	hasVersion, err := gooseTableExists(ctx, db, "schema_version")
	if err != nil {
		return 0, err
	}
	if !hasVersion {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(
		ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version",
	).Scan(&version); err != nil {
		return 0, fmt.Errorf("reading goose schema version: %w", err)
	}
	return version, nil
}

func gooseTableExists(
	ctx context.Context, db *sql.DB, table string,
) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?
	`, table).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking goose table %s: %w", table, err)
	}
	return count > 0, nil
}

func gooseTableColumns(
	ctx context.Context, db *sql.DB, table string,
) (map[string]bool, error) {
	if table != "sessions" && table != "messages" && table != "usage_ledger" {
		return nil, fmt.Errorf("unsupported goose schema table %q", table)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, fmt.Errorf("listing goose %s columns: %w", table, err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			typeName   string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(
			&cid, &name, &typeName, &notNull, &defaultV, &primaryKey,
		); err != nil {
			return nil, fmt.Errorf("scanning goose %s columns: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}
