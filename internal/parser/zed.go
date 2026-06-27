package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
)

const (
	zedIDPrefix         = "zed:"
	ZedThreadsDBRelPath = "threads/threads.db"
	zedThreadsDBRelPath = ZedThreadsDBRelPath
)

// ZedSQLiteSessionExists reports whether a top-level Zed thread row
// with the given ID exists in threads.db.
func ZedSQLiteSessionExists(dbPath, sessionID string) bool {
	if dbPath == "" || sessionID == "" {
		return false
	}
	if !IsRegularFile(dbPath) {
		return false
	}
	db, err := openZedDB(dbPath)
	if err != nil {
		return false
	}
	defer db.Close()

	var found int
	err = db.QueryRow(
		`SELECT 1
		   FROM threads
		  WHERE id = ?
		    AND COALESCE(parent_id, '') = ''
		  LIMIT 1`,
		sessionID,
	).Scan(&found)
	return err == nil
}

// ZedSQLiteSourceMtime resolves the per-thread updated_at timestamp
// for a virtual Zed SQLite source path.
func ZedSQLiteSourceMtime(path string) (int64, error) {
	dbPath, sessionID, ok := parseZedVirtualPath(path)
	if !ok {
		return 0, fmt.Errorf("not a zed sqlite virtual path: %s", path)
	}
	db, err := openZedDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var updatedAt string
	err = db.QueryRow(
		`SELECT COALESCE(updated_at, '')
		   FROM threads
		  WHERE id = ?
		    AND COALESCE(parent_id, '') = ''
		  LIMIT 1`,
		sessionID,
	).Scan(&updatedAt)
	if err != nil {
		return 0, fmt.Errorf("loading zed thread mtime %s: %w", sessionID, err)
	}
	return parseTimestamp(updatedAt).UnixNano(), nil
}

// ZedThreadMeta holds lightweight per-thread metadata used by the sync
// engine for per-session skip detection without loading the data payload.
type ZedThreadMeta struct {
	RawID       string
	VirtualPath string
	FileMtime   int64
}

// ListZedThreadMetas queries thread IDs and updated_at timestamps using an
// already-open connection. Used by the Zed provider to check per-session
// mtimes before deciding whether to parse, sharing the same connection as the
// subsequent parseZedThreadFromDB loop to avoid a second DB open.
func ListZedThreadMetas(conn *sql.DB, dbPath string) ([]ZedThreadMeta, error) {
	rows, err := conn.Query(
		`SELECT id, COALESCE(updated_at, '')
		   FROM threads
		  WHERE COALESCE(parent_id, '') = ''
		  ORDER BY updated_at, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing zed thread metas: %w", err)
	}
	defer rows.Close()

	var metas []ZedThreadMeta
	for rows.Next() {
		var id, updatedAt string
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning zed thread meta: %w", err)
		}
		if !IsValidSessionID(id) {
			continue
		}
		metas = append(metas, ZedThreadMeta{
			RawID:       id,
			VirtualPath: ZedSQLiteVirtualPath(dbPath, id),
			FileMtime:   parseTimestamp(updatedAt).UnixNano(),
		})
	}
	return metas, rows.Err()
}

// parseZedThreadFromDB queries and parses one thread using an already-open
// connection. Callers that parse multiple threads should open the DB once and
// call this in a loop to avoid repeated open/close overhead.
func parseZedThreadFromDB(
	conn *sql.DB, dbPath, rawID, machine string, dbInfo os.FileInfo,
) (*ParseResult, error) {
	var row zedThreadRow
	row.id = rawID
	err := conn.QueryRow(
		`SELECT COALESCE(summary, ''), COALESCE(updated_at, ''),
		        COALESCE(data_type, ''), data, COALESCE(parent_id, ''),
		        COALESCE(folder_paths, ''), COALESCE(created_at, '')
		   FROM threads
		  WHERE id = ?
		    AND COALESCE(parent_id, '') = ''`,
		rawID,
	).Scan(
		&row.summary, &row.updatedAt, &row.dataType, &row.data,
		&row.parentID, &row.folderPaths, &row.createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading zed thread %s: %w", rawID, err)
	}
	result, ok := buildZedParseResult(row, dbPath, dbInfo, machine)
	if !ok {
		return nil, nil
	}
	return &result, nil
}

// OpenZedDB opens the Zed threads.db file read-only.
// Callers are responsible for calling Close on the returned *sql.DB.
func OpenZedDB(dbPath string) (*sql.DB, error) {
	return openZedDB(dbPath)
}

func openZedDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?mode=ro&immutable=0&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening zed db %s: %w", dbPath, err)
	}
	return db, nil
}

type zedThreadRow struct {
	id          string
	summary     string
	updatedAt   string
	dataType    string
	data        []byte
	parentID    string
	folderPaths string
	createdAt   string
}

func buildZedParseResult(
	row zedThreadRow,
	dbPath string,
	info os.FileInfo,
	machine string,
) (ParseResult, bool) {
	if !IsValidSessionID(row.id) {
		return ParseResult{}, false
	}
	payload, err := decodeZedThreadData(row.dataType, row.data)
	if err != nil {
		return ParseResult{}, false
	}

	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return ParseResult{}, false
	}

	meta := extractZedDocMeta(doc)
	messages := parseZedMessagesFromDoc(doc)
	for i := range messages {
		messages[i].Ordinal = i
		if messages[i].Role == RoleAssistant && meta.model != "" {
			messages[i].Model = meta.model
		}
	}

	startedAt := parseTimestamp(row.createdAt)
	endedAt := parseTimestamp(row.updatedAt)
	if startedAt.IsZero() {
		startedAt = endedAt
	} else if endedAt.IsZero() {
		endedAt = startedAt
	}

	project := zedProjectFromFolderPaths(row.folderPaths)
	var firstMessage string
	var userCount int
	for _, msg := range messages {
		if msg.Role == RoleUser {
			userCount++
			if firstMessage == "" && strings.TrimSpace(msg.Content) != "" {
				firstMessage = truncate(
					strings.ReplaceAll(msg.Content, "\n", " "), 300,
				)
			}
		}
	}
	if firstMessage == "" {
		firstMessage = truncate(row.summary, 300)
	}

	sessionID := zedIDPrefix + row.id
	sess := ParsedSession{
		ID:                   sessionID,
		Project:              project,
		Machine:              machine,
		Agent:                AgentZed,
		ParentSessionID:      zedParentSessionID(row.parentID),
		RelationshipType:     zedRelationshipType(row.parentID),
		Cwd:                  zedFirstFolderPath(row.folderPaths),
		FirstMessage:         firstMessage,
		SessionName:          row.summary,
		StartedAt:            startedAt,
		EndedAt:              endedAt,
		MessageCount:         len(messages),
		UserMessageCount:     userCount,
		TotalOutputTokens:    meta.totalOutputTokens,
		PeakContextTokens:    meta.peakContextTokens,
		HasTotalOutputTokens: meta.hasTokenUsage,
		HasPeakContextTokens: meta.hasTokenUsage,
		File: FileInfo{
			Path:  ZedSQLiteVirtualPath(dbPath, row.id),
			Size:  info.Size(),
			Mtime: endedAt.UnixNano(),
		},
	}
	var usageEvents []ParsedUsageEvent
	if meta.hasTokenUsage && meta.model != "" {
		usageEvents = []ParsedUsageEvent{{
			SessionID:    sessionID,
			Source:       "session",
			Model:        meta.model,
			InputTokens:  meta.totalInputTokens,
			OutputTokens: meta.totalOutputTokens,
			OccurredAt:   timeString(endedAt, startedAt),
			DedupKey:     "session:" + sessionID,
		}}
	}
	return ParseResult{Session: sess, Messages: messages, UsageEvents: usageEvents}, true
}

func decodeZedThreadData(dataType string, data []byte) ([]byte, error) {
	switch strings.ToLower(dataType) {
	case "json", "":
		return data, nil
	case "zstd":
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer decoder.Close()
		return decoder.DecodeAll(data, nil)
	default:
		return nil, fmt.Errorf("unsupported data_type %q", dataType)
	}
}

func parseZedMessagesFromDoc(doc map[string]any) []ParsedMessage {
	items, ok := doc["messages"].([]any)
	if !ok {
		return nil
	}

	var messages []ParsedMessage
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if user, ok := obj["User"]; ok {
			rawContent := zedMessageContent(user)
			content := strings.TrimSpace(zedExtractText(rawContent))
			// Drop only when there is truly no content at all.
			// Messages with structured blocks (attachments, images) but
			// no Text leaf are kept with empty content so conversation
			// continuity is preserved.
			if content == "" && !zedHasContent(rawContent) {
				continue
			}
			messages = append(messages, ParsedMessage{
				Role:          RoleUser,
				Content:       content,
				ContentLength: len(content),
			})
			continue
		}
		if agent, ok := obj["Agent"]; ok {
			content := strings.TrimSpace(zedExtractText(zedMessageContent(agent)))
			thinking := strings.TrimSpace(zedExtractThinking(agent))
			toolCalls := zedExtractToolCalls(agent)
			toolResults := zedExtractToolResults(agent)
			if content == "" && thinking == "" &&
				len(toolCalls) == 0 && len(toolResults) == 0 {
				continue
			}
			messages = append(messages, ParsedMessage{
				Role:          RoleAssistant,
				Content:       content,
				ThinkingText:  thinking,
				HasThinking:   thinking != "",
				HasToolUse:    len(toolCalls) > 0,
				ContentLength: len(content),
				ToolCalls:     toolCalls,
				ToolResults:   toolResults,
			})
		}
	}
	return messages
}

// zedHasContent reports whether v contains any non-empty content structure.
// Used to distinguish "no content" (drop) from "content without Text blocks" (keep).
func zedHasContent(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func zedMessageContent(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if content, ok := obj["content"]; ok && content != nil {
		return content
	}
	return v
}
