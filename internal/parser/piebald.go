package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const piebaldDBFilename = "app.db"

// PiebaldSession bundles a parsed Piebald chat with its messages.
type PiebaldSession struct {
	Session  ParsedSession
	Messages []ParsedMessage
}

// PiebaldSessionMeta is lightweight metadata for a Piebald chat.
type PiebaldSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
}

// FindPiebaldDBPath returns the Piebald SQLite database path when present.
func FindPiebaldDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, piebaldDBFilename)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

// ListPiebaldSessionMeta returns lightweight metadata for all non-empty chats.
func ListPiebaldSessionMeta(dbPath string) ([]PiebaldSessionMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openPiebaldDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id,
		       COALESCE(updated_at, created_at)
		FROM chats
		WHERE COALESCE(is_deleted, 0) = 0
		  AND message_count > 0
	`)
	if err != nil {
		return nil, fmt.Errorf("listing piebald chats: %w", err)
	}
	defer rows.Close()

	var metas []PiebaldSessionMeta
	for rows.Next() {
		var id int64
		var updatedAt string
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning piebald chat meta: %w", err)
		}
		metas = append(metas, PiebaldSessionMeta{
			SessionID:   fmt.Sprintf("%d", id),
			VirtualPath: fmt.Sprintf("%s#%d", dbPath, id),
			FileMtime:   parsePiebaldTimestamp(updatedAt).UnixNano(),
		})
	}
	return metas, rows.Err()
}

// ParsePiebaldDB opens the Piebald SQLite database read-only and returns chats.
func ParsePiebaldDB(dbPath, machine string) ([]PiebaldSession, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openPiebaldDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	chats, err := loadPiebaldChats(db)
	if err != nil {
		return nil, fmt.Errorf("loading piebald chats: %w", err)
	}

	var results []PiebaldSession
	for _, c := range chats {
		parsedResults, err := buildPiebaldSessionResults(db, c, dbPath, machine)
		if err != nil {
			log.Printf("piebald chat %d: %v", c.id, err)
			continue
		}
		for _, parsed := range parsedResults {
			results = append(results, PiebaldSession(parsed))
		}
	}
	return results, nil
}

// ParsePiebaldSession parses a single Piebald chat by ID from app.db.
func ParsePiebaldSession(dbPath, chatID, machine string) (*ParsedSession, []ParsedMessage, error) {
	results, err := ParsePiebaldSessionResults(dbPath, chatID, machine)
	if err != nil || len(results) == 0 {
		return nil, nil, err
	}
	return &results[0].Session, results[0].Messages, nil
}

// ParsePiebaldSessionResults parses a single Piebald chat and any large
// message-DAG branches as fork child sessions.
func ParsePiebaldSessionResults(dbPath, chatID, machine string) ([]ParseResult, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("piebald db not found: %s", dbPath)
	}

	db, err := openPiebaldDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	c, err := loadOnePiebaldChat(db, chatID)
	if err != nil {
		return nil, fmt.Errorf("loading piebald chat %s: %w", chatID, err)
	}
	return buildPiebaldSessionResults(db, c, dbPath, machine)
}

func openPiebaldDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?mode=ro&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening piebald db %s: %w", dbPath, err)
	}
	return db, nil
}

type piebaldChatRow struct {
	id               int64
	title            string
	createdAt        string
	updatedAt        string
	messageCount     int
	currentDirectory string
	worktreePath     string
	branchName       string
	projectDirectory string
	projectName      string
}

func loadPiebaldChats(db *sql.DB) ([]piebaldChatRow, error) {
	rows, err := db.Query(piebaldChatSelect(`
		WHERE COALESCE(c.is_deleted, 0) = 0
		  AND c.message_count > 0
		ORDER BY COALESCE(c.updated_at, c.created_at)
	`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []piebaldChatRow
	for rows.Next() {
		c, err := scanPiebaldChat(rows)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

func loadOnePiebaldChat(db *sql.DB, chatID string) (piebaldChatRow, error) {
	row := db.QueryRow(piebaldChatSelect(`
		WHERE c.id = ?
		  AND COALESCE(c.is_deleted, 0) = 0
	`), chatID)
	return scanPiebaldChat(row)
}

func piebaldChatSelect(where string) string {
	return `
		SELECT c.id,
		       COALESCE(c.title, ''),
		       c.created_at,
		       COALESCE(c.updated_at, c.created_at),
		       c.message_count,
		       COALESCE(c.current_directory, ''),
		       COALESCE(c.worktree_path, ''),
		       COALESCE(c.branch_name, ''),
		       COALESCE(p.directory, ''),
		       COALESCE(p.name, '')
		FROM chats c
		LEFT JOIN projects p ON p.id = c.project_id
	` + where
}

type piebaldChatScanner interface {
	Scan(dest ...any) error
}

func scanPiebaldChat(scanner piebaldChatScanner) (piebaldChatRow, error) {
	var c piebaldChatRow
	err := scanner.Scan(
		&c.id, &c.title, &c.createdAt, &c.updatedAt,
		&c.messageCount, &c.currentDirectory, &c.worktreePath,
		&c.branchName, &c.projectDirectory, &c.projectName,
	)
	return c, err
}

type piebaldMessageRow struct {
	id               int64
	parentChatID     int64
	parentMessageID  sql.NullInt64
	isEnabled        int
	role             string
	model            string
	createdAt        string
	updatedAt        string
	inputTokens      sql.NullInt64
	outputTokens     sql.NullInt64
	reasoningTokens  sql.NullInt64
	cacheReadTokens  sql.NullInt64
	cacheWriteTokens sql.NullInt64
	status           string
	finishReason     string
	errorText        string
}

type piebaldPartRow struct {
	id        int64
	partType  string
	partIndex int
}

type piebaldToolCallRow struct {
	providerToolUseID string
	toolName          string
	toolInput         string
	toolResult        sql.NullString
	toolError         sql.NullString
	toolState         string
	subAgentChatID    sql.NullInt64
}

func buildPiebaldSessionResults(db *sql.DB, c piebaldChatRow, dbPath, machine string) ([]ParseResult, error) {
	messageRows, err := loadPiebaldMessages(db, c.id)
	if err != nil {
		return nil, err
	}
	if len(messageRows) == 0 {
		return nil, nil
	}

	branches := splitPiebaldBranches(messageRows)
	if len(branches) == 0 {
		return nil, nil
	}

	var results []ParseResult
	baseID := fmt.Sprintf("piebald:%d", c.id)
	for i, b := range branches {
		messages, firstMsg, realUserCount, err := buildPiebaldMessages(db, b.rows)
		if err != nil {
			return nil, err
		}
		if len(messages) == 0 {
			continue
		}

		startedAt := parsePiebaldTimestamp(c.createdAt)
		if startedAt.IsZero() || i > 0 {
			startedAt = messages[0].Timestamp
		}
		endedAt := parsePiebaldTimestamp(c.updatedAt)
		if endedAt.IsZero() || i > 0 {
			endedAt = messages[len(messages)-1].Timestamp
		}

		sess := buildPiebaldSessionMeta(c, dbPath, machine)
		sess.ID = baseID
		if i > 0 {
			sess.ID = fmt.Sprintf("%s-%d", baseID, b.firstRowID)
			sess.ParentSessionID = b.parentID
			if sess.ParentSessionID == "" {
				sess.ParentSessionID = baseID
			}
			sess.RelationshipType = RelFork
		}
		sess.FirstMessage = firstMsg
		sess.StartedAt = startedAt
		sess.EndedAt = endedAt
		sess.MessageCount = len(messages)
		sess.UserMessageCount = realUserCount
		accumulateMessageTokenUsage(&sess, messages)

		results = append(results, ParseResult{Session: sess, Messages: messages})
	}
	return results, nil
}

func buildPiebaldMessages(db *sql.DB, rows []piebaldMessageRow) ([]ParsedMessage, string, int, error) {
	var (
		messages      []ParsedMessage
		firstMsg      string
		realUserCount int
	)
	for _, mr := range rows {
		msg, ok, err := buildPiebaldMessage(db, mr, len(messages))
		if err != nil {
			return nil, "", 0, err
		}
		if !ok {
			continue
		}
		if firstMsg == "" && msg.Role == RoleUser && strings.TrimSpace(msg.Content) != "" {
			firstMsg = truncate(strings.ReplaceAll(msg.Content, "\n", " "), 300)
		}
		if msg.Role == RoleUser && len(msg.ToolResults) == 0 {
			realUserCount++
		}
		messages = append(messages, msg)
	}
	return messages, firstMsg, realUserCount, nil
}

func buildPiebaldSessionMeta(c piebaldChatRow, dbPath, machine string) ParsedSession {
	cwd := firstNonEmptyPiebald(c.worktreePath, c.currentDirectory, c.projectDirectory)
	project := c.projectName
	if project == "" && c.projectDirectory != "" {
		project = filepath.Base(c.projectDirectory)
	}
	if project == "" && cwd != "" {
		project = filepath.Base(cwd)
	}
	if project == "" {
		project = "piebald"
	}

	return ParsedSession{
		Project:         project,
		Machine:         machine,
		Agent:           AgentPiebald,
		Cwd:             cwd,
		GitBranch:       c.branchName,
		SourceSessionID: fmt.Sprintf("%d", c.id),
		SourceVersion:   "piebald-appdb-v1",
		DisplayName:     c.title,
		File: FileInfo{
			Path:  fmt.Sprintf("%s#%d", dbPath, c.id),
			Mtime: parsePiebaldTimestamp(c.updatedAt).UnixNano(),
		},
	}
}

func loadPiebaldMessages(db *sql.DB, chatID int64) ([]piebaldMessageRow, error) {
	rows, err := db.Query(`
		SELECT id,
		       parent_chat_id,
		       parent_message_id,
		       enabled,
		       role,
		       COALESCE(model, ''),
		       created_at,
		       COALESCE(updated_at, created_at),
		       input_tokens,
		       output_tokens,
		       reasoning_tokens,
		       cache_read_tokens,
		       cache_write_tokens,
		       COALESCE(status, ''),
		       COALESCE(finish_reason, ''),
		       COALESCE(error, '')
		FROM messages
		WHERE parent_chat_id = ?
		ORDER BY created_at, id
	`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []piebaldMessageRow
	for rows.Next() {
		var m piebaldMessageRow
		if err := rows.Scan(
			&m.id, &m.parentChatID, &m.parentMessageID, &m.isEnabled,
			&m.role, &m.model, &m.createdAt, &m.updatedAt,
			&m.inputTokens, &m.outputTokens, &m.reasoningTokens,
			&m.cacheReadTokens, &m.cacheWriteTokens, &m.status,
			&m.finishReason, &m.errorText,
		); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

type piebaldBranch struct {
	rows       []piebaldMessageRow
	parentID   string
	firstRowID int64
}

func splitPiebaldBranches(rows []piebaldMessageRow) []piebaldBranch {
	rowByID := make(map[int64]piebaldMessageRow, len(rows))
	children := make(map[int64][]piebaldMessageRow, len(rows))
	for _, row := range rows {
		rowByID[row.id] = row
		if row.parentMessageID.Valid {
			children[row.parentMessageID.Int64] = append(children[row.parentMessageID.Int64], row)
		}
	}

	var roots []piebaldMessageRow
	for _, row := range rows {
		if !row.parentMessageID.Valid {
			roots = append(roots, row)
			continue
		}
		if _, ok := rowByID[row.parentMessageID.Int64]; !ok {
			roots = append(roots, row)
		}
	}
	if len(roots) != 1 {
		return []piebaldBranch{{rows: rows}}
	}

	var forkBranches []piebaldBranch
	var walk func(row piebaldMessageRow, ownerID string) []piebaldMessageRow
	walk = func(row piebaldMessageRow, ownerID string) []piebaldMessageRow {
		path := []piebaldMessageRow{row}
		current := row
		for {
			kids := children[current.id]
			if len(kids) == 0 {
				break
			}
			if len(kids) == 1 {
				current = kids[0]
				path = append(path, current)
				continue
			}

			mainIdx := len(kids) - 1
			for i := len(kids) - 1; i >= 0; i-- {
				if kids[i].enabled() {
					mainIdx = i
					break
				}
			}
			for i, kid := range kids {
				if i == mainIdx {
					continue
				}
				forkID := fmt.Sprintf("piebald:%d-%d", kid.parentChatID, kid.id)
				// Evaluate walk() before append so nested forks
				// added by the recursive call aren't lost to
				// unspecified Go evaluation order.
				branchRows := walk(kid, forkID)
				forkBranches = append(forkBranches, piebaldBranch{
					rows:       branchRows,
					parentID:   ownerID,
					firstRowID: kid.id,
				})
			}
			current = kids[mainIdx]
			path = append(path, current)
		}
		return path
	}

	mainParent := ""
	mainRows := walk(roots[0], mainParent)
	branches := []piebaldBranch{{rows: mainRows}}
	branches = append(branches, forkBranches...)
	return branches
}

func (m piebaldMessageRow) enabled() bool {
	return m.isEnabled != 0
}

func buildPiebaldMessage(db *sql.DB, mr piebaldMessageRow, ordinal int) (ParsedMessage, bool, error) {
	parts, err := loadPiebaldMessageParts(db, mr.id)
	if err != nil {
		return ParsedMessage{}, false, err
	}

	var (
		contentParts []string
		thinking     []string
		toolCalls    []ParsedToolCall
		toolResults  []ParsedToolResult
	)
	for _, part := range parts {
		switch part.partType {
		case "text":
			text, isThinking, err := loadPiebaldTextPart(db, part.id)
			if err != nil {
				return ParsedMessage{}, false, err
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			if isThinking {
				thinking = append(thinking, text)
			} else {
				contentParts = append(contentParts, text)
			}
		case "tool_call":
			call, result, err := loadPiebaldToolCall(db, part.id)
			if err != nil {
				return ParsedMessage{}, false, err
			}
			if call.ToolUseID != "" {
				toolCalls = append(toolCalls, call)
			}
			if result.ToolUseID != "" {
				toolResults = append(toolResults, result)
			}
		}
	}

	content := strings.TrimSpace(strings.Join(contentParts, "\n"))
	thinkingText := strings.TrimSpace(strings.Join(thinking, "\n"))
	role := piebaldRole(mr.role)
	if content == "" && thinkingText == "" && len(toolCalls) == 0 && len(toolResults) == 0 {
		return ParsedMessage{}, false, nil
	}

	msg := ParsedMessage{
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		ThinkingText:  thinkingText,
		Timestamp:     parsePiebaldTimestamp(mr.createdAt),
		HasThinking:   thinkingText != "",
		HasToolUse:    len(toolCalls) > 0,
		ContentLength: len(content) + len(thinkingText),
		ToolCalls:     toolCalls,
		ToolResults:   toolResults,
		Model:         mr.model,
		StopReason:    mr.finishReason,
	}
	applyPiebaldTokenUsage(&msg, mr)
	return msg, true, nil
}

func loadPiebaldMessageParts(db *sql.DB, messageID int64) ([]piebaldPartRow, error) {
	rows, err := db.Query(`
		SELECT id, part_type, part_index
		FROM message_parts
		WHERE parent_chat_message_id = ?
		ORDER BY part_index, id
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []piebaldPartRow
	for rows.Next() {
		var p piebaldPartRow
		if err := rows.Scan(&p.id, &p.partType, &p.partIndex); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

func loadPiebaldTextPart(db *sql.DB, partID int64) (string, bool, error) {
	var isThinking bool
	if err := db.QueryRow(`
		SELECT COALESCE(is_thinking, 0)
		FROM message_part_text
		WHERE message_part_id = ?
	`, partID).Scan(&isThinking); err != nil {
		return "", false, err
	}
	rows, err := db.Query(`
		SELECT COALESCE(mnt.content, '')
		FROM message_content_nodes mcn
		JOIN message_node_text mnt ON mnt.node_id = mcn.id
		WHERE mcn.parent_text_part_id = ?
		  AND mcn.node_type = 'text'
		ORDER BY mcn.node_index, mcn.id
	`, partID)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var chunks []string
	for rows.Next() {
		var chunk string
		if err := rows.Scan(&chunk); err != nil {
			return "", false, err
		}
		chunks = append(chunks, chunk)
	}
	return strings.Join(chunks, ""), isThinking, rows.Err()
}

func loadPiebaldToolCall(db *sql.DB, partID int64) (ParsedToolCall, ParsedToolResult, error) {
	row := db.QueryRow(`
		SELECT provider_tool_use_id,
		       tool_name,
		       COALESCE(tool_input, ''),
		       tool_result,
		       tool_error,
		       COALESCE(tool_state, ''),
		       sub_agent_chat_id
		FROM message_part_tool_call
		WHERE message_part_id = ?
	`, partID)
	var t piebaldToolCallRow
	if err := row.Scan(
		&t.providerToolUseID, &t.toolName, &t.toolInput,
		&t.toolResult, &t.toolError, &t.toolState, &t.subAgentChatID,
	); err != nil {
		return ParsedToolCall{}, ParsedToolResult{}, err
	}
	call := ParsedToolCall{
		ToolUseID: t.providerToolUseID,
		ToolName:  t.toolName,
		Category:  NormalizeToolCategory(t.toolName),
		InputJSON: normalizeJSON(t.toolInput),
	}
	if t.subAgentChatID.Valid {
		call.SubagentSessionID = fmt.Sprintf("piebald:%d", t.subAgentChatID.Int64)
	}
	resultText := ""
	if t.toolResult.Valid {
		resultText = t.toolResult.String
	} else if t.toolError.Valid {
		resultText = t.toolError.String
	}
	if resultText == "" && t.toolState != "" && t.toolState != "completed" {
		resultText = "[" + t.toolState + "]"
	}
	if resultText == "" {
		return call, ParsedToolResult{}, nil
	}
	quoted, _ := json.Marshal(resultText)
	return call, ParsedToolResult{
		ToolUseID:     t.providerToolUseID,
		ContentLength: len(resultText),
		ContentRaw:    string(quoted),
	}, nil
}

func applyPiebaldTokenUsage(msg *ParsedMessage, mr piebaldMessageRow) {
	msg.tokenPresenceKnown = true

	inputTokens := 0
	outputTokens := 0
	cacheReadTokens := 0
	cacheWriteTokens := 0

	if mr.inputTokens.Valid {
		inputTokens = int(mr.inputTokens.Int64)
		msg.ContextTokens += inputTokens
		msg.HasContextTokens = true
	}
	if mr.cacheReadTokens.Valid {
		cacheReadTokens = int(mr.cacheReadTokens.Int64)
		msg.ContextTokens += cacheReadTokens
		msg.HasContextTokens = true
	}
	if mr.cacheWriteTokens.Valid {
		cacheWriteTokens = int(mr.cacheWriteTokens.Int64)
		msg.ContextTokens += cacheWriteTokens
		msg.HasContextTokens = true
	}
	if mr.outputTokens.Valid {
		outputTokens = int(mr.outputTokens.Int64)
		msg.OutputTokens += outputTokens
		msg.HasOutputTokens = true
	}
	if mr.reasoningTokens.Valid {
		outputTokens += int(mr.reasoningTokens.Int64)
		msg.OutputTokens += int(mr.reasoningTokens.Int64)
		msg.HasOutputTokens = true
	}

	if !msg.HasContextTokens && !msg.HasOutputTokens {
		return
	}

	normalized := map[string]int{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
		"cache_read_input_tokens":     cacheReadTokens,
		"cache_creation_input_tokens": cacheWriteTokens,
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	msg.TokenUsage = j
}

func piebaldRole(raw string) RoleType {
	switch strings.ToLower(raw) {
	case "assistant":
		return RoleAssistant
	default:
		return RoleUser
	}
}

func parsePiebaldTimestamp(raw string) time.Time {
	return parseTimestamp(raw)
}

func normalizeJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return trimmed
	}
	quoted, _ := json.Marshal(trimmed)
	return string(quoted)
}

func firstNonEmptyPiebald(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
