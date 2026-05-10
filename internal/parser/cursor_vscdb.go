package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cursorVscdbBasename is the file name of Cursor's global SQLite
// state database. Used to detect virtual paths that point at
// vscdb-synced sessions and to identify vscdb entries within
// the cursor agent's configured paths.
const cursorVscdbBasename = "state.vscdb"

// IsCursorVscdbPath reports whether path points at a Cursor
// state.vscdb file (basename match). Used by sync code to
// pick the vscdb out of the cursor agent's configured paths
// and by discovery to skip non-transcripts entries.
func IsCursorVscdbPath(path string) bool {
	return filepath.Base(path) == cursorVscdbBasename
}

// FindCursorVscdb returns the first existing state.vscdb path
// in the cursor agent's configured paths, or "" when none
// exist. The agent registry lists the vscdb default for each
// platform and only the matching one resolves at runtime.
func FindCursorVscdb(cursorPaths []string) string {
	for _, p := range cursorPaths {
		if !IsCursorVscdbPath(p) {
			continue
		}
		if info, err := os.Stat(p); err == nil &&
			!info.IsDir() {
			return p
		}
	}
	return ""
}

// IsCursorVscdbVirtualPath reports whether path looks like a
// Cursor vscdb virtual session path (state.vscdb#<sessionID>).
// Used by sync code to detect sessions already populated from
// the richer vscdb source so JSONL transcripts don't overwrite
// them on later watcher events.
func IsCursorVscdbVirtualPath(path string) bool {
	idx := strings.LastIndex(path, "#")
	if idx <= 0 {
		return false
	}
	return filepath.Base(path[:idx]) == cursorVscdbBasename
}

// CursorVscdbMeta is lightweight session metadata from state.vscdb,
// used by the sync engine to detect changes without parsing messages.
type CursorVscdbMeta struct {
	SessionID      string
	VirtualPath    string
	FileMtime      int64 // lastUpdatedAt in nanoseconds (millis * 1e6)
	Project        string
	Name           string
	SubComposerIDs []string
	CreatedAt      int64 // unix millis
	LastUpdatedAt  int64 // unix millis
}

// ListCursorVscdbSessions returns metadata for all Cursor sessions
// found in the global state.vscdb. Returns nil without error if the
// file does not exist. Project names are resolved by scanning the
// workspaceStorage directory adjacent to globalStorage.
func ListCursorVscdbSessions(
	dbPath string,
) ([]CursorVscdbMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openCursorVscdb(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	projects, err := loadCursorWorkspaceProjects(dbPath)
	if err != nil {
		log.Printf("cursor vscdb: loading workspace projects: %v", err)
		// Non-fatal; sessions get "unknown" project.
	}

	rows, err := db.Query(
		"SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"listing cursor vscdb sessions: %w", err,
		)
	}
	defer rows.Close()

	var metas []CursorVscdbMeta
	for rows.Next() {
		var key string
		var rawVal []byte
		if err := rows.Scan(&key, &rawVal); err != nil {
			return nil, fmt.Errorf(
				"scanning cursor vscdb row: %w", err,
			)
		}

		sessionID, ok := strings.CutPrefix(key, "composerData:")
		if !ok || sessionID == "" {
			continue
		}

		var cd cursorComposerData
		if err := json.Unmarshal(rawVal, &cd); err != nil {
			continue
		}

		// Skip sessions with no conversation content.
		if len(cd.FullConversationHeadersOnly) == 0 {
			continue
		}

		project := projects[sessionID]
		if project == "" {
			project = "unknown"
		}

		subIDs := cd.SubComposerIDs
		if len(cd.SubagentComposerIDs) > 0 {
			subIDs = append(subIDs, cd.SubagentComposerIDs...)
		}

		metas = append(metas, CursorVscdbMeta{
			SessionID:      sessionID,
			VirtualPath:    dbPath + "#" + sessionID,
			FileMtime:      cd.LastUpdatedAt * 1_000_000,
			Project:        project,
			Name:           cd.Name,
			SubComposerIDs: subIDs,
			CreatedAt:      cd.CreatedAt,
			LastUpdatedAt:  cd.LastUpdatedAt,
		})
	}
	return metas, rows.Err()
}

// ParseCursorVscdbSession parses a single Cursor session from
// state.vscdb. Returns nil without error for empty sessions.
func ParseCursorVscdbSession(
	dbPath, sessionID, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf(
			"cursor vscdb not found: %s", dbPath,
		)
	}

	db, err := openCursorVscdb(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	// Load session metadata.
	var rawVal []byte
	err = db.QueryRow(
		"SELECT value FROM cursorDiskKV WHERE key = ?",
		"composerData:"+sessionID,
	).Scan(&rawVal)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading cursor vscdb session %s: %w",
			sessionID, err,
		)
	}

	var cd cursorComposerData
	if err := json.Unmarshal(rawVal, &cd); err != nil {
		return nil, nil, fmt.Errorf(
			"parsing cursor vscdb composerData %s: %w",
			sessionID, err,
		)
	}

	if len(cd.FullConversationHeadersOnly) == 0 {
		return nil, nil, nil
	}

	// Load all bubbles for this session.
	bubbles, err := loadCursorBubbles(db, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading cursor vscdb bubbles %s: %w",
			sessionID, err,
		)
	}

	msgs := buildCursorVscdbMessages(
		cd.FullConversationHeadersOnly, bubbles,
	)

	if len(msgs) == 0 {
		return nil, nil, nil
	}

	firstMsg := ""
	userCount := 0
	for _, m := range msgs {
		if m.Role == RoleUser {
			userCount++
			if firstMsg == "" && m.Content != "" {
				firstMsg = truncate(
					strings.ReplaceAll(m.Content, "\n", " "),
					300,
				)
			}
		}
	}

	if userCount == 0 {
		return nil, nil, nil
	}

	startedAt := millisToTime(cd.CreatedAt)
	endedAt := millisToTime(cd.LastUpdatedAt)

	if project == "" {
		project = "unknown"
	}

	sess := &ParsedSession{
		ID:               "cursor:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentCursor,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(msgs),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  dbPath + "#" + sessionID,
			Mtime: cd.LastUpdatedAt * 1_000_000,
		},
	}

	return sess, msgs, nil
}

// cursorComposerData is the JSON structure stored under
// composerData:<sessionId> in the cursorDiskKV table.
type cursorComposerData struct {
	ComposerID                  string               `json:"composerId"`
	Name                        string               `json:"name"`
	CreatedAt                   int64                `json:"createdAt"`
	LastUpdatedAt               int64                `json:"lastUpdatedAt"`
	FullConversationHeadersOnly []cursorBubbleHeader `json:"fullConversationHeadersOnly"`
	SubComposerIDs              []string             `json:"subComposerIds"`
	SubagentComposerIDs         []string             `json:"subagentComposerIds"`
	Status                      string               `json:"status"`
	UnifiedMode                 string               `json:"unifiedMode"`
}

// cursorBubbleHeader is one entry in fullConversationHeadersOnly.
type cursorBubbleHeader struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"` // 1=user, 2=assistant
}

// cursorBubble is the JSON structure stored under
// bubbleId:<sessionId>:<bubbleId> in cursorDiskKV.
type cursorBubble struct {
	BubbleID       string                `json:"bubbleId"`
	Type           int                   `json:"type"` // 1=user, 2=assistant
	Text           string                `json:"text"`
	CreatedAt      string                `json:"createdAt"` // ISO 8601 string
	ToolFormerData *cursorToolFormerData `json:"toolFormerData"`
}

// cursorToolFormerData holds tool call information embedded in
// an assistant bubble.
type cursorToolFormerData struct {
	Name       string `json:"name"`
	ToolCallID string `json:"toolCallId"`
	Status     string `json:"status"`
	// Params and Result are JSON strings (not nested objects).
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func openCursorVscdb(dbPath string) (*sql.DB, error) {
	// mode=ro avoids racing Cursor's writer; _busy_timeout
	// retries briefly when Cursor holds a SHARED lock.
	// _journal_mode is intentionally not forced — Cursor
	// already runs in WAL, and forcing it here would require
	// a write that fails under mode=ro.
	dsn := cursorVscdbDSN(
		dbPath, "mode=ro&_busy_timeout=3000",
	)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf(
			"opening cursor vscdb %s: %w", dbPath, err,
		)
	}
	return db, nil
}

// cursorVscdbDSN builds a SQLite file: URI for the given path
// with the supplied query parameters. Going through a proper
// URI (rather than `path?params` concatenation) ensures any
// `?`, `#`, or `%` characters in the filesystem path are
// percent-encoded instead of being parsed by the SQLite driver
// as part of the DSN query string. Forward slashes are used
// regardless of OS so the URI matches RFC 3986.
func cursorVscdbDSN(dbPath, rawQuery string) string {
	p := filepath.ToSlash(dbPath)
	// Ensure a leading slash so url.URL serializes as
	// "file:///path" or "file:///C:/path" rather than
	// "file://C:/path", which omits the host component.
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := &url.URL{
		Scheme:   "file",
		Path:     p,
		RawQuery: rawQuery,
	}
	return u.String()
}

// loadCursorWorkspaceProjects scans workspaceStorage directories
// adjacent to globalStorage and returns a map of
// composerId → project name.
func loadCursorWorkspaceProjects(
	globalDbPath string,
) (map[string]string, error) {
	// globalStorage/state.vscdb → workspaceStorage/
	globalStorageDir := filepath.Dir(globalDbPath)
	userDir := filepath.Dir(globalStorageDir)
	wsDir := filepath.Join(userDir, "workspaceStorage")

	entries, err := os.ReadDir(wsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"reading workspaceStorage: %w", err,
		)
	}

	projects := make(map[string]string)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirPath := filepath.Join(wsDir, e.Name())
		project := extractWorkspaceProject(dirPath)
		if project == "" {
			continue
		}
		ids := extractWorkspaceComposerIDs(dirPath)
		for _, id := range ids {
			if id != "" {
				projects[id] = project
			}
		}
	}
	return projects, nil
}

// extractWorkspaceProject reads the project path from
// workspaceStorage/<hash>/workspace.json.
func extractWorkspaceProject(dirPath string) string {
	wjPath := filepath.Join(dirPath, "workspace.json")
	data, err := os.ReadFile(wjPath)
	if err != nil {
		return ""
	}
	var wj struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(data, &wj); err != nil {
		return ""
	}
	if wj.Folder == "" {
		return ""
	}

	// folder is a file:// URL, e.g. "file:///home/user/proj"
	// or "file:///C:/repo" / "file://host/share" on Windows.
	folderPath := wj.Folder
	if strings.HasPrefix(folderPath, "file://") {
		folderPath = fileURLToPath(folderPath)
	}

	return ExtractProjectFromCwd(folderPath)
}

// fileURLToPath converts a "file://" URL into a native path,
// preserving Windows drive letters and UNC host components
// when running on Windows. Returns "" when the URL cannot be
// parsed; returns the input unchanged when it lacks the
// "file://" scheme.
func fileURLToPath(raw string) string {
	if !strings.HasPrefix(raw, "file://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path, err := url.PathUnescape(u.Path)
	if err != nil {
		path = u.Path
	}
	if runtime.GOOS == "windows" {
		// "file://host/share" → UNC: \\host\share
		if u.Host != "" {
			return `\\` + u.Host + filepath.FromSlash(path)
		}
		// "file:///C:/repo" → "/C:/repo"; strip the leading
		// slash so the drive letter lands at the start.
		path = strings.TrimPrefix(path, "/")
		return filepath.FromSlash(path)
	}
	if u.Host != "" {
		// Non-Windows hosts cannot mount remote UNC paths
		// directly; fall back to the path component so
		// project extraction still has something to work with.
		return path
	}
	return path
}

// extractWorkspaceComposerIDs reads composer IDs from
// workspaceStorage/<hash>/state.vscdb ItemTable.
func extractWorkspaceComposerIDs(dirPath string) []string {
	dbPath := filepath.Join(dirPath, "state.vscdb")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	db, err := sql.Open(
		"sqlite3",
		cursorVscdbDSN(dbPath, "mode=ro&_busy_timeout=3000"),
	)
	if err != nil {
		return nil
	}
	defer db.Close()

	var rawVal []byte
	err = db.QueryRow(
		"SELECT value FROM ItemTable WHERE key = 'composer.composerData'",
	).Scan(&rawVal)
	if err != nil {
		return nil
	}

	var cd struct {
		AllComposers []struct {
			ComposerID string `json:"composerId"`
		} `json:"allComposers"`
	}
	if err := json.Unmarshal(rawVal, &cd); err != nil {
		return nil
	}

	ids := make([]string, 0, len(cd.AllComposers))
	for _, c := range cd.AllComposers {
		if c.ComposerID != "" {
			ids = append(ids, c.ComposerID)
		}
	}
	return ids
}

// loadCursorBubbles fetches all bubble data for a session,
// keyed by bubble ID.
func loadCursorBubbles(
	db *sql.DB, sessionID string,
) (map[string]cursorBubble, error) {
	rows, err := db.Query(
		"SELECT key, value FROM cursorDiskKV WHERE key LIKE ?",
		"bubbleId:"+sessionID+":%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bubbles := make(map[string]cursorBubble)
	for rows.Next() {
		var key string
		var rawVal []byte
		if err := rows.Scan(&key, &rawVal); err != nil {
			return nil, err
		}

		// key = "bubbleId:<sessionId>:<bubbleId>"
		parts := strings.SplitN(key, ":", 3)
		if len(parts) != 3 {
			continue
		}
		bubbleID := parts[2]

		var b cursorBubble
		if err := json.Unmarshal(rawVal, &b); err != nil {
			continue
		}
		bubbles[bubbleID] = b
	}
	return bubbles, rows.Err()
}

// buildCursorVscdbMessages reconstructs ParsedMessages from bubble
// headers and bubble data. Consecutive assistant bubbles (text +
// tool calls) are merged into a single assistant ParsedMessage.
func buildCursorVscdbMessages(
	headers []cursorBubbleHeader,
	bubbles map[string]cursorBubble,
) []ParsedMessage {
	var msgs []ParsedMessage
	ordinal := 0

	// Tracks the current assistant message being assembled.
	var curAsst *ParsedMessage

	flushAssistant := func() {
		if curAsst == nil {
			return
		}
		if strings.TrimSpace(curAsst.Content) != "" ||
			curAsst.HasToolUse {
			msgs = append(msgs, *curAsst)
			ordinal++
		}
		curAsst = nil
	}

	for _, h := range headers {
		b, ok := bubbles[h.BubbleID]
		if !ok {
			continue
		}

		switch h.Type {
		case 1: // user
			flushAssistant()
			text := strings.TrimSpace(b.Text)
			if text == "" {
				continue
			}
			msgs = append(msgs, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       text,
				Timestamp:     parseCursorBubbleTime(b.CreatedAt),
				ContentLength: len(text),
			})
			ordinal++

		case 2: // assistant
			isToolCall := b.ToolFormerData != nil &&
				b.ToolFormerData.Name != ""

			if curAsst == nil {
				ts := parseCursorBubbleTime(b.CreatedAt)
				curAsst = &ParsedMessage{
					Ordinal:   ordinal,
					Role:      RoleAssistant,
					Timestamp: ts,
				}
			}

			if isToolCall {
				tc := buildCursorToolCall(b.ToolFormerData)
				curAsst.ToolCalls = append(
					curAsst.ToolCalls, tc,
				)
				curAsst.HasToolUse = true
				if tr, ok := buildCursorToolResult(
					b.ToolFormerData,
				); ok {
					curAsst.ToolResults = append(
						curAsst.ToolResults, tr,
					)
				}
			} else {
				text := strings.TrimSpace(b.Text)
				if text != "" {
					if curAsst.Content != "" {
						curAsst.Content += "\n"
					}
					curAsst.Content += text
				}
			}
		}
	}

	flushAssistant()

	// Update ContentLength on all messages.
	for i := range msgs {
		msgs[i].ContentLength = len(msgs[i].Content)
	}

	return msgs
}

// buildCursorToolCall converts a cursorToolFormerData into a
// ParsedToolCall using the vscdb tool name taxonomy.
func buildCursorToolCall(
	tf *cursorToolFormerData,
) ParsedToolCall {
	if tf == nil {
		return ParsedToolCall{}
	}

	inputJSON := ""
	if len(tf.Params) > 0 {
		// params may be a JSON string (needs unquoting) or
		// a JSON object — normalize to object form.
		inputJSON = normalizeCursorParamsJSON(tf.Params)
	}

	return ParsedToolCall{
		ToolUseID: tf.ToolCallID,
		ToolName:  tf.Name,
		Category:  NormalizeToolCategory(tf.Name),
		InputJSON: inputJSON,
	}
}

// buildCursorToolResult converts the result field on a
// cursorToolFormerData into a ParsedToolResult. Returns false
// when the tool call has no captured result so the caller can
// skip empty entries. ContentRaw stores the raw JSON value as
// returned by Cursor; ContentLength reflects the decoded
// textual length so search/analytics see comparable numbers
// to JSONL-sourced sessions.
func buildCursorToolResult(
	tf *cursorToolFormerData,
) (ParsedToolResult, bool) {
	if tf == nil || len(tf.Result) == 0 || tf.ToolCallID == "" {
		return ParsedToolResult{}, false
	}
	raw := string(tf.Result)
	return ParsedToolResult{
		ToolUseID:     tf.ToolCallID,
		ContentLength: len(DecodeContent(raw)),
		ContentRaw:    raw,
	}, true
}

// normalizeCursorParamsJSON handles the case where params is
// stored as a JSON-encoded string (a string containing JSON)
// rather than a JSON object directly.
func normalizeCursorParamsJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// If it's a JSON string, unwrap it.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	// Already a JSON object or array.
	return string(raw)
}

// parseCursorBubbleTime parses the ISO 8601 createdAt string
// used in Cursor bubbles. Returns zero time on parse failure.
func parseCursorBubbleTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999Z",
		"2006-01-02T15:04:05Z",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
