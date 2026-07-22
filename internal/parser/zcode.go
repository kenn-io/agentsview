package parser

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const ZCodeDBName = "db.sqlite"

const zcodeDBName = ZCodeDBName

type zcodeProviderFactory struct {
	def AgentDef
}

func newZcodeProviderFactory(def AgentDef) ProviderFactory {
	return zcodeProviderFactory{def: cloneAgentDef(def)}
}

func (f zcodeProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f zcodeProviderFactory) Capabilities() Capabilities {
	return zcodeProviderCapabilities()
}

func (f zcodeProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	cfg.Roots = normalizeZCodeRoots(cfg.Roots)
	spec := zcodeProviderSpec()
	return &dbBackedProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   spec.caps,
			Config: cfg,
		},
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
}

func zcodeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: dbBackedSourceCapabilities(CapabilityNotApplicable),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}

func zcodeProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentZCode,
		dbName: zcodeDBName,
		findDB: zcodeDBPath,
		streamMeta: func(
			ctx context.Context,
			dbPath string,
			yield func(dbBackedSessionMeta) error,
		) error {
			return forEachZCodeSessionMeta(ctx, dbPath, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return zcodeSessionMeta(ctx, dbPath, sessionID)
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			result, err := parseZCodeSession(dbPath, sessionID, machine)
			if err != nil || result == nil {
				return nil, err
			}
			return []ParseResult{*result}, nil
		},
		caps: zcodeProviderCapabilities(),
	}
}

func normalizeZCodeRoots(roots []string) []string {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
	for _, root := range cleaned {
		normalized := normalizeZCodeRoot(root)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func normalizeZCodeRoot(root string) string {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return ""
	}
	if filepath.Base(root) == "db" {
		return root
	}
	return filepath.Join(root, "db")
}

func zcodeDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, zcodeDBName)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

func zcodeVirtualPathParts(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, zcodeDBName)
}

func ZCodeSQLiteVirtualPath(dbPath, sessionID string) string {
	return VirtualSourcePath(dbPath, sessionID)
}

func ZCodeSQLiteSourceMtime(path string) (int64, error) {
	dbPath, sessionID, ok := zcodeVirtualPathParts(path)
	if !ok {
		return 0, fmt.Errorf("not a zcode sqlite virtual path: %s", path)
	}
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	row, err := loadZCodeSessionRow(db, sessionID)
	if err != nil {
		return 0, err
	}
	return zcodeSessionFileMtime(dbPath, db, row), nil
}

func forEachZCodeSessionMeta(
	ctx context.Context, dbPath string, yield func(dbBackedSessionMeta) error,
) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT id,
		       project_id,
		       workspace_id,
		       COALESCE(directory, ''),
		       COALESCE(title, ''),
		       COALESCE(time_created, ''),
		       COALESCE(time_updated, '')
		  FROM session
		 ORDER BY COALESCE(time_updated, time_created), id
	`)
	if err != nil {
		return fmt.Errorf("listing zcode sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		row, err := scanZCodeSessionRow(rows)
		if err != nil {
			return err
		}
		if row.id == "" {
			continue
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(dbBackedSessionMeta{
			SessionID:   row.id,
			VirtualPath: ZCodeSQLiteVirtualPath(dbPath, row.id),
			FileMtime:   zcodeSessionFileMtime(dbPath, db, row),
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func zcodeSessionMeta(
	ctx context.Context, dbPath, sessionID string,
) (dbBackedSessionMeta, bool, error) {
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return dbBackedSessionMeta{}, false, err
	}
	defer db.Close()
	row, err := loadZCodeSessionRow(db, sessionID)
	if err == sql.ErrNoRows {
		return dbBackedSessionMeta{}, false, nil
	}
	if err != nil {
		return dbBackedSessionMeta{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return dbBackedSessionMeta{}, false, err
	}
	return dbBackedSessionMeta{
		SessionID: row.id, VirtualPath: ZCodeSQLiteVirtualPath(dbPath, row.id),
		FileMtime: zcodeSessionFileMtime(dbPath, db, row),
	}, true, nil
}

func parseZCodeSession(dbPath, sessionID, machine string) (*ParseResult, error) {
	db, err := openZCodeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	row, err := loadZCodeSessionRow(db, sessionID)
	if err != nil {
		return nil, err
	}
	result, err := buildZCodeParseResult(dbPath, machine, row, db)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func openZCodeDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=0&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening zcode db %s: %w", dbPath, err)
	}
	return db, nil
}

type zcodeSessionRow struct {
	id          string
	projectID   sql.NullString
	workspaceID sql.NullString
	directory   sql.NullString
	title       sql.NullString
	timeCreated sql.NullString
	timeUpdated sql.NullString
}

type zcodeUsageRow struct {
	sessionID                string
	turnID                   sql.NullString
	providerID               sql.NullString
	modelID                  sql.NullString
	status                   sql.NullString
	inputTokens              sql.NullInt64
	outputTokens             sql.NullInt64
	reasoningTokens          sql.NullInt64
	cacheCreationInputTokens sql.NullInt64
	cacheReadInputTokens     sql.NullInt64
	computedTotalTokens      sql.NullInt64
	startedAt                sql.NullString
	completedAt              sql.NullString
	durationMS               sql.NullInt64
	toolCallCount            sql.NullInt64
}

type zcodeMessageRow struct {
	id          string
	data        string
	timeCreated string
}

type zcodePartRow struct {
	id          string
	messageID   string
	timeCreated string
	data        string
}

func loadZCodeSessionRow(db *sql.DB, sessionID string) (zcodeSessionRow, error) {
	row := db.QueryRow(`
		SELECT id,
		       project_id,
		       workspace_id,
		       COALESCE(directory, ''),
		       COALESCE(title, ''),
		       COALESCE(time_created, ''),
		       COALESCE(time_updated, '')
		  FROM session
		 WHERE id = ?
	`, sessionID)

	var out zcodeSessionRow
	err := row.Scan(
		&out.id, &out.projectID, &out.workspaceID,
		&out.directory, &out.title, &out.timeCreated, &out.timeUpdated,
	)
	if err != nil {
		return zcodeSessionRow{}, fmt.Errorf("loading zcode session %s: %w", sessionID, err)
	}
	return out, nil
}

func scanZCodeSessionRow(rows *sql.Rows) (zcodeSessionRow, error) {
	var out zcodeSessionRow
	if err := rows.Scan(
		&out.id, &out.projectID, &out.workspaceID,
		&out.directory, &out.title, &out.timeCreated, &out.timeUpdated,
	); err != nil {
		return zcodeSessionRow{}, fmt.Errorf("scanning zcode session meta: %w", err)
	}
	return out, nil
}

func buildZCodeParseResult(
	dbPath, machine string,
	row zcodeSessionRow,
	db *sql.DB,
) (ParseResult, error) {
	startedAt := zcodeParseTime(row.timeCreated.String)
	endedAt := zcodeParseTime(row.timeUpdated.String)
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	directory := row.directory.String
	project := ExtractProjectFromCwd(directory)
	if project == "" {
		switch {
		case row.projectID.Valid:
			project = "project-" + strings.TrimSpace(row.projectID.String)
		case row.workspaceID.Valid:
			project = "workspace-" + strings.TrimSpace(row.workspaceID.String)
		default:
			project = "unknown"
		}
	}

	title := strings.TrimSpace(row.title.String)
	firstMessage := title
	if firstMessage != "" {
		firstMessage = truncate(strings.ReplaceAll(firstMessage, "\n", " "), 300)
	}

	msgs, err := loadZCodeMessages(db, row.id)
	if err != nil {
		return ParseResult{}, err
	}
	parts, err := loadZCodeParts(db, row.id)
	if err != nil {
		return ParseResult{}, err
	}
	parsedMessages := buildZCodeMessages(msgs, parts)
	userMessageCount := 0
	for _, msg := range parsedMessages {
		if msg.Role == RoleUser {
			userMessageCount++
		}
	}

	sess := ParsedSession{
		ID:               "zcode:" + row.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentZCode,
		Cwd:              directory,
		FirstMessage:     firstMessage,
		SessionName:      title,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsedMessages),
		UserMessageCount: userMessageCount,
		File: FileInfo{
			Path: ZCodeSQLiteVirtualPath(dbPath, row.id),
		},
	}
	if info, err := os.Stat(dbPath); err == nil {
		sess.File.Size = info.Size()
	}
	sess.File.Mtime = zcodeSessionFileMtime(dbPath, db, row)

	usageEvents, err := listZCodeUsageEvents(db, row.id, startedAt, endedAt)
	if err != nil {
		return ParseResult{}, err
	}
	applyUsageEventTokenTotals(&sess, usageEvents)

	return ParseResult{
		Session:     sess,
		Messages:    parsedMessages,
		UsageEvents: usageEvents,
	}, nil
}

func loadZCodeMessages(
	db *sql.DB, sessionID string,
) ([]zcodeMessageRow, error) {
	rows, err := db.Query(`
		SELECT id,
		       COALESCE(data, '{}'),
		       CAST(COALESCE(time_created, '') AS TEXT)
		  FROM message
		 WHERE session_id = ?
		 ORDER BY CAST(COALESCE(time_created, '') AS TEXT), id
	`, sessionID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table: message") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing zcode messages for %s: %w", sessionID, err)
	}
	defer rows.Close()

	msgs := make([]zcodeMessageRow, 0)
	for rows.Next() {
		var row zcodeMessageRow
		if err := rows.Scan(&row.id, &row.data, &row.timeCreated); err != nil {
			return nil, fmt.Errorf("scanning zcode message row: %w", err)
		}
		if row.id == "" {
			continue
		}
		msgs = append(msgs, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return msgs, nil
}

func loadZCodeParts(
	db *sql.DB, sessionID string,
) (map[string][]zcodePartRow, error) {
	selectTime := `''`
	orderBy := `id`
	if has, err := zcodeTableHasColumn(db, "part", "time_created"); err != nil {
		return nil, err
	} else if has {
		selectTime = `CAST(COALESCE(time_created, '') AS TEXT)`
		orderBy = selectTime + `, id`
	}

	rows, err := db.Query(`
		SELECT id,
		       message_id,
		       `+selectTime+`,
		       COALESCE(data, '{}')
		  FROM part
		 WHERE session_id = ?
		 ORDER BY `+orderBy, sessionID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table: part") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing zcode parts for %s: %w", sessionID, err)
	}
	defer rows.Close()

	parts := make(map[string][]zcodePartRow)
	for rows.Next() {
		var row zcodePartRow
		if err := rows.Scan(&row.id, &row.messageID, &row.timeCreated, &row.data); err != nil {
			return nil, fmt.Errorf("scanning zcode part row: %w", err)
		}
		if row.messageID == "" {
			continue
		}
		parts[row.messageID] = append(parts[row.messageID], row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return parts, nil
}

func buildZCodeMessages(
	msgs []zcodeMessageRow,
	parts map[string][]zcodePartRow,
) []ParsedMessage {
	if msgs == nil || parts == nil {
		return nil
	}

	parsed := make([]ParsedMessage, 0, len(msgs))
	for _, row := range msgs {
		msg, ok := buildZCodeMessage(len(parsed), row, parts[row.id])
		if !ok {
			continue
		}
		parsed = append(parsed, msg)
	}
	return parsed
}

func buildZCodeMessage(
	ordinal int,
	row zcodeMessageRow,
	parts []zcodePartRow,
) (ParsedMessage, bool) {
	role, ok := normalizeZCodeRole(gjson.Get(row.data, "role").Str)
	if !ok {
		return ParsedMessage{}, false
	}

	msg := ParsedMessage{
		Ordinal:   ordinal,
		Role:      role,
		Timestamp: zcodeParseTime(row.timeCreated),
		Model:     zcodeMessageModel(row.data),
		IsSystem:  role == RoleSystem,
	}

	var texts []string
	var thinking []string
	for _, part := range parts {
		block := gjson.Parse(part.data)
		switch block.Get("type").Str {
		case "text":
			if text := zcodeBlockText(block); text != "" {
				texts = append(texts, text)
			}
		case "thinking", "reasoning":
			text := zcodeThinkingText(block)
			if text == "" {
				continue
			}
			msg.HasThinking = true
			thinking = append(thinking, text)
			texts = append(texts, "[Thinking]\n"+text+"\n[/Thinking]")
		case "tool_use", "tool":
			msg.HasToolUse = true
			if tc, ok := zcodeParseToolCall(block); ok {
				msg.ToolCalls = append(msg.ToolCalls, tc)
			}
			if tr, ok := zcodeParseToolResult(block); ok {
				msg.ToolResults = append(msg.ToolResults, tr)
			}
		case "tool_result":
			if tr, ok := zcodeParseToolResult(block); ok {
				msg.ToolResults = append(msg.ToolResults, tr)
			}
		}
	}

	msg.Content = strings.Join(texts, "\n")
	msg.ThinkingText = strings.Join(thinking, "\n\n")
	msg.ContentLength = len(msg.Content)
	return msg, true
}

func normalizeZCodeRole(role string) (RoleType, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return RoleUser, true
	case "assistant", "model":
		return RoleAssistant, true
	case "system":
		return RoleSystem, true
	default:
		return "", false
	}
}

func zcodeMessageModel(raw string) string {
	for _, path := range []string{
		"modelID",
		"model_id",
		"model.modelID",
		"model.id",
		"model.name",
		"model",
	} {
		if model := strings.TrimSpace(gjson.Get(raw, path).Str); model != "" {
			return model
		}
	}
	return ""
}

func zcodeBlockText(block gjson.Result) string {
	for _, path := range []string{"text", "content", "value"} {
		value := block.Get(path)
		if !value.Exists() {
			continue
		}
		if text := decodeContent(value); text != "" {
			return text
		}
		if value.Type == gjson.String && value.Str != "" {
			return value.Str
		}
	}
	return ""
}

func zcodeThinkingText(block gjson.Result) string {
	for _, path := range []string{"thinking", "text", "content"} {
		value := block.Get(path)
		if !value.Exists() {
			continue
		}
		if text := decodeContent(value); text != "" {
			return text
		}
		if value.Type == gjson.String && value.Str != "" {
			return value.Str
		}
	}
	return ""
}

func zcodeParseToolCall(block gjson.Result) (ParsedToolCall, bool) {
	name := block.Get("name").Str
	if name == "" {
		name = block.Get("tool_name").Str
	}
	if name == "" {
		name = block.Get("toolName").Str
	}
	if name == "" {
		name = block.Get("tool").Str
	}
	if name == "" {
		return ParsedToolCall{}, false
	}

	input := toolCallInput(block)
	if input.Raw == "" || input.Raw == "{}" {
		if stateInput := block.Get("state.input"); stateInput.Exists() {
			input = stateInput
		}
	}
	inputJSON := input.Raw
	if inputJSON == "" {
		inputJSON = "{}"
	}
	toolUseID := block.Get("id").Str
	if toolUseID == "" {
		toolUseID = block.Get("tool_use_id").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("toolUseID").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("callID").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("callId").Str
	}

	call := ParsedToolCall{
		ToolUseID: toolUseID,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: inputJSON,
	}
	switch name {
	case "Skill", "skill":
		call.SkillName = input.Get("skill").Str
		if call.SkillName == "" {
			call.SkillName = input.Get("name").Str
		}
	default:
		call.SkillName = inferToolSkillName(name, inputJSON)
	}
	return call, true
}

func zcodeParseToolResult(block gjson.Result) (ParsedToolResult, bool) {
	toolUseID := block.Get("tool_use_id").Str
	if toolUseID == "" {
		toolUseID = block.Get("toolUseID").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("tool_call_id").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("callID").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("callId").Str
	}
	if toolUseID == "" {
		return ParsedToolResult{}, false
	}

	content := block.Get("content")
	if !content.Exists() || content.Type == gjson.Null {
		content = block.Get("result")
	}
	if !content.Exists() || content.Type == gjson.Null {
		content = block.Get("output")
	}
	if !content.Exists() || content.Type == gjson.Null {
		content = block.Get("state.output")
	}
	if !content.Exists() || content.Type == gjson.Null {
		content = block.Get("state.error")
	}
	if content.Exists() && content.Type != gjson.Null {
		return ParsedToolResult{
			ToolUseID:     toolUseID,
			ContentLength: toolResultContentLength(content),
			ContentRaw:    content.Raw,
		}, true
	}

	for _, key := range []string{"text", "error", "value"} {
		if text := block.Get(key).Str; text != "" {
			return ParsedToolResult{
				ToolUseID:     toolUseID,
				ContentLength: len(text),
				ContentRaw:    strconv.Quote(text),
			}, true
		}
	}
	return ParsedToolResult{}, false
}

func zcodeTableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("listing zcode table info for %s: %w", table, err)
	}
	defer rows.Close()

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
			return false, fmt.Errorf("scanning zcode table info for %s: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func listZCodeUsageEvents(
	db *sql.DB,
	sessionID string,
	startedAt, endedAt time.Time,
) ([]ParsedUsageEvent, error) {
	rows, err := db.Query(`
		SELECT session_id,
		       CAST(turn_id AS TEXT),
		       provider_id,
		       COALESCE(model_id, ''),
		       COALESCE(status, ''),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(reasoning_tokens, 0),
		       COALESCE(cache_creation_input_tokens, 0),
		       COALESCE(cache_read_input_tokens, 0),
		       COALESCE(computed_total_tokens, 0),
		       COALESCE(started_at, ''),
		       COALESCE(completed_at, ''),
		       COALESCE(duration_ms, 0),
		       COALESCE(tool_call_count, 0)
		  FROM model_usage
		 WHERE session_id = ?
		 ORDER BY
		       COALESCE(turn_id, -1),
		       COALESCE(started_at, ''),
		       COALESCE(completed_at, ''),
		       COALESCE(provider_id, -1),
		       COALESCE(model_id, ''),
		       COALESCE(status, ''),
		       COALESCE(input_tokens, 0),
		       COALESCE(output_tokens, 0),
		       COALESCE(reasoning_tokens, 0),
		       COALESCE(cache_creation_input_tokens, 0),
		       COALESCE(cache_read_input_tokens, 0),
		       COALESCE(computed_total_tokens, 0),
		       COALESCE(duration_ms, 0),
		       COALESCE(tool_call_count, 0)
	`, sessionID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: model_usage") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing zcode usage rows for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var events []ParsedUsageEvent
	for rows.Next() {
		row, err := scanZCodeUsageRow(rows)
		if err != nil {
			return nil, err
		}
		if row.sessionID == "" {
			continue
		}
		fullSessionID := "zcode:" + row.sessionID
		ev := ParsedUsageEvent{
			SessionID:                fullSessionID,
			Source:                   "session",
			Model:                    row.modelID.String,
			InputTokens:              int(row.inputTokens.Int64),
			OutputTokens:             int(row.outputTokens.Int64),
			CacheCreationInputTokens: int(row.cacheCreationInputTokens.Int64),
			CacheReadInputTokens:     int(row.cacheReadInputTokens.Int64),
			ReasoningTokens:          int(row.reasoningTokens.Int64),
			OccurredAt:               timeString(zcodeUsageTime(row.completedAt.String), zcodeUsageTime(row.startedAt.String)),
			DedupKey:                 zcodeUsageDedupKey(fullSessionID, row),
		}
		if row.turnID.Valid {
			if parsed, err := strconv.Atoi(strings.TrimSpace(row.turnID.String)); err == nil {
				ev.MessageOrdinal = &parsed
			}
		}
		if ev.OccurredAt == "" {
			ev.OccurredAt = timeString(endedAt, startedAt)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func scanZCodeUsageRow(rows *sql.Rows) (zcodeUsageRow, error) {
	var out zcodeUsageRow
	if err := rows.Scan(
		&out.sessionID, &out.turnID, &out.providerID,
		&out.modelID, &out.status, &out.inputTokens, &out.outputTokens,
		&out.reasoningTokens, &out.cacheCreationInputTokens,
		&out.cacheReadInputTokens, &out.computedTotalTokens,
		&out.startedAt, &out.completedAt, &out.durationMS,
		&out.toolCallCount,
	); err != nil {
		return zcodeUsageRow{}, fmt.Errorf("scanning zcode usage row: %w", err)
	}
	return out, nil
}

func zcodeUsageDedupKey(fullSessionID string, row zcodeUsageRow) string {
	parts := []string{
		"session:" + fullSessionID,
		"turn=" + row.turnID.String,
		"provider=" + row.providerID.String,
		"model=" + row.modelID.String,
		"status=" + row.status.String,
		"started_at=" + row.startedAt.String,
		"completed_at=" + row.completedAt.String,
		"duration_ms=" + zcodeNullInt64String(row.durationMS),
		"tool_call_count=" + zcodeNullInt64String(row.toolCallCount),
		"input_tokens=" + zcodeNullInt64String(row.inputTokens),
		"output_tokens=" + zcodeNullInt64String(row.outputTokens),
		"reasoning_tokens=" + zcodeNullInt64String(row.reasoningTokens),
		"cache_creation_input_tokens=" + zcodeNullInt64String(row.cacheCreationInputTokens),
		"cache_read_input_tokens=" + zcodeNullInt64String(row.cacheReadInputTokens),
		"computed_total_tokens=" + zcodeNullInt64String(row.computedTotalTokens),
	}
	return strings.Join(parts, "|")
}

func zcodeNullInt64String(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func zcodeParseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts := parseTimestamp(raw); !ts.IsZero() {
		return ts
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case n >= 1_000_000_000_000_000_000:
			return time.Unix(0, n).UTC()
		case n >= 1_000_000_000_000:
			return time.UnixMilli(n).UTC()
		case n >= 1_000_000_000:
			return time.Unix(n, 0).UTC()
		default:
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

func zcodeUsageTime(raw string) time.Time {
	return zcodeParseTime(raw)
}

func zcodeSessionFileMtime(dbPath string, db *sql.DB, row zcodeSessionRow) int64 {
	maxMtime := zcodeTimeUnixNano(zcodeRowUpdatedAt(row))
	if usageMtime, err := zcodeMaxUsageMtime(db, row.id); err == nil {
		maxMtime = max(maxMtime, usageMtime)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if info, err := os.Stat(path); err == nil {
			maxMtime = max(maxMtime, info.ModTime().UnixNano())
		}
	}
	return maxMtime
}

func zcodeMaxUsageMtime(db *sql.DB, sessionID string) (int64, error) {
	rows, err := db.Query(`
		SELECT CAST(COALESCE(completed_at, '') AS TEXT),
		       CAST(COALESCE(started_at, '') AS TEXT)
		  FROM model_usage
		 WHERE session_id = ?
	`, sessionID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table: model_usage") {
			return 0, nil
		}
		return 0, fmt.Errorf("listing zcode usage mtimes: %w", err)
	}
	defer rows.Close()

	var maxMtime int64
	for rows.Next() {
		var completedAt, startedAt string
		if err := rows.Scan(&completedAt, &startedAt); err != nil {
			return 0, fmt.Errorf("scanning zcode usage mtime: %w", err)
		}
		maxMtime = max(maxMtime, zcodeTimeUnixNano(zcodeUsageTime(completedAt)))
		maxMtime = max(maxMtime, zcodeTimeUnixNano(zcodeUsageTime(startedAt)))
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return maxMtime, nil
}

func zcodeTimeUnixNano(ts time.Time) int64 {
	if ts.IsZero() {
		return 0
	}
	return ts.UnixNano()
}

func zcodeRowUpdatedAt(row zcodeSessionRow) time.Time {
	if ts := zcodeParseTime(row.timeUpdated.String); !ts.IsZero() {
		return ts
	}
	if ts := zcodeParseTime(row.timeCreated.String); !ts.IsZero() {
		return ts
	}
	return time.Time{}
}
