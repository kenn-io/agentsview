package parser

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
)

// CrushDBName is the SQLite store filename inside each project's
// <project>/.crush data directory.
const CrushDBName = "crush.db"

// CrushProjectsFileName is the registry file inside Crush's data directory
// that maps project paths to their per-project data directories.
const CrushProjectsFileName = "projects.json"

// crushSourceVersion identifies the Crush SQLite session format parsed here.
const crushSourceVersion = "crush-sqlite-v1"

type crushSessionRow struct {
	id               string
	title            string
	parentSessionID  string
	messageCount     int64
	promptTokens     int64
	completionTokens int64
	cost             float64
	createdAt        int64
	updatedAt        int64
	maxMessageAt     sql.NullInt64
}

// crushProjectsFile holds the structure of
// ~/.local/share/crush/projects.json.
type crushProjectsFile struct {
	Projects []struct {
		Path    string `json:"path"`
		DataDir string `json:"data_dir"`
	} `json:"projects"`
}

// crushProjectsDataDirs reads a Crush projects.json registry and returns
// the per-project data directories it lists.
func crushProjectsDataDirs(registryPath string) []string {
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return nil
	}
	var pf crushProjectsFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil
	}
	dirs := make([]string, 0, len(pf.Projects))
	for _, project := range pf.Projects {
		dir := strings.TrimSpace(project.DataDir)
		if dir == "" {
			continue
		}
		dirs = append(dirs, filepath.Clean(dir))
	}
	return dirs
}

// crushProjectDirsMapping reads a Crush projects.json registry and returns
// a mapping from each data directory to its project path. The project path
// is used for project attribution when the data directory does not follow
// the default <project>/.crush layout.
func crushProjectDirsMapping(registryPath string) map[string]string {
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return nil
	}
	var pf crushProjectsFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil
	}
	mapping := make(map[string]string, len(pf.Projects))
	for _, project := range pf.Projects {
		dir := strings.TrimSpace(project.DataDir)
		if dir == "" {
			continue
		}
		path := strings.TrimSpace(project.Path)
		if path == "" {
			continue
		}
		mapping[filepath.Clean(dir)] = filepath.Clean(path)
	}
	return mapping
}

func openCrushDB(dbPath string, stableSnapshot bool) (*sql.DB, error) {
	immutable := "0"
	if stableSnapshot {
		immutable = "1"
	}
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=" + immutable + "&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening crush sessions database %s: %w", dbPath, err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		if stableSnapshot {
			// The snapshot DSN itself failed; a fallback would break the
			// stable-snapshot guarantee, so surface the failure.
			return nil, fmt.Errorf("opening crush sessions database %s: %w (WAL corruption is a possible cause; Crush must repair the store)", dbPath, err)
		}
		fallback := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=1&_busy_timeout=3000"
		if fb, err2 := sql.Open("sqlite3", fallback); err2 == nil {
			if err3 := fb.PingContext(context.Background()); err3 == nil {
				return fb, nil
			}
			_ = fb.Close()
		}
		return nil, fmt.Errorf("opening crush sessions database %s: %w (WAL corruption is a possible cause; Crush must repair the store)", dbPath, err)
	}
	return db, nil
}

func crushTableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?
	`, table).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking crush table %s: %w", table, err)
	}
	return count > 0, nil
}

func crushTableColumns(
	ctx context.Context, db *sql.DB, table string,
) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, fmt.Errorf("listing crush %s columns: %w", table, err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid      int
			name     string
			typeName string
			notNull  int
			defaultV sql.NullString
			pk       int
		)
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultV, &pk); err != nil {
			return nil, fmt.Errorf("scanning crush %s columns: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

// validateCrushSchema requires the Crush shape: sessions with title
// columns and messages with a parts JSON column. The parts column is the
// agent-specific marker — Crush vendors goose's migration tool, so a
// goose_db_version table proves nothing about the format.
func validateCrushSchema(ctx context.Context, db *sql.DB) error {
	hasSessions, err := crushTableExists(ctx, db, "sessions")
	if err != nil {
		return err
	}
	hasMessages, err := crushTableExists(ctx, db, "messages")
	if err != nil {
		return err
	}
	if !hasSessions || !hasMessages {
		return fmt.Errorf("unsupported crush schema: missing sessions or messages table")
	}
	sessionColumns, err := crushTableColumns(ctx, db, "sessions")
	if err != nil {
		return err
	}
	for _, required := range []string{"id", "title", "created_at", "updated_at"} {
		if !sessionColumns[required] {
			return fmt.Errorf("unsupported crush sessions schema: missing sessions.%s", required)
		}
	}
	messageColumns, err := crushTableColumns(ctx, db, "messages")
	if err != nil {
		return err
	}
	for _, required := range []string{"id", "session_id", "role", "parts", "created_at"} {
		if !messageColumns[required] {
			return fmt.Errorf("unsupported crush messages schema: missing messages.%s", required)
		}
	}
	return nil
}

// crushSessionSelect joins per-session message activity so discovery and
// fingerprints can stat one snapshot. created_at values are Unix seconds
// despite schema comments claiming milliseconds.
const crushSessionSelect = `
	SELECT id,
	       COALESCE(title, ''),
	       COALESCE(parent_session_id, ''),
	       COALESCE(message_count, 0),
	       COALESCE(prompt_tokens, 0),
	       COALESCE(completion_tokens, 0),
	       COALESCE(cost, 0),
	       COALESCE(created_at, 0),
	       COALESCE(updated_at, 0),
	       (SELECT MAX(m.created_at) FROM messages m WHERE m.session_id = sessions.id)
	  FROM sessions
`

func scanCrushSessionRow(scanner interface{ Scan(...any) error }) (crushSessionRow, error) {
	var row crushSessionRow
	err := scanner.Scan(
		&row.id, &row.title, &row.parentSessionID, &row.messageCount,
		&row.promptTokens, &row.completionTokens, &row.cost,
		&row.createdAt, &row.updatedAt, &row.maxMessageAt,
	)
	if err != nil {
		return crushSessionRow{}, err
	}
	return row, nil
}

func forEachCrushSessionMeta(
	ctx context.Context, dbPath string, stableSnapshot bool,
	yield func(dbBackedSessionMeta) error,
) error {
	if !IsRegularFile(dbPath) {
		return nil
	}
	db, err := openCrushDB(dbPath, stableSnapshot)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := validateCrushSchema(ctx, db); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, crushSessionSelect+" ORDER BY id")
	if err != nil {
		return fmt.Errorf("listing crush sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanCrushSessionRow(rows)
		if err != nil {
			return fmt.Errorf("scanning crush session metadata: %w", err)
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(dbBackedSessionMeta{
			SessionID:   row.id,
			VirtualPath: VirtualSourcePath(dbPath, row.id),
			FileMtime:   crushSessionMtime(dbPath, row),
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func crushSessionMeta(
	ctx context.Context, dbPath, sessionID string, stableSnapshot bool,
) (dbBackedSessionMeta, bool, error) {
	if !IsRegularFile(dbPath) {
		return dbBackedSessionMeta{}, false, nil
	}
	db, err := openCrushDB(dbPath, stableSnapshot)
	if err != nil {
		return dbBackedSessionMeta{}, false, err
	}
	defer db.Close()
	if err := validateCrushSchema(ctx, db); err != nil {
		return dbBackedSessionMeta{}, false, err
	}
	row, err := scanCrushSessionRow(db.QueryRowContext(
		ctx, crushSessionSelect+" WHERE sessions.id = ?", sessionID,
	))
	if err == sql.ErrNoRows {
		return dbBackedSessionMeta{}, false, nil
	}
	if err != nil {
		return dbBackedSessionMeta{}, false, fmt.Errorf(
			"loading crush session %s: %w", sessionID, err,
		)
	}
	return dbBackedSessionMeta{
		SessionID:   row.id,
		VirtualPath: VirtualSourcePath(dbPath, row.id),
		FileMtime:   crushSessionMtime(dbPath, row),
	}, true, nil
}

func crushSessionMtime(dbPath string, row crushSessionRow) int64 {
	maxTime := maxCrushTime(
		crushUnixTimestamp(row.updatedAt),
		crushUnixTimestamp(row.createdAt),
		crushUnixTimestamp(row.maxMessageAt.Int64),
	)
	if !maxTime.IsZero() {
		return maxTime.UnixNano()
	}
	mtime, _ := sqliteDBCompositeMtime(dbPath, []string{"", "-wal"})
	return mtime
}

func parseCrushSession(
	ctx context.Context, dbPath, sessionID, machine string,
	stableSnapshot bool, projectMapping map[string]string,
) (*ParsedSession, []ParsedMessage, error) {
	db, err := openCrushDB(dbPath, stableSnapshot)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if err := validateCrushSchema(ctx, db); err != nil {
		return nil, nil, err
	}
	row, err := scanCrushSessionRow(db.QueryRowContext(
		ctx, crushSessionSelect+" WHERE sessions.id = ?", sessionID,
	))
	if err == sql.ErrNoRows {
		return nil, nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, nil, fmt.Errorf("loading crush session %s: %w", sessionID, err)
	}
	messages, err := loadCrushMessages(ctx, db, sessionID)
	if err != nil {
		return nil, nil, err
	}
	links, err := crushSubagentLinks(ctx, db, sessionID)
	if err != nil {
		return nil, nil, err
	}
	for i := range messages {
		for j := range messages[i].ToolCalls {
			if child, ok := links[messages[i].ToolCalls[j].ToolUseID]; ok {
				messages[i].ToolCalls[j].SubagentSessionID = child
			}
		}
	}

	// Resolve the project directory: first check the registry mapping for
	// an explicit project path, then fall back to the two-levels-above
	// heuristic for the default <project>/.crush layout.
	dataDir := filepath.Dir(dbPath)
	projectDir := ""
	if projectMapping != nil {
		if path, ok := projectMapping[dataDir]; ok {
			projectDir = path
		}
	}
	if projectDir == "" {
		projectDir = filepath.Clean(filepath.Dir(dataDir))
	}
	project := ExtractProjectFromCwdWithBranchContext(ctx, projectDir, "")
	if project == "" {
		project = "crush"
	}

	sessionName := strings.TrimSpace(row.title)
	firstMessage := crushFirstMessage(messages)
	if firstMessage == "" && sessionName != "" {
		firstMessage = truncate(strings.ReplaceAll(sessionName, "\n", " "), 300)
	}

	userMessages := 0
	for _, message := range messages {
		if message.Role == RoleUser && len(message.ToolResults) == 0 {
			userMessages++
		}
	}

	startedAt := crushUnixTimestamp(row.createdAt)
	endedAt := crushUnixTimestamp(row.updatedAt)
	for _, message := range messages {
		if message.Timestamp.After(endedAt) {
			endedAt = message.Timestamp
		}
		if startedAt.IsZero() ||
			(!message.Timestamp.IsZero() && message.Timestamp.Before(startedAt)) {
			startedAt = message.Timestamp
		}
	}
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	session := &ParsedSession{
		ID:                  "crush:" + row.id,
		Project:             project,
		Machine:             machine,
		Agent:               AgentCrush,
		Cwd:                 projectDir,
		SourceSessionID:     row.id,
		SourceVersion:       crushSourceVersion,
		FirstMessage:        firstMessage,
		SessionName:         sessionName,
		StartedAt:           startedAt,
		EndedAt:             endedAt,
		MessageCount:        len(messages),
		UserMessageCount:    userMessages,
		CountsAuthoritative: true,
		File: FileInfo{
			Path:  VirtualSourcePath(dbPath, row.id),
			Mtime: crushSessionMtime(dbPath, row),
		},
	}
	if info, err := os.Stat(dbPath); err == nil {
		session.File.Size = info.Size()
	}
	if parentID := strings.TrimSpace(row.parentSessionID); parentID != "" {
		session.ParentSessionID = "crush:" + parentID
		session.RelationshipType = RelSubagent
	}
	usageEvents := crushUsageEvents(session, row, messages)
	applyUsageEventTokenTotals(session, usageEvents)
	session.UsageEvents = usageEvents
	return session, messages, nil
}

// crushSubagentLinks maps a session's prefixed tool-call IDs to the
// crush-prefixed child session they spawned. Crush names subagent sessions
// "<own-uuid>$$<spawning-tool-call-id>" under the delegating session.
func crushSubagentLinks(
	ctx context.Context, db *sql.DB, sessionID string,
) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM sessions WHERE parent_session_id = ?
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing crush child sessions for %s: %w", sessionID, err)
	}
	defer rows.Close()
	links := make(map[string]string)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning crush child session: %w", err)
		}
		if idx := strings.LastIndex(id, "$$"); idx >= 0 && idx+2 < len(id) {
			links["crush:"+id[idx+2:]] = "crush:" + id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}

func loadCrushMessages(
	ctx context.Context, db *sql.DB, sessionID string,
) ([]ParsedMessage, error) {
	columns, err := crushTableColumns(ctx, db, "messages")
	if err != nil {
		return nil, fmt.Errorf("inspecting crush messages columns: %w", err)
	}
	selectCols := []string{
		"id",
		"COALESCE(role, '')",
		"COALESCE(parts, '[]')",
		"COALESCE(model, '')",
		"COALESCE(created_at, 0)",
	}
	if columns["provider"] {
		selectCols = append(selectCols, "COALESCE(provider, '')")
	}
	if columns["is_summary_message"] {
		selectCols = append(selectCols, "COALESCE(is_summary_message, 0)")
	}
	query := "SELECT " + strings.Join(selectCols, ", ") + `
		FROM messages
	   WHERE session_id = ?
	   ORDER BY created_at, rowid`
	rows, err := db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing crush messages for %s: %w", sessionID, err)
	}
	defer rows.Close()
	parsed := make([]ParsedMessage, 0)
	for rows.Next() {
		var (
			rowID     string
			role      string
			parts     string
			model     string
			createdAt int64
			provider  string
			isSummary int64
		)
		scanArgs := []any{&rowID, &role, &parts, &model, &createdAt}
		if columns["provider"] {
			scanArgs = append(scanArgs, &provider)
		}
		if columns["is_summary_message"] {
			scanArgs = append(scanArgs, &isSummary)
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("scanning crush message row: %w", err)
		}
		message, ok, err := buildCrushMessage(
			ctx, len(parsed), rowID, role, parts, model, provider, createdAt,
			isSummary != 0,
		)
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

func buildCrushMessage(
	ctx context.Context, ordinal int, rowID, role, parts, model, provider string,
	createdAt int64, isSummary bool,
) (ParsedMessage, bool, error) {
	contentJSON := gjson.Parse(parts)
	if !gjson.Valid(parts) || !contentJSON.IsArray() {
		return ParsedMessage{}, false, fmt.Errorf(
			"parsing crush message %s parts: expected JSON array", rowID,
		)
	}
	message := ParsedMessage{
		Ordinal:    ordinal,
		Timestamp:  crushUnixTimestamp(createdAt),
		SourceUUID: rowID,
	}
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		message.Role = RoleUser
	case "assistant":
		message.Role = RoleAssistant
		message.Model = strings.TrimSpace(model)
		message.ProviderID = strings.TrimSpace(provider)
	case "tool":
		message.Role = RoleUser
		message.IsSystem = true
	default:
		return ParsedMessage{}, false, nil
	}
	if isSummary {
		// Crush marks condensed conversation summaries with
		// is_summary_message=1 (backed by sessions.summary_message_id);
		// they are context-management boundaries, not conversation
		// content of the row's original role.
		message.Role = RoleSystem
		message.IsSystem = true
		message.IsCompactBoundary = true
		message.Model = ""
		message.ProviderID = ""
	}

	var texts []string
	var thinking []string
	contentJSON.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").Str {
		case "text":
			if text := strings.TrimSpace(part.Get("data.text").Str); text != "" {
				texts = append(texts, text)
			}
		case "reasoning":
			message.HasThinking = true
			if text := strings.TrimSpace(part.Get("data.thinking").Str); text != "" {
				thinking = append(thinking, text)
				texts = append(texts, "[Thinking]\n"+text+"\n[/Thinking]")
			}
		case "tool_call":
			if call, ok := crushParseToolCall(ctx, rowID, part); ok {
				message.HasToolUse = true
				message.ToolCalls = append(message.ToolCalls, call)
			}
		case "tool_result":
			if result, ok := crushParseToolResult(part); ok {
				message.ToolResults = append(message.ToolResults, result)
			}
		case "finish":
			if message.Role == RoleAssistant {
				if reason := strings.TrimSpace(part.Get("data.reason").Str); reason != "" {
					message.StopReason = reason
				}
			}
		default:
			// Unknown part types are skipped; Crush adds part types in
			// minor releases and unknown data must not fail the session.
		}
		return true
	})
	message.Content = strings.Join(texts, "\n")
	message.ThinkingText = strings.Join(thinking, "\n\n")
	message.ContentLength = len(message.Content)
	return message, true, nil
}

func crushParseToolCall(
	ctx context.Context, rowID string, part gjson.Result,
) (ParsedToolCall, bool) {
	data := part.Get("data")
	name := strings.TrimSpace(data.Get("name").Str)
	if name == "" {
		return ParsedToolCall{}, false
	}
	toolUseID := strings.TrimSpace(data.Get("id").Str)
	if toolUseID == "" {
		// Payload call IDs are unique per request; fall back to the row ID
		// plus part index so repeated calls never share a ToolUseID.
		toolUseID = rowID + ":" + strconv.Itoa(part.Index)
	}
	inputJSON := data.Get("input").Str
	if !gjson.Valid(inputJSON) {
		inputJSON = "{}"
	}
	call := ParsedToolCall{
		ToolUseID: "crush:" + toolUseID,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: inputJSON,
		SkillName: inferToolSkillName(ctx, name, inputJSON),
	}
	return call, true
}

func crushParseToolResult(part gjson.Result) (ParsedToolResult, bool) {
	data := part.Get("data")
	toolUseID := strings.TrimSpace(data.Get("tool_call_id").Str)
	if toolUseID == "" {
		return ParsedToolResult{}, false
	}
	content := data.Get("content")
	if !content.Exists() || content.Type == gjson.Null {
		return ParsedToolResult{ToolUseID: "crush:" + toolUseID, ContentRaw: "null"}, true
	}
	var quoted []byte
	if content.Type == gjson.String {
		quoted, _ = json.Marshal(content.Str)
	} else {
		quoted = []byte(content.Raw)
	}
	return ParsedToolResult{
		ToolUseID:     "crush:" + toolUseID,
		ContentLength: len(content.Str),
		ContentRaw:    string(quoted),
	}, true
}

func crushFirstMessage(messages []ParsedMessage) string {
	for _, message := range messages {
		if message.Role != RoleUser || message.IsSystem {
			continue
		}
		text := strings.TrimSpace(message.Content)
		if text == "" {
			continue
		}
		return truncate(strings.ReplaceAll(text, "\n", " "), 300)
	}
	return ""
}

// crushUsageEvents emits one aggregate usage event per session. Crush
// tracks cumulative session totals (prompt_tokens, completion_tokens, cost)
// with no per-request breakdown; the model is the most recent assistant
// message's model.
func crushUsageEvents(
	session *ParsedSession, row crushSessionRow, messages []ParsedMessage,
) []ParsedUsageEvent {
	promptTokens := nonnegativeCrushToken(row.promptTokens)
	completionTokens := nonnegativeCrushToken(row.completionTokens)
	cost := row.cost
	if promptTokens <= 0 && completionTokens <= 0 && cost <= 0 {
		return nil
	}
	event := ParsedUsageEvent{
		SessionID:    "crush:" + row.id,
		Source:       "session",
		Model:        crushLatestModel(messages),
		ProviderID:   crushLatestProvider(messages),
		InputTokens:  promptTokens,
		OutputTokens: completionTokens,
		OccurredAt:   timeString(session.EndedAt, session.StartedAt),
		DedupKey:     "session:crush:" + row.id + "|aggregate",
	}
	if cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) {
		if parsed, err := money.FromFloatDollars(cost); err == nil {
			event.Cost = &parsed
			event.CostStatus = "unknown"
			event.CostSource = "crush-session"
		}
	}
	return []ParsedUsageEvent{event}
}

func crushLatestModel(messages []ParsedMessage) string {
	model := ""
	for _, message := range messages {
		if message.Role == RoleAssistant && message.Model != "" {
			model = message.Model
		}
	}
	return model
}

func crushLatestProvider(messages []ParsedMessage) string {
	provider := ""
	for _, message := range messages {
		if message.Role == RoleAssistant && message.ProviderID != "" {
			provider = message.ProviderID
		}
	}
	return provider
}

func nonnegativeCrushToken(value int64) int {
	if value <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

// crushUnixTimestamp decodes Crush timestamps. The schema comments claim
// milliseconds, but Crush writes Unix seconds (its update triggers use
// strftime('%s','now')); values beyond the millisecond threshold are
// decoded as milliseconds for forward compatibility.
func crushUnixTimestamp(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value >= 10_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func maxCrushTime(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if value.After(result) {
			result = value
		}
	}
	return result
}
