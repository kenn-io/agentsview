package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const openCodeStorageFingerprintPrefix = "opencode-storage:v1:"

// OpenCodeSessionMeta is lightweight metadata for a session,
// used to detect changes without parsing messages or parts.
type OpenCodeSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
}

// OpenCodeSQLiteSessionExists reports whether a session row with
// the given ID is present in the OpenCode SQLite database at
// dbPath. Returns false when the file is missing, the schema is
// unexpected, or no row matches. Used by the OpenCode-format
// provider's source lookup so callers can distinguish "this DB has
// the session" from
// "this DB exists but doesn't have it" — the latter must let
// resolution continue to other configured roots.
func OpenCodeSQLiteSessionExists(dbPath, sessionID string) bool {
	if dbPath == "" || sessionID == "" {
		return false
	}
	info, err := os.Stat(dbPath)
	if err != nil || info.IsDir() {
		return false
	}
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	var found int
	err = db.QueryRow(
		"SELECT 1 FROM session WHERE id = ? LIMIT 1",
		sessionID,
	).Scan(&found)
	return err == nil
}

// ListOpenCodeSessionMeta returns lightweight metadata for
// all sessions without parsing messages or parts. Used by
// the sync engine to detect which sessions have changed.
func ListOpenCodeSessionMeta(
	dbPath string,
) ([]OpenCodeSessionMeta, error) {
	var metas []OpenCodeSessionMeta
	err := ForEachOpenCodeSessionMeta(
		context.Background(), dbPath,
		func(meta OpenCodeSessionMeta) error {
			metas = append(metas, meta)
			return nil
		},
	)
	return metas, err
}

// ForEachOpenCodeSessionMeta streams lightweight session rows directly from
// SQLite. The callback runs while the read-only query is open and receives one
// row at a time; callers must not retain database-owned values.
func ForEachOpenCodeSessionMeta(
	ctx context.Context,
	dbPath string,
	yield func(OpenCodeSessionMeta) error,
) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		"SELECT id, time_updated FROM session",
	)
	if err != nil {
		return fmt.Errorf(
			"listing opencode sessions: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var timeUpdated int64
		if err := rows.Scan(
			&id, &timeUpdated,
		); err != nil {
			return fmt.Errorf(
				"scanning opencode session meta: %w", err,
			)
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(OpenCodeSessionMeta{
			SessionID:   id,
			VirtualPath: dbPath + "#" + id,
			FileMtime:   timeUpdated * 1_000_000,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// parseOpenCodeDBSession parses a single session by ID from the
// OpenCode SQLite database. The OpenCode-format provider owns this
// path; Kilo and MiMoCode reuse it and relabel the result.
func parseOpenCodeDBSession(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf(
			"opencode db not found: %s", dbPath,
		)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	projects, err := loadOpenCodeProjectsCached(db, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading opencode projects: %w", err,
		)
	}

	hasDirectory, err := openCodeSessionHasDirectoryCached(db, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"probing opencode session schema: %w", err,
		)
	}

	s, err := loadOneOpenCodeSession(db, sessionID, hasDirectory)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading opencode session %s: %w",
			sessionID, err,
		)
	}

	projectWorktree := strings.TrimSpace(projects[s.projectID])
	cwd := resolveOpenCodeWorktree(s.directory, projectWorktree)
	if !openCodeUsableWorktree(projectWorktree) {
		projectWorktree = cwd
	}
	return buildOpenCodeSession(
		db, s, cwd, projectWorktree, dbPath, machine,
	)
}

// resolveOpenCodeWorktree picks the session working directory used for
// cwd/project. OpenCode's synthetic "global" project stores worktree="/",
// while session.directory still holds the real path the session ran in.
// Prefer a concrete session directory; fall back to the project worktree.
func resolveOpenCodeWorktree(
	sessionDirectory, projectWorktree string,
) string {
	if dir := strings.TrimSpace(sessionDirectory); openCodeUsableWorktree(dir) {
		return dir
	}
	return strings.TrimSpace(projectWorktree)
}

func openCodeUsableWorktree(path string) bool {
	if path == "" {
		return false
	}
	// Root is OpenCode's global-project placeholder, not a real project cwd.
	return path != string(filepath.Separator) && path != "/"
}

// parseOpenCodeStorageFile parses a file-backed OpenCode storage
// session rooted at storage/session/<project>/<session>.json. The
// OpenCode-format provider owns this path; Kilo and MiMoCode reuse it
// and relabel the result.
func parseOpenCodeStorageFile(
	sessionPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"reading opencode session file %s: %w",
			sessionPath, err,
		)
	}

	var sf openCodeStorageSessionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, nil, fmt.Errorf(
			"decoding opencode session file %s: %w",
			sessionPath, err,
		)
	}
	if sf.ID == "" {
		return nil, nil, fmt.Errorf(
			"opencode session file %s missing id",
			sessionPath,
		)
	}

	root := filepath.Dir(filepath.Dir(filepath.Dir(
		filepath.Dir(sessionPath),
	)))
	// OpenCode session sync replaces the full stored transcript.
	// If a child JSON is truncated mid-write, skipping it here
	// would silently drop previously persisted content until the
	// next successful sync, so malformed children abort the parse.
	msgs, err := loadOpenCodeStorageMessages(root, sf.ID)
	if err != nil {
		return nil, nil, err
	}
	parts, err := loadOpenCodeStorageParts(root, msgs)
	if err != nil {
		return nil, nil, err
	}
	fileMtime, err := OpenCodeSourceMtime(sessionPath)
	if err != nil {
		return nil, nil, err
	}

	sess, parsed, err := buildOpenCodeParsedSession(
		openCodeSessionRow{
			id:          sf.ID,
			parentID:    sf.ParentID,
			title:       sf.Title,
			timeCreated: sf.Time.Created,
			timeUpdated: sf.Time.Updated,
		},
		sf.Directory,
		sf.Directory,
		sessionPath,
		fileMtime,
		machine,
		msgs,
		parts,
	)
	if err != nil || sess == nil {
		return sess, parsed, err
	}
	sess.File.Hash = buildOpenCodeSessionFingerprint(
		openCodeSessionRow{
			id:          sf.ID,
			parentID:    sf.ParentID,
			title:       sf.Title,
			timeCreated: sf.Time.Created,
			timeUpdated: sf.Time.Updated,
		},
		sf.Directory,
		sf.Directory,
		msgs,
		parts,
	)
	return sess, parsed, nil
}

func openOpenCodeDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(dbPath) +
		"?mode=ro&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf(
			"opening opencode db %s: %w", dbPath, err,
		)
	}
	return db, nil
}

// openCodeProject is a row from the opencode project table.
type openCodeProject struct {
	id       string
	worktree string
}

func loadOpenCodeProjects(
	db *sql.DB,
) (map[string]string, error) {
	rows, err := db.Query(
		"SELECT id, worktree FROM project",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	projects := make(map[string]string)
	for rows.Next() {
		var p openCodeProject
		if err := rows.Scan(&p.id, &p.worktree); err != nil {
			return nil, err
		}
		projects[p.id] = p.worktree
	}
	return projects, rows.Err()
}

type openCodeProjectsCacheEntry struct {
	state    SQLiteContainerState
	projects map[string]string
}

// openCodeProjectsCache memoizes the project table per shared SQLite DB,
// keyed by the container's change-detection state. The engine parses each
// session of a container through an independent provider instance, so
// without this cache every parsed session re-queried the full project
// table — the dominant per-session cost when re-verifying a changed
// container. Entries hold only a handful of small maps (one per configured
// container path) and are replaced in place.
var (
	openCodeProjectsCacheMu sync.Mutex
	openCodeProjectsCache   = map[string]openCodeProjectsCacheEntry{}
)

// loadOpenCodeProjectsCached returns the project→worktree map for the
// shared DB at dbPath, reusing the previous load while the container state
// is unchanged. The state is captured before the query, so a write racing
// the load can only make cached data newer than its key — the next capture
// then mismatches and reloads. The returned map is shared and must be
// treated as read-only.
func loadOpenCodeProjectsCached(
	db *sql.DB, dbPath string,
) (map[string]string, error) {
	state, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return loadOpenCodeProjects(db)
	}
	openCodeProjectsCacheMu.Lock()
	entry, hit := openCodeProjectsCache[dbPath]
	openCodeProjectsCacheMu.Unlock()
	if hit && entry.state == state {
		return entry.projects, nil
	}
	projects, err := loadOpenCodeProjects(db)
	if err != nil {
		return nil, err
	}
	openCodeProjectsCacheMu.Lock()
	openCodeProjectsCache[dbPath] = openCodeProjectsCacheEntry{
		state:    state,
		projects: projects,
	}
	openCodeProjectsCacheMu.Unlock()
	return projects, nil
}

// openCodeSessionRow is a row from the opencode session table.
type openCodeSessionRow struct {
	id          string
	projectID   string
	parentID    string
	title       string
	directory   string
	timeCreated int64
	timeUpdated int64
}

type openCodeSessionSchemaCacheEntry struct {
	state        SQLiteContainerState
	hasDirectory bool
}

// openCodeSessionSchemaCache memoizes whether session.directory exists for
// each shared OpenCode SQLite path. Legacy OpenCode-family DBs (older
// OpenCode, Kilo, MiMoCode, ICodeMate) omit the column; probing once per
// container state avoids a PRAGMA on every session parse.
var (
	openCodeSessionSchemaCacheMu sync.Mutex
	openCodeSessionSchemaCache   = map[string]openCodeSessionSchemaCacheEntry{}
)

func openCodeSessionHasDirectoryCached(
	db *sql.DB, dbPath string,
) (bool, error) {
	state, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return openCodeSessionTableHasDirectory(db)
	}
	openCodeSessionSchemaCacheMu.Lock()
	entry, hit := openCodeSessionSchemaCache[dbPath]
	openCodeSessionSchemaCacheMu.Unlock()
	if hit && entry.state == state {
		return entry.hasDirectory, nil
	}
	hasDirectory, err := openCodeSessionTableHasDirectory(db)
	if err != nil {
		return false, err
	}
	openCodeSessionSchemaCacheMu.Lock()
	openCodeSessionSchemaCache[dbPath] = openCodeSessionSchemaCacheEntry{
		state:        state,
		hasDirectory: hasDirectory,
	}
	openCodeSessionSchemaCacheMu.Unlock()
	return hasDirectory, nil
}

func openCodeSessionTableHasDirectory(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(session)`)
	if err != nil {
		return false, fmt.Errorf(
			"listing opencode session table info: %w", err,
		)
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
			return false, fmt.Errorf(
				"scanning opencode session table info: %w", err,
			)
		}
		if strings.EqualFold(name, "directory") {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func loadOneOpenCodeSession(
	db *sql.DB, sessionID string, hasDirectory bool,
) (openCodeSessionRow, error) {
	var (
		row *sql.Row
		s   openCodeSessionRow
		err error
	)
	if hasDirectory {
		row = db.QueryRow(`
			SELECT s.id, s.project_id,
			       COALESCE(s.parent_id, ''),
			       COALESCE(s.title, ''),
			       COALESCE(s.directory, ''),
			       s.time_created, s.time_updated
			FROM session s
			WHERE s.id = ?
		`, sessionID)
		err = row.Scan(
			&s.id, &s.projectID, &s.parentID,
			&s.title, &s.directory,
			&s.timeCreated, &s.timeUpdated,
		)
		return s, err
	}

	// Legacy OpenCode-family schemas omit session.directory; cwd falls
	// back to project.worktree via resolveOpenCodeWorktree.
	row = db.QueryRow(`
		SELECT s.id, s.project_id,
		       COALESCE(s.parent_id, ''),
		       COALESCE(s.title, ''),
		       s.time_created, s.time_updated
		FROM session s
		WHERE s.id = ?
	`, sessionID)
	err = row.Scan(
		&s.id, &s.projectID, &s.parentID,
		&s.title, &s.timeCreated, &s.timeUpdated,
	)
	return s, err
}

// openCodeMessageRow is a row from the opencode message table.
// The role is extracted from the JSON data column.
type openCodeMessageRow struct {
	id          string
	data        string
	timeCreated int64
	fileMtime   int64
}

// openCodeMessageData holds the scalar fields we extract from
// the message data JSON blob. Token usage lives under `tokens`
// and is read separately via gjson so the parser can
// distinguish explicit zero fields from absent ones.
type openCodeMessageData struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Model      struct {
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
}

// openCodePartRow is a row from the opencode part table.
// The part type is extracted from the JSON data column.
type openCodePartRow struct {
	id          string
	messageID   string
	data        string
	timeCreated int64
	fileMtime   int64
}

type openCodeStorageFingerprint struct {
	Session  *openCodeStorageFingerprintSession  `json:"session,omitempty"`
	Messages []openCodeStorageFingerprintMessage `json:"messages"`
}

type openCodeStorageFingerprintSession struct {
	ID          string `json:"id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Directory   string `json:"directory,omitempty"`
	Worktree    string `json:"worktree,omitempty"`
	TimeCreated int64  `json:"time_created,omitempty"`
	TimeUpdated int64  `json:"time_updated,omitempty"`
}

type openCodeStorageFingerprintMessage struct {
	ID    string                           `json:"id"`
	Time  int64                            `json:"time"`
	Hash  string                           `json:"hash,omitempty"`
	Parts []openCodeStorageFingerprintPart `json:"parts,omitempty"`
}

type openCodeStorageFingerprintPart struct {
	ID   string `json:"id"`
	Time int64  `json:"time"`
	Hash string `json:"hash,omitempty"`
}

func loadOpenCodeMessages(
	db *sql.DB, sessionID string,
) ([]openCodeMessageRow, error) {
	rows, err := db.Query(`
		SELECT id, data, time_created
		FROM message
		WHERE session_id = ?
		ORDER BY time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []openCodeMessageRow
	for rows.Next() {
		var m openCodeMessageRow
		if err := rows.Scan(
			&m.id, &m.data, &m.timeCreated,
		); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func loadOpenCodeParts(
	db *sql.DB, sessionID string,
) (map[string][]openCodePartRow, error) {
	rows, err := db.Query(`
		SELECT p.id, p.message_id,
		       COALESCE(p.data, '{}'),
		       p.time_created
		FROM part p
		WHERE p.session_id = ?
		ORDER BY p.time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	parts := make(map[string][]openCodePartRow)
	for rows.Next() {
		var p openCodePartRow
		if err := rows.Scan(
			&p.id, &p.messageID,
			&p.data, &p.timeCreated,
		); err != nil {
			return nil, err
		}
		parts[p.messageID] = append(
			parts[p.messageID], p,
		)
	}
	return parts, rows.Err()
}

func buildOpenCodeSession(
	db *sql.DB,
	s openCodeSessionRow,
	cwd, projectWorktree, dbPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	msgs, err := loadOpenCodeMessages(db, s.id)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading messages for %s: %w", s.id, err,
		)
	}

	parts, err := loadOpenCodeParts(db, s.id)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading parts for %s: %w", s.id, err,
		)
	}

	sess, parsed, err := buildOpenCodeParsedSession(
		s,
		cwd,
		projectWorktree,
		dbPath+"#"+s.id,
		s.timeUpdated*1_000_000,
		machine,
		msgs,
		parts,
	)
	if err != nil || sess == nil {
		return sess, parsed, err
	}
	sess.File.Hash = buildOpenCodeSessionFingerprint(
		s, cwd, projectWorktree, msgs, parts,
	)
	return sess, parsed, nil
}

func buildOpenCodeParsedSession(
	s openCodeSessionRow,
	cwd, projectWorktree, filePath string,
	fileMtime int64,
	machine string,
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) (*ParsedSession, []ParsedMessage, error) {

	var (
		parsed       []ParsedMessage
		firstMsg     string
		hasUserOrAst bool
		ordinal      int
	)

	// Prefer OpenCode's LLM-generated title when available.
	// Skip default placeholders that match OpenCode's exact
	// format: "New session - " or "Child session - " followed
	// by an ISO-8601 timestamp.
	if s.title != "" && !isOpenCodeDefaultTitle(s.title) {
		firstMsg = truncate(s.title, 300)
	}

	for _, m := range msgs {
		var md openCodeMessageData
		if json.Unmarshal([]byte(m.data), &md) != nil {
			continue
		}
		role := normalizeOpenCodeRole(md.Role)
		if role == "" {
			continue
		}
		hasUserOrAst = true

		msgParts := parts[m.id]
		sort.Slice(msgParts, func(a, b int) bool {
			if msgParts[a].timeCreated ==
				msgParts[b].timeCreated {
				return msgParts[a].id < msgParts[b].id
			}
			return msgParts[a].timeCreated <
				msgParts[b].timeCreated
		})

		pm := buildOpenCodeMessage(
			ordinal, role, m.timeCreated, msgParts, cwd,
		)
		applyOpenCodeTokenUsage(&pm, md, m.data, msgParts)
		if strings.TrimSpace(pm.Content) == "" &&
			!pm.HasToolUse {
			continue
		}

		if role == RoleUser && firstMsg == "" {
			firstMsg = truncate(
				strings.ReplaceAll(pm.Content, "\n", " "),
				300,
			)
		}

		parsed = append(parsed, pm)
		ordinal++
	}

	if !hasUserOrAst || len(parsed) == 0 {
		return nil, nil, nil
	}

	project := ExtractProjectFromCwd(projectWorktree)
	if project == "" {
		project = "unknown"
	}

	parentID := ""
	if s.parentID != "" {
		parentID = "opencode:" + s.parentID
	}

	startedAt := millisToTime(s.timeCreated)
	endedAt := millisToTime(s.timeUpdated)

	userCount := 0
	for _, m := range parsed {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               "opencode:" + s.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenCode,
		Cwd:              cwd,
		ParentSessionID:  parentID,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsed),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  filePath,
			Mtime: fileMtime,
		},
	}

	accumulateMessageTokenUsage(sess, parsed)

	return sess, parsed, nil
}

// applyOpenCodeTokenUsage copies the assistant message's model
// id and per-message token counts into pm so the usage
// dashboard can attribute cost. OpenCode's token field names
// use a nested `cache.{read,write}` shape; this maps them onto
// the agentsview-native `cache_{read,creation}_input_tokens`
// keys that internal/db/usage.go expects.
//
// Coverage semantics match the claude parser contract: a field
// that is present at zero is preserved as "known zero" and
// sets its coverage flag, while a tokens object with no
// recognized fields (empty `{}` or a foreign schema) leaves
// TokenUsage empty so the usage query filter skips the row.
func applyOpenCodeTokenUsage(
	pm *ParsedMessage,
	md openCodeMessageData,
	dataRaw string,
	parts []openCodePartRow,
) {
	if md.ModelID != "" {
		pm.Model = md.ModelID
	} else if md.Model.ModelID != "" {
		pm.Model = md.Model.ModelID
	}
	raws := []string{dataRaw}
	for _, part := range parts {
		if extractOpenCodePartType(part.data) == "step-finish" {
			raws = append(raws, part.data)
		}
	}
	fields, ok := collectOpenCodeTokenFields(raws...)
	if !ok {
		return
	}

	normalized := map[string]int{
		"input_tokens":                fields.input,
		"output_tokens":               fields.output,
		"cache_read_input_tokens":     fields.cacheRead,
		"cache_creation_input_tokens": fields.cacheCreate,
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	pm.TokenUsage = j
	pm.OutputTokens = fields.output
	pm.HasOutputTokens = fields.hasOutput
	pm.ContextTokens = fields.input +
		fields.cacheRead + fields.cacheCreate
	pm.HasContextTokens = fields.hasInput ||
		fields.hasCacheRead || fields.hasCacheCreate
}

type openCodeTokenFields struct {
	input          int
	output         int
	cacheRead      int
	cacheCreate    int
	hasInput       bool
	hasOutput      bool
	hasCacheRead   bool
	hasCacheCreate bool
}

func collectOpenCodeTokenFields(
	raws ...string,
) (openCodeTokenFields, bool) {
	var (
		fields openCodeTokenFields
		any    bool
	)

	for _, raw := range raws {
		tokens := gjson.Get(raw, "tokens")
		if !tokens.Exists() {
			continue
		}
		if field := tokens.Get("input"); field.Exists() {
			fields.input = int(field.Int())
			fields.hasInput = true
			any = true
		}
		if field := tokens.Get("output"); field.Exists() {
			fields.output = int(field.Int())
			fields.hasOutput = true
			any = true
		}
		if field := tokens.Get("cache.read"); field.Exists() {
			fields.cacheRead = int(field.Int())
			fields.hasCacheRead = true
			any = true
		}
		if field := tokens.Get("cache.write"); field.Exists() {
			fields.cacheCreate = int(field.Int())
			fields.hasCacheCreate = true
			any = true
		}
	}

	return fields, any
}

// openCodeDefaultTitleRe matches the exact placeholder format
// OpenCode uses before the LLM generates a real title:
// "New session - 2026-03-22T10:00:00.000Z" or
// "Child session - 2026-03-22T10:00:00.000Z".
var openCodeDefaultTitleRe = regexp.MustCompile(
	`^(New session|Child session) - \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`,
)

func isOpenCodeDefaultTitle(title string) bool {
	return openCodeDefaultTitleRe.MatchString(title)
}

func normalizeOpenCodeRole(role string) RoleType {
	switch role {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	default:
		return ""
	}
}

func buildOpenCodeMessage(
	ordinal int,
	role RoleType,
	timeCreatedMs int64,
	parts []openCodePartRow,
	cwd string,
) ParsedMessage {
	var (
		texts       []string
		toolCalls   []ParsedToolCall
		hasThinking bool
		hasToolUse  bool
	)

	for _, p := range parts {
		partType := extractOpenCodePartType(p.data)
		switch partType {
		case "text":
			text := extractOpenCodeText(p.data)
			if text != "" {
				texts = append(texts, text)
			}
		case "tool":
			hasToolUse = true
			tc := extractOpenCodeToolCall(p.data, cwd)
			if tc.ToolName != "" {
				toolCalls = append(toolCalls, tc)
			}
		case "reasoning":
			text := extractOpenCodeText(p.data)
			if text != "" {
				hasThinking = true
				texts = append(texts,
					"[Thinking]\n"+text+"\n[/Thinking]")
			}
		}
		// skip step-start, step-finish, patch, etc.
	}

	content := strings.Join(texts, "\n")
	return ParsedMessage{
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		Timestamp:     millisToTime(timeCreatedMs),
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
	}
}

// openCodePartTypeData extracts just the type from a part's
// JSON data blob.
type openCodePartTypeData struct {
	Type string `json:"type"`
}

func extractOpenCodePartType(data string) string {
	var d openCodePartTypeData
	if json.Unmarshal([]byte(data), &d) != nil {
		return ""
	}
	return d.Type
}

// openCodeTextData is the JSON structure for a text part's data.
type openCodeTextData struct {
	Content string `json:"content"`
	Text    string `json:"text"`
}

func extractOpenCodeText(data string) string {
	var d openCodeTextData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return ""
	}
	if d.Content != "" {
		return d.Content
	}
	return d.Text
}

// openCodeToolData is the JSON structure for a tool part's data.
type openCodeToolData struct {
	ToolName string          `json:"tool"`
	CallID   string          `json:"callID"`
	State    json.RawMessage `json:"state"`
}

// openCodeToolState holds the nested state of a tool call.
type openCodeToolState struct {
	Input    json.RawMessage `json:"input"`
	Metadata json.RawMessage `json:"metadata"`
}

// openCodeToolMetadata holds the optional metadata from a tool state.
type openCodeToolMetadata struct {
	Exit int `json:"exit"`
}

func extractOpenCodeToolCall(data, cwd string) ParsedToolCall {
	var d openCodeToolData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return ParsedToolCall{}
	}

	var (
		inputJSON  string
		isFailure  bool
	)
	if len(d.State) > 0 {
		var state openCodeToolState
		if err := json.Unmarshal(d.State, &state); err == nil {
			if len(state.Input) > 0 {
				inputJSON = string(state.Input)
			}
			if len(state.Metadata) > 0 {
				var m openCodeToolMetadata
				if err := json.Unmarshal(state.Metadata, &m); err == nil && m.Exit > 0 {
					isFailure = true
				}
			}
		}
	}

	if d.ToolName == "invalid" {
		isFailure = true
	}

	var skillName string
	switch d.ToolName {
	case "skill":
		skillName = gjson.Get(inputJSON, "skill").Str
		if skillName == "" {
			skillName = gjson.Get(inputJSON, "name").Str
		}
	default:
		skillName = inferOpenCodeSkillName(d.ToolName, inputJSON, cwd)
	}

	tc := ParsedToolCall{
		ToolUseID: d.CallID,
		ToolName:  d.ToolName,
		Category:  NormalizeToolCategory(d.ToolName),
		InputJSON: inputJSON,
		SkillName: skillName,
	}

	if isFailure {
		tc.ResultEvents = append(tc.ResultEvents, ParsedToolResultEvent{
			ToolUseID: d.CallID,
			Status:    "errored",
		})
	}

	return tc
}

func inferOpenCodeSkillName(toolName, inputJSON, cwd string) string {
	if isCursorSkillReadTool(toolName) {
		// OpenCode's read-tool input carries no cwd/workdir key, so
		// inferSkillNameFromJSONPaths can't resolve relative SKILL.md
		// paths and falls back to the parent directory name. Try the
		// file_path directly against the session worktree first.
		if fp := gjson.Get(inputJSON, "file_path").Str; fp != "" && cwd != "" {
			if name := skillNameFromPath(fp, cwd); name != "" {
				return name
			}
		}
		return inferSkillNameFromJSONPaths(inputJSON)
	}
	return inferCodexSkillNameWithBase(toolName, inputJSON, cwd)
}

type openCodeStorageTime struct {
	Created int64 `json:"created"`
	Start   int64 `json:"start"`
	End     int64 `json:"end"`
	Updated int64 `json:"updated"`
}

func (t openCodeStorageTime) messageSortTime() int64 {
	switch {
	case t.Created != 0:
		return t.Created
	case t.Start != 0:
		return t.Start
	case t.End != 0:
		return t.End
	default:
		return t.Updated
	}
}

func (t openCodeStorageTime) partSortTime() int64 {
	switch {
	case t.Start != 0:
		return t.Start
	case t.Created != 0:
		return t.Created
	case t.End != 0:
		return t.End
	default:
		return t.Updated
	}
}

type openCodeStorageSessionFile struct {
	ID        string              `json:"id"`
	Directory string              `json:"directory"`
	ParentID  string              `json:"parentID"`
	Title     string              `json:"title"`
	Time      openCodeStorageTime `json:"time"`
}

type openCodeStorageMessageFile struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Model      struct {
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time openCodeStorageTime `json:"time"`
}

type openCodeStoragePartFile struct {
	ID        string              `json:"id"`
	SessionID string              `json:"sessionID"`
	MessageID string              `json:"messageID"`
	Time      openCodeStorageTime `json:"time"`
}

func loadOpenCodeStorageMessages(
	root, sessionID string,
) ([]openCodeMessageRow, error) {
	dir := filepath.Join(root, "storage", "message", sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"reading opencode message dir %s: %w", dir, err,
		)
	}

	var msgs []openCodeMessageRow
	for _, entry := range entries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf(
				"reading opencode message file %s: %w",
				path, err,
			)
		}
		var mf openCodeStorageMessageFile
		if err := json.Unmarshal(raw, &mf); err != nil {
			return nil, fmt.Errorf(
				"decoding opencode message file %s: %w",
				path, err,
			)
		}
		if mf.ID == "" {
			return nil, fmt.Errorf(
				"opencode message file %s missing id",
				path,
			)
		}
		msgs = append(msgs, openCodeMessageRow{
			id:          mf.ID,
			data:        string(raw),
			timeCreated: mf.Time.messageSortTime(),
			fileMtime:   mustEntryMtime(entry),
		})
	}

	sort.Slice(msgs, func(i, j int) bool {
		if msgs[i].timeCreated == msgs[j].timeCreated {
			return msgs[i].id < msgs[j].id
		}
		return msgs[i].timeCreated < msgs[j].timeCreated
	})
	return msgs, nil
}

func loadOpenCodeStorageParts(
	root string, msgs []openCodeMessageRow,
) (map[string][]openCodePartRow, error) {
	parts := make(map[string][]openCodePartRow, len(msgs))
	for _, msg := range msgs {
		dir := filepath.Join(root, "storage", "part", msg.id)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf(
				"reading opencode part dir %s: %w", dir, err,
			)
		}
		for _, entry := range entries {
			if entry.IsDir() ||
				!strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf(
					"reading opencode part file %s: %w",
					path, err,
				)
			}
			var pf openCodeStoragePartFile
			if err := json.Unmarshal(raw, &pf); err != nil {
				return nil, fmt.Errorf(
					"decoding opencode part file %s: %w",
					path, err,
				)
			}
			if pf.ID == "" {
				return nil, fmt.Errorf(
					"opencode part file %s missing id",
					path,
				)
			}
			if pf.MessageID == "" {
				pf.MessageID = msg.id
			}
			parts[msg.id] = append(parts[msg.id], openCodePartRow{
				id:          pf.ID,
				messageID:   pf.MessageID,
				data:        string(raw),
				timeCreated: pf.Time.partSortTime(),
				fileMtime:   mustEntryMtime(entry),
			})
		}
	}
	return parts, nil
}

// OpenCodeSourceMtime returns a composite mtime for either an
// OpenCode storage session JSON path or a legacy SQLite virtual
// path in the form opencode.db#<sessionID>.
func OpenCodeSourceMtime(sourcePath string) (int64, error) {
	if sourcePath == "" {
		return 0, nil
	}
	if dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(
		openCodeFmt.dbName, sourcePath,
	); ok {
		return openCodeSQLiteSessionMtime(dbPath, sessionID)
	}
	return openCodeStorageSessionMtime(sourcePath)
}

func OpenCodeStorageFingerprintMissing(
	storedHash, currentHash string,
) bool {
	stored, ok := decodeOpenCodeStorageFingerprint(storedHash)
	if !ok {
		return false
	}
	current, ok := decodeOpenCodeStorageFingerprint(currentHash)
	if !ok {
		return false
	}
	if stored.Session != nil {
		if current.Session == nil ||
			*current.Session != *stored.Session {
			return true
		}
	}

	currentMsgs := make(map[string]openCodeStorageFingerprintMessage, len(current.Messages))
	for _, msg := range current.Messages {
		currentMsgs[msg.ID] = msg
	}
	for _, storedMsg := range stored.Messages {
		currentMsg, ok := currentMsgs[storedMsg.ID]
		if !ok || currentMsg.Time < storedMsg.Time ||
			currentMsg.Hash != storedMsg.Hash {
			return true
		}
		currentParts := make(map[string]openCodeStorageFingerprintPart, len(currentMsg.Parts))
		for _, part := range currentMsg.Parts {
			currentParts[part.ID] = part
		}
		for _, storedPart := range storedMsg.Parts {
			currentPart, ok := currentParts[storedPart.ID]
			if !ok || currentPart.Time < storedPart.Time ||
				currentPart.Hash != storedPart.Hash {
				return true
			}
		}
	}
	return false
}

func HasOpenCodeStorageFingerprint(hash string) bool {
	return strings.HasPrefix(hash, openCodeStorageFingerprintPrefix)
}

func buildOpenCodeStorageFingerprint(
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) string {
	return buildOpenCodeStorageFingerprintWithSession(nil, msgs, parts)
}

func buildOpenCodeSessionFingerprint(
	s openCodeSessionRow,
	directory, worktree string,
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) string {
	return buildOpenCodeStorageFingerprintWithSession(
		&openCodeStorageFingerprintSession{
			ID:          s.id,
			ProjectID:   s.projectID,
			ParentID:    s.parentID,
			Title:       s.title,
			Directory:   directory,
			Worktree:    worktree,
			TimeCreated: s.timeCreated,
			TimeUpdated: s.timeUpdated,
		},
		msgs,
		parts,
	)
}

func buildOpenCodeStorageFingerprintWithSession(
	session *openCodeStorageFingerprintSession,
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) string {
	fp := openCodeStorageFingerprint{
		Session: session,
		Messages: make(
			[]openCodeStorageFingerprintMessage,
			0, len(msgs),
		),
	}
	for _, msg := range msgs {
		partRows := append([]openCodePartRow(nil), parts[msg.id]...)
		sort.Slice(partRows, func(i, j int) bool {
			if partRows[i].timeCreated == partRows[j].timeCreated {
				return partRows[i].id < partRows[j].id
			}
			return partRows[i].timeCreated < partRows[j].timeCreated
		})
		fpMsg := openCodeStorageFingerprintMessage{
			ID:   msg.id,
			Time: msg.timeCreated,
			Hash: openCodeStorageFingerprintHash(msg.data),
		}
		for _, part := range partRows {
			fpMsg.Parts = append(fpMsg.Parts,
				openCodeStorageFingerprintPart{
					ID:   part.id,
					Time: part.timeCreated,
					Hash: openCodeStorageFingerprintHash(part.data),
				},
			)
		}
		fp.Messages = append(fp.Messages, fpMsg)
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return ""
	}
	return openCodeStorageFingerprintPrefix + string(raw)
}

func decodeOpenCodeStorageFingerprint(
	hash string,
) (openCodeStorageFingerprint, bool) {
	if !strings.HasPrefix(hash, openCodeStorageFingerprintPrefix) {
		return openCodeStorageFingerprint{}, false
	}
	raw := strings.TrimPrefix(hash, openCodeStorageFingerprintPrefix)
	var fp openCodeStorageFingerprint
	if err := json.Unmarshal([]byte(raw), &fp); err != nil {
		return openCodeStorageFingerprint{}, false
	}
	return fp, true
}

func openCodeStorageFingerprintHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

func openCodeSQLiteSessionMtime(
	dbPath, sessionID string,
) (int64, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"stat opencode db %s: %w", dbPath, err,
		)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	row := db.QueryRow(
		"SELECT time_updated FROM session WHERE id = ?",
		sessionID,
	)
	var timeUpdated int64
	if err := row.Scan(&timeUpdated); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"loading opencode session mtime %s#%s: %w",
			dbPath, sessionID, err,
		)
	}
	return timeUpdated * 1_000_000, nil
}

func openCodeStorageSessionMtime(
	sessionPath string,
) (int64, error) {
	info, err := os.Stat(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"stat opencode session file %s: %w",
			sessionPath, err,
		)
	}

	root := filepath.Dir(filepath.Dir(filepath.Dir(
		filepath.Dir(sessionPath),
	)))
	messageRoot := filepath.Join(root, "storage", "message")
	partRoot := filepath.Join(root, "storage", "part")
	sessionID := strings.TrimSuffix(
		filepath.Base(sessionPath), filepath.Ext(sessionPath),
	)
	fileMtime := info.ModTime().UnixNano()

	messageDir := filepath.Join(root, "storage", "message", sessionID)
	fileMtime = max(fileMtime, statMtime(messageDir))
	msgEntries, err := os.ReadDir(messageDir)
	if err != nil {
		if os.IsNotExist(err) {
			fileMtime = max(fileMtime, statMtime(messageRoot))
			return fileMtime, nil
		}
		return 0, fmt.Errorf(
			"reading opencode message dir %s: %w",
			messageDir, err,
		)
	}
	for _, entry := range msgEntries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fileMtime = max(fileMtime, mustEntryMtime(entry))
		messageID := strings.TrimSuffix(
			entry.Name(), filepath.Ext(entry.Name()),
		)
		partDir := filepath.Join(root, "storage", "part", messageID)
		fileMtime = max(fileMtime, statMtime(partDir))
		partEntries, err := os.ReadDir(partDir)
		if err != nil {
			if os.IsNotExist(err) {
				fileMtime = max(fileMtime, statMtime(partRoot))
				continue
			}
			return 0, fmt.Errorf(
				"reading opencode part dir %s: %w",
				partDir, err,
			)
		}
		for _, partEntry := range partEntries {
			if partEntry.IsDir() ||
				!strings.HasSuffix(partEntry.Name(), ".json") {
				continue
			}
			fileMtime = max(fileMtime, mustEntryMtime(partEntry))
		}
	}

	return fileMtime, nil
}

func mustEntryMtime(entry os.DirEntry) int64 {
	info, err := entry.Info()
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func statMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func millisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
