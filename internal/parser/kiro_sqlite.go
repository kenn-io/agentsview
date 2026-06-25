package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const kiroSQLiteDBName = "data.sqlite3"

// KiroSQLiteSessionMeta is lightweight per-conversation metadata
// used to detect changed Kiro SQLite sessions without parsing the
// entire JSON payload on every sync.
type KiroSQLiteSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
}

type kiroSQLiteRow struct {
	key            string
	conversationID string
	value          string
	createdAt      int64
	updatedAt      int64
}

// KiroSQLiteStore keeps one read-only SQLite handle open while a
// caller performs multiple Kiro conversation lookups from the same DB.
type KiroSQLiteStore struct {
	dbPath string
	db     *sql.DB
}

// OpenKiroSQLiteStore opens a read-only current-store Kiro SQLite DB.
func OpenKiroSQLiteStore(dbPath string) (*KiroSQLiteStore, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kiro sqlite db not found: %s", dbPath)
	}
	db, err := openKiroSQLiteDB(dbPath)
	if err != nil {
		return nil, err
	}
	return &KiroSQLiteStore{dbPath: dbPath, db: db}, nil
}

// Close releases the underlying SQLite handle.
func (s *KiroSQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// kiroSQLiteDBPath returns the current-store Kiro SQLite DB when the
// configured root contains one.
func kiroSQLiteDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, kiroSQLiteDBName)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

// KiroSQLiteVirtualPath gives each conversation inside the shared
// Kiro DB a stable source identity for the AgentsView archive.
func KiroSQLiteVirtualPath(dbPath, sessionID string) string {
	return VirtualSourcePath(dbPath, sessionID)
}

// kiroSQLiteVirtualPathParts splits a virtual Kiro SQLite source
// path back into its database path and raw session ID.
func kiroSQLiteVirtualPathParts(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, kiroSQLiteDBName)
}

// KiroSQLiteSessionExists reports whether the current Kiro DB has
// at least one row for sessionID.
func KiroSQLiteSessionExists(dbPath, sessionID string) bool {
	if dbPath == "" || sessionID == "" {
		return false
	}
	store, err := OpenKiroSQLiteStore(dbPath)
	if err != nil {
		return false
	}
	defer store.Close()
	return store.SessionExists(sessionID)
}

// SessionExists reports whether the current Kiro DB has at least one
// row for sessionID.
func (s *KiroSQLiteStore) SessionExists(sessionID string) bool {
	if s == nil || s.db == nil || sessionID == "" {
		return false
	}
	var found int
	err := s.db.QueryRow(
		`SELECT 1
		   FROM conversations_v2
		  WHERE conversation_id = ?
		  LIMIT 1`,
		sessionID,
	).Scan(&found)
	return err == nil
}

// ListKiroSQLiteSessionMeta returns one metadata row per logical
// conversation. Kiro's schema permits the same conversation_id
// under multiple cwd keys, so the newest row is canonical.
func ListKiroSQLiteSessionMeta(
	dbPath string,
) ([]KiroSQLiteSessionMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	store, err := OpenKiroSQLiteStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListSessionMeta()
}

// ListSessionMeta returns one metadata row per logical conversation
// using the store's existing SQLite handle.
func (s *KiroSQLiteStore) ListSessionMeta() ([]KiroSQLiteSessionMeta, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kiro sqlite store is closed")
	}
	rows, err := s.db.Query(`
		SELECT conversation_id, MAX(updated_at)
		  FROM conversations_v2
		 GROUP BY conversation_id
	`)
	if err != nil {
		return nil, fmt.Errorf(
			"listing kiro sqlite sessions: %w", err,
		)
	}
	defer rows.Close()

	var metas []KiroSQLiteSessionMeta
	for rows.Next() {
		var id string
		var updatedAt int64
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return nil, fmt.Errorf(
				"scanning kiro sqlite session meta: %w", err,
			)
		}
		if id == "" {
			continue
		}
		metas = append(metas, KiroSQLiteSessionMeta{
			SessionID:   id,
			VirtualPath: KiroSQLiteVirtualPath(s.dbPath, id),
			FileMtime:   updatedAt * 1_000_000,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].SessionID < metas[j].SessionID
	})
	return metas, nil
}

// KiroSQLiteSessionIDs returns the set of current-store logical
// conversation IDs under dir.
func KiroSQLiteSessionIDs(dir string) map[string]struct{} {
	dbPath := kiroSQLiteDBPath(dir)
	if dbPath == "" {
		return nil
	}
	metas, err := ListKiroSQLiteSessionMeta(dbPath)
	if err != nil {
		return nil
	}
	ids := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		ids[meta.SessionID] = struct{}{}
	}
	return ids
}

// KiroSQLiteSourceMtime resolves the canonical per-session
// updated_at timestamp for a virtual SQLite source path.
func KiroSQLiteSourceMtime(path string) (int64, error) {
	dbPath, sessionID, ok := kiroSQLiteVirtualPathParts(path)
	if !ok {
		return 0, fmt.Errorf("not a kiro sqlite virtual path: %s", path)
	}
	row, err := loadKiroSQLiteRow(dbPath, sessionID)
	if err != nil {
		return 0, err
	}
	return row.updatedAt * 1_000_000, nil
}

// parseKiroSQLiteSession parses one current-store Kiro CLI conversation
// into normal AgentsView session/message records.
func parseKiroSQLiteSession(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	store, err := OpenKiroSQLiteStore(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	return store.ParseSession(sessionID, machine)
}

// ParseSession parses one current-store Kiro CLI conversation using
// the store's existing SQLite handle.
func (s *KiroSQLiteStore) ParseSession(
	sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	row, err := s.loadRow(sessionID)
	if err != nil {
		return nil, nil, err
	}
	if !gjson.Valid(row.value) {
		return nil, nil, fmt.Errorf(
			"kiro sqlite session %s has malformed payload",
			sessionID,
		)
	}

	var messages []ParsedMessage
	var firstMessage string
	var cwd string
	ordinal := 0

	gjson.Get(row.value, "history").ForEach(
		func(_, turn gjson.Result) bool {
			user := turn.Get("user")
			if cwd == "" {
				cwd = user.Get(
					"env_context.env_state.current_working_directory",
				).Str
			}
			if prompt := strings.TrimSpace(
				user.Get("content.Prompt.prompt").Str,
			); prompt != "" {
				if firstMessage == "" {
					firstMessage = truncate(
						strings.ReplaceAll(prompt, "\n", " "),
						300,
					)
				}
				messages = append(messages, ParsedMessage{
					Ordinal:       ordinal,
					Role:          RoleUser,
					Content:       prompt,
					ContentLength: len(prompt),
					Timestamp: parseTimestamp(
						user.Get("timestamp").Str,
					),
				})
				ordinal++
			}

			if assistant, ok := parseKiroSQLiteAssistant(
				turn.Get("assistant"),
				turn.Get("request_metadata.stream_end_timestamp_ms").Int(),
				ordinal,
			); ok {
				messages = append(messages, assistant)
				ordinal++
			}
			return true
		},
	)

	hasContent := false
	for _, msg := range messages {
		if msg.Content != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil, nil, nil
	}

	if row.key != "" {
		cwd = row.key
	}
	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = "unknown"
	}

	userCount := 0
	for _, msg := range messages {
		if msg.Role == RoleUser && msg.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               "kiro:" + row.conversationID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentKiro,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
		StartedAt:        time.UnixMilli(row.createdAt).UTC(),
		EndedAt:          time.UnixMilli(row.updatedAt).UTC(),
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  KiroSQLiteVirtualPath(s.dbPath, row.conversationID),
			Size:  int64(len(row.value)),
			Mtime: row.updatedAt * 1_000_000,
		},
	}
	return sess, messages, nil
}

func openKiroSQLiteDB(dbPath string) (*sql.DB, error) {
	dsn := dbPath +
		"?mode=ro&_journal_mode=WAL&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf(
			"opening kiro sqlite db %s: %w", dbPath, err,
		)
	}
	return db, nil
}

func loadKiroSQLiteRow(
	dbPath, sessionID string,
) (kiroSQLiteRow, error) {
	store, err := OpenKiroSQLiteStore(dbPath)
	if err != nil {
		return kiroSQLiteRow{}, err
	}
	defer store.Close()
	return store.loadRow(sessionID)
}

func (s *KiroSQLiteStore) loadRow(
	sessionID string,
) (kiroSQLiteRow, error) {
	if s == nil || s.db == nil {
		return kiroSQLiteRow{}, fmt.Errorf("kiro sqlite store is closed")
	}
	var row kiroSQLiteRow
	err := s.db.QueryRow(`
		SELECT key, conversation_id, value,
		       created_at, updated_at
		  FROM conversations_v2
		 WHERE conversation_id = ?
		 ORDER BY updated_at DESC, created_at DESC, key DESC
		 LIMIT 1
	`, sessionID).Scan(
		&row.key, &row.conversationID, &row.value,
		&row.createdAt, &row.updatedAt,
	)
	if err != nil {
		return kiroSQLiteRow{}, fmt.Errorf(
			"loading kiro sqlite session %s: %w",
			sessionID, err,
		)
	}
	return row, nil
}

func parseKiroSQLiteAssistant(
	assistant gjson.Result, streamEndMS int64, ordinal int,
) (ParsedMessage, bool) {
	timestamp := time.Time{}
	if streamEndMS > 0 {
		timestamp = time.UnixMilli(streamEndMS).UTC()
	}

	if response := assistant.Get("Response"); response.Exists() {
		content := strings.TrimSpace(response.Get("content").Str)
		if content == "" {
			return ParsedMessage{}, false
		}
		return ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleAssistant,
			Content:       content,
			ContentLength: len(content),
			Timestamp:     timestamp,
		}, true
	}

	toolUse := assistant.Get("ToolUse")
	if !toolUse.Exists() {
		return ParsedMessage{}, false
	}
	text := strings.TrimSpace(toolUse.Get("content").Str)
	toolCalls := kiroSQLiteToolCalls(toolUse.Get("tool_uses"))
	hasToolUse := len(toolCalls) > 0
	displayContent := text
	if displayContent == "" && hasToolUse {
		displayContent = kiroFormatToolCalls(toolCalls)
	}
	if displayContent == "" && !hasToolUse {
		return ParsedMessage{}, false
	}
	return ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleAssistant,
		Content:       displayContent,
		ContentLength: len(displayContent),
		Timestamp:     timestamp,
		HasToolUse:    hasToolUse,
		ToolCalls:     toolCalls,
	}, true
}

func kiroSQLiteToolCalls(
	toolUses gjson.Result,
) []ParsedToolCall {
	var calls []ParsedToolCall
	toolUses.ForEach(func(_, toolUse gjson.Result) bool {
		name := toolUse.Get("name").Str
		if name == "" {
			return true
		}
		displayName := name
		category := NormalizeToolCategory(name)
		if name == "write" {
			if toolUse.Get("args.command").Str == "strReplace" {
				displayName = "Edit"
				category = "Edit"
			} else {
				displayName = "Write"
			}
		}
		calls = append(calls, ParsedToolCall{
			ToolUseID: toolUse.Get("id").Str,
			ToolName:  displayName,
			Category:  category,
			InputJSON: toolUse.Get("args").Raw,
		})
		return true
	})
	return calls
}
