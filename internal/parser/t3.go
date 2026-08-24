package parser

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	t3IDPrefix = "t3:"
	t3DBName   = "state.sqlite"
)

// t3code is an event-sourced desktop agent: an append-only event log is folded
// into `projection_*` read models, and those projections are what a reader can
// interpret without replaying the log. The conversation lives in
// projection_threads (one row per thread) and projection_thread_messages (the
// visible user/assistant text), with projection_projects supplying the
// workspace root and projection_thread_sessions the backing provider.
//
// Every thread shares one database, so t3 is a multi-session container
// provider: discovery surfaces userdata/state.sqlite once and Parse fans it
// out into one session per thread, addressed by "<db>#<threadID>".

// T3VirtualPath builds the virtual source path identifying one thread inside
// the shared database.
func T3VirtualPath(dbPath, threadID string) string {
	return dbPath + "#" + threadID
}

// parseT3VirtualPath splits a t3 virtual source path back into its physical
// state.sqlite path and raw thread ID.
func parseT3VirtualPath(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, t3DBName)
}

// OpenT3DB opens the shared t3 database read-only.
func OpenT3DB(dbPath string) (*sql.DB, error) {
	return openT3DB(dbPath)
}

func openT3DB(dbPath string) (*sql.DB, error) {
	// immutable=0 because t3 writes continuously while it runs; a read-only
	// WAL reader sees the last committed state without blocking the writer.
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=0&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening t3 db %s: %w", dbPath, err)
	}
	return db, nil
}

// T3ThreadExists reports whether the database still holds a live (undeleted)
// thread with the given ID.
func T3ThreadExists(dbPath, threadID string) bool {
	if dbPath == "" || threadID == "" || !IsRegularFile(dbPath) {
		return false
	}
	conn, err := openT3DB(dbPath)
	if err != nil {
		return false
	}
	defer conn.Close()

	var found int
	err = conn.QueryRow(
		`SELECT 1 FROM projection_threads
		  WHERE thread_id = ? AND deleted_at IS NULL LIMIT 1`,
		threadID,
	).Scan(&found)
	return err == nil
}

// T3ThreadMeta holds the per-thread change signal used by the sync engine to
// decide whether a thread needs reparsing.
type T3ThreadMeta struct {
	RawID       string
	VirtualPath string
	// FileMtime is the per-thread change token as a nanosecond timestamp: the
	// latest of the thread's, its messages', and its project's created/updated
	// timestamps. Message updated_at is included so an in-place edit advances
	// the token, and the project timestamps are included because
	// workspace_root determines the session's cwd and project. The parse path
	// persists the identical token as File.Mtime (see buildT3ParseResult); if
	// the two ever diverged, a reparse triggered by the fingerprint would be
	// discarded as unchanged by the mtime comparison. t3 writes ISO-8601
	// timestamps with millisecond precision, so unlike Shelley no content
	// digest is needed to separate two writes in the same second.
	FileMtime int64
}

// ListT3ThreadMetas collects every live thread's change signal on an
// already-open connection.
func ListT3ThreadMetas(conn *sql.DB, dbPath string) ([]T3ThreadMeta, error) {
	var metas []T3ThreadMeta
	err := ForEachT3ThreadMeta(
		context.Background(), conn, dbPath,
		func(meta T3ThreadMeta) error {
			metas = append(metas, meta)
			return nil
		},
	)
	return metas, err
}

// ForEachT3ThreadMeta streams every live thread's change signal.
func ForEachT3ThreadMeta(
	ctx context.Context, conn *sql.DB, dbPath string,
	yield func(T3ThreadMeta) error,
) error {
	shape, err := inspectT3Schema(ctx, conn)
	if err != nil {
		return err
	}
	return forEachT3ThreadMetaQuery(ctx, conn, shape, dbPath, "", nil, yield)
}

// T3ThreadMetaByID resolves one thread's change signal.
func T3ThreadMetaByID(
	ctx context.Context, conn *sql.DB, dbPath, threadID string,
) (T3ThreadMeta, bool, error) {
	shape, err := inspectT3Schema(ctx, conn)
	if err != nil {
		return T3ThreadMeta{}, false, err
	}
	var (
		meta  T3ThreadMeta
		found bool
	)
	err = forEachT3ThreadMetaQuery(
		ctx, conn, shape, dbPath, "AND t.thread_id = ?", []any{threadID},
		func(m T3ThreadMeta) error {
			meta, found = m, true
			return nil
		},
	)
	return meta, found, err
}

func forEachT3ThreadMetaQuery(
	ctx context.Context, conn *sql.DB, shape t3Schema,
	dbPath, extraWhere string, args []any,
	yield func(T3ThreadMeta) error,
) error {
	// Only timestamps are read here, never message text, which keeps the scan
	// cheap enough to run on every reconcile. The aggregated stamps feed
	// t3LatestNanos exactly as the parse path does, so both compute one token.
	query := `SELECT t.thread_id,
	                 COALESCE(t.updated_at, ''),
	                 COALESCE(t.created_at, ''),
	                 COALESCE(MAX(m.created_at), ''),
	                 COALESCE(MAX(` + shape.messageUpdatedExpr("m.") + `), ''),
	                 MAX(` + shape.projectUpdatedExpr() + `),
	                 MAX(` + shape.projectCreatedExpr() + `)
	            FROM projection_threads t
	            LEFT JOIN projection_thread_messages m
	                   ON m.thread_id = t.thread_id` + shape.projectJoin() + `
	           WHERE t.deleted_at IS NULL ` + extraWhere + `
	           GROUP BY t.thread_id
	           ORDER BY t.thread_id`
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("listing t3 threads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var threadID string
		var stamps [6]string
		if err := rows.Scan(&threadID, &stamps[0], &stamps[1], &stamps[2],
			&stamps[3], &stamps[4], &stamps[5]); err != nil {
			return fmt.Errorf("scanning t3 thread meta: %w", err)
		}
		if !IsValidSessionID(threadID) {
			continue
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(T3ThreadMeta{
			RawID:       threadID,
			VirtualPath: T3VirtualPath(dbPath, threadID),
			FileMtime:   t3LatestNanos(stamps[:]...),
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// t3LatestTime returns the latest of the given ISO-8601 timestamps, or the
// zero time when none parses.
func t3LatestTime(values ...string) time.Time {
	var latest time.Time
	for _, v := range values {
		if ts := parseTimestamp(v); !ts.IsZero() && ts.After(latest) {
			latest = ts
		}
	}
	return latest
}

// t3LatestNanos returns the latest of the given ISO-8601 timestamps as Unix
// nanoseconds, or 0 when none parses.
func t3LatestNanos(values ...string) int64 {
	latest := t3LatestTime(values...)
	if latest.IsZero() {
		return 0
	}
	return latest.UnixNano()
}

// T3SourceMtime resolves the per-thread change signal for a virtual t3 source
// path. The live watcher compares it for inequality and treats a zero or error
// result as "source gone".
func T3SourceMtime(path string) (int64, error) {
	return T3SourceMtimeContext(context.Background(), path)
}

// T3SourceMtimeContext is T3SourceMtime with caller-supplied cancellation.
func T3SourceMtimeContext(ctx context.Context, path string) (int64, error) {
	dbPath, threadID, ok := parseT3VirtualPath(path)
	if !ok {
		return 0, fmt.Errorf("not a t3 virtual path: %s", path)
	}
	conn, err := openT3DB(dbPath)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	meta, found, err := T3ThreadMetaByID(ctx, conn, dbPath, threadID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return meta.FileMtime, nil
}

// t3Schema records which optional columns this database generation has. The
// projection tables are migrated in place and old columns are dropped, so a
// reader that hard-codes the current shape fails outright on an older install:
// model_selection_json replaced a plain model column, and attachments_json and
// provider_instance_id were both added later. Absent columns degrade to empty
// expressions rather than an error.
type t3Schema struct {
	hasModelSelectionJSON bool
	hasLegacyModel        bool
	hasAttachments        bool
	hasMessageUpdatedAt   bool
	hasProviderInstanceID bool
	hasProjectsTable      bool
	hasProjectUpdatedAt   bool
	hasProjectCreatedAt   bool
}

func (s t3Schema) modelSelectionExpr() string {
	if s.hasModelSelectionJSON {
		return `COALESCE(t.model_selection_json, '')`
	}
	return `''`
}

func (s t3Schema) legacyModelExpr() string {
	if s.hasLegacyModel {
		return `COALESCE(t.model, '')`
	}
	return `''`
}

func (s t3Schema) attachmentsExpr() string {
	if s.hasAttachments {
		return `COALESCE(attachments_json, '')`
	}
	return `''`
}

// messageUpdatedExpr qualifies the column with table when the caller joins
// multiple tables that all carry an updated_at.
func (s t3Schema) messageUpdatedExpr(table string) string {
	if s.hasMessageUpdatedAt {
		return `COALESCE(` + table + `updated_at, '')`
	}
	return `''`
}

func (s t3Schema) providerInstanceExpr() string {
	if s.hasProviderInstanceID {
		return `COALESCE(s.provider_instance_id, '')`
	}
	return `''`
}

// projectJoin returns the projection_projects LEFT JOIN, or "" when the table
// is absent so a query never references a table that does not exist.
func (s t3Schema) projectJoin() string {
	if s.hasProjectsTable {
		return ` LEFT JOIN projection_projects p ON p.project_id = t.project_id`
	}
	return ``
}

func (s t3Schema) workspaceRootExpr() string {
	if s.hasProjectsTable {
		return `COALESCE(p.workspace_root, '')`
	}
	return `''`
}

func (s t3Schema) projectUpdatedExpr() string {
	if s.hasProjectsTable && s.hasProjectUpdatedAt {
		return `COALESCE(p.updated_at, '')`
	}
	return `''`
}

func (s t3Schema) projectCreatedExpr() string {
	if s.hasProjectsTable && s.hasProjectCreatedAt {
		return `COALESCE(p.created_at, '')`
	}
	return `''`
}

// inspectT3Schema validates the columns every generation must have and records
// the optional ones.
func inspectT3Schema(ctx context.Context, conn *sql.DB) (t3Schema, error) {
	var shape t3Schema
	threadColumns, err := t3TableColumns(ctx, conn, "projection_threads")
	if err != nil {
		return shape, err
	}
	if len(threadColumns) == 0 {
		return shape, fmt.Errorf("missing t3 projection_threads table")
	}
	for _, required := range []string{
		"thread_id", "title", "created_at", "updated_at", "deleted_at",
	} {
		if !threadColumns[required] {
			return shape, fmt.Errorf(
				"missing required t3 projection_threads column %s", required,
			)
		}
	}
	shape.hasModelSelectionJSON = threadColumns["model_selection_json"]
	shape.hasLegacyModel = threadColumns["model"]

	messageColumns, err := t3TableColumns(ctx, conn, "projection_thread_messages")
	if err != nil {
		return shape, err
	}
	if len(messageColumns) == 0 {
		return shape, fmt.Errorf("missing t3 projection_thread_messages table")
	}
	for _, required := range []string{
		"message_id", "thread_id", "role", "text", "created_at",
	} {
		if !messageColumns[required] {
			return shape, fmt.Errorf(
				"missing required t3 projection_thread_messages column %s", required,
			)
		}
	}
	shape.hasAttachments = messageColumns["attachments_json"]
	shape.hasMessageUpdatedAt = messageColumns["updated_at"]

	sessionColumns, err := t3TableColumns(ctx, conn, "projection_thread_sessions")
	if err != nil {
		return shape, err
	}
	shape.hasProviderInstanceID = sessionColumns["provider_instance_id"]

	projectColumns, err := t3TableColumns(ctx, conn, "projection_projects")
	if err != nil {
		return shape, err
	}
	shape.hasProjectsTable = len(projectColumns) > 0
	shape.hasProjectUpdatedAt = projectColumns["updated_at"]
	shape.hasProjectCreatedAt = projectColumns["created_at"]
	return shape, nil
}

// t3TableColumns returns the column set of a table, or an empty map when the
// table does not exist.
func t3TableColumns(
	ctx context.Context, conn *sql.DB, table string,
) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspecting t3 %s schema: %w", table, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(
			&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey,
		); err != nil {
			return nil, fmt.Errorf("scanning t3 %s schema: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading t3 %s schema: %w", table, err)
	}
	return columns, nil
}

// t3ThreadRow is one thread joined to the metadata a session needs: its
// project's workspace root and the provider backing its runtime.
type t3ThreadRow struct {
	threadID           string
	title              string
	createdAt          string
	updatedAt          string
	branch             string
	worktreePath       string
	modelSelectionJSON string
	legacyModel        string
	workspaceRoot      string
	projectUpdatedAt   string
	projectCreatedAt   string
	providerName       string
	providerInstanceID string
}

func loadT3Thread(
	ctx context.Context, conn *sql.DB, shape t3Schema, threadID string,
) (t3ThreadRow, bool, error) {
	// LEFT JOINs throughout: a thread whose project row was deleted, or which
	// never opened a provider session, is still a real conversation worth
	// indexing -- it just loses the cwd or the model attribution.
	query := `SELECT t.thread_id,
	                 COALESCE(t.title, ''),
	                 COALESCE(t.created_at, ''),
	                 COALESCE(t.updated_at, ''),
	                 COALESCE(t.branch, ''),
	                 COALESCE(t.worktree_path, ''),
	                 ` + shape.modelSelectionExpr() + `,
	                 ` + shape.legacyModelExpr() + `,
	                 ` + shape.workspaceRootExpr() + `,
	                 ` + shape.projectUpdatedExpr() + `,
	                 ` + shape.projectCreatedExpr() + `,
	                 COALESCE(s.provider_name, ''),
	                 ` + shape.providerInstanceExpr() + `
	            FROM projection_threads t` + shape.projectJoin() + `
	            LEFT JOIN projection_thread_sessions s ON s.thread_id = t.thread_id
	           WHERE t.thread_id = ? AND t.deleted_at IS NULL
	           LIMIT 1`
	var row t3ThreadRow
	err := conn.QueryRowContext(ctx, query, threadID).Scan(
		&row.threadID, &row.title, &row.createdAt, &row.updatedAt,
		&row.branch, &row.worktreePath, &row.modelSelectionJSON,
		&row.legacyModel, &row.workspaceRoot, &row.projectUpdatedAt,
		&row.projectCreatedAt, &row.providerName, &row.providerInstanceID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return t3ThreadRow{}, false, nil
		}
		return t3ThreadRow{}, false, fmt.Errorf("loading t3 thread %s: %w", threadID, err)
	}
	return row, true, nil
}

// t3MessageStamps carries the newest created_at and updated_at seen across a
// thread's messages -- all of them, including rows the parser drops as empty,
// because the discovery-side change token aggregates over every row. If the
// parse-side token skipped dropped rows, the two would diverge and the engine
// would discard a legitimately reparsed thread as unchanged.
type t3MessageStamps struct {
	maxCreated string
	maxUpdated string
}

func loadT3Messages(
	ctx context.Context, conn *sql.DB, shape t3Schema, threadID string,
) ([]ParsedMessage, t3MessageStamps, error) {
	// message_id breaks ties: two messages can share a created_at at
	// millisecond precision, and an unstable order would reshuffle the
	// transcript between parses.
	query := `SELECT COALESCE(role, ''), COALESCE(text, ''),
	                 COALESCE(created_at, ''),
	                 ` + shape.messageUpdatedExpr("") + `,
	                 ` + shape.attachmentsExpr() + `
	            FROM projection_thread_messages
	           WHERE thread_id = ?
	           ORDER BY created_at, message_id`
	rows, err := conn.QueryContext(ctx, query, threadID)
	if err != nil {
		return nil, t3MessageStamps{}, fmt.Errorf(
			"loading t3 messages for %s: %w", threadID, err)
	}
	defer rows.Close()

	var messages []ParsedMessage
	var stamps t3MessageStamps
	for rows.Next() {
		var role, text, createdAt, updatedAt, attachments string
		if err := rows.Scan(
			&role, &text, &createdAt, &updatedAt, &attachments,
		); err != nil {
			return nil, t3MessageStamps{}, fmt.Errorf("scanning t3 message: %w", err)
		}
		// ISO-8601 strings in one format order lexicographically, matching the
		// SQL MAX() the discovery query uses.
		if createdAt > stamps.maxCreated {
			stamps.maxCreated = createdAt
		}
		if updatedAt > stamps.maxUpdated {
			stamps.maxUpdated = updatedAt
		}
		content := strings.TrimSpace(text)
		if content == "" && !t3HasAttachments(attachments) {
			continue
		}
		messages = append(messages, ParsedMessage{
			Ordinal:       len(messages),
			Role:          t3Role(role),
			Content:       content,
			ContentLength: len(content),
			Timestamp:     parseTimestamp(createdAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, t3MessageStamps{}, fmt.Errorf(
			"reading t3 messages for %s: %w", threadID, err)
	}
	return messages, stamps, nil
}

// t3HasAttachments keeps an image-only message in the transcript: it carries no
// text but it is still a turn the user took.
func t3HasAttachments(raw string) bool {
	switch strings.TrimSpace(raw) {
	case "", "[]", "null", "{}":
		return false
	}
	return true
}

func t3Role(role string) RoleType {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	case "system":
		return RoleSystem
	case "tool":
		return RoleTool
	default:
		return RoleType(strings.ToLower(strings.TrimSpace(role)))
	}
}

// t3ModelSelection is the per-thread model picker's persisted state. A thread
// backed by a named provider instance records instanceId; one backed by a
// built-in provider records provider instead. Both always carry model.
type t3ModelSelection struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	InstanceID string `json:"instanceId"`
}

func t3ParseModelSelection(raw string) t3ModelSelection {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return t3ModelSelection{}
	}
	var sel t3ModelSelection
	if err := json.Unmarshal([]byte(raw), &sel); err != nil {
		return t3ModelSelection{}
	}
	return sel
}

func parseT3ThreadFromDB(
	ctx context.Context, conn *sql.DB, dbPath, threadID, machine string,
	info os.FileInfo,
) (*ParseResult, error) {
	shape, err := inspectT3Schema(ctx, conn)
	if err != nil {
		return nil, err
	}
	return parseT3ThreadFromDBWithSchema(
		ctx, conn, shape, dbPath, threadID, machine, info,
	)
}

// parseT3ThreadFromDBWithSchema is the container-loop entry point: the schema
// is inspected once per database rather than once per thread.
func parseT3ThreadFromDBWithSchema(
	ctx context.Context, conn *sql.DB, shape t3Schema,
	dbPath, threadID, machine string, info os.FileInfo,
) (*ParseResult, error) {
	if !IsValidSessionID(threadID) {
		return nil, fmt.Errorf("invalid t3 thread ID: %s", threadID)
	}
	row, found, err := loadT3Thread(ctx, conn, shape, threadID)
	if err != nil {
		return nil, err
	}
	if !found {
		// Deleted or never existed. Returning (nil, nil) lets the container
		// parse skip it rather than failing the whole database.
		return nil, nil
	}
	messages, stamps, err := loadT3Messages(ctx, conn, shape, threadID)
	if err != nil {
		return nil, err
	}
	result := buildT3ParseResult(row, messages, stamps, dbPath, info, machine)
	return &result, nil
}

func buildT3ParseResult(
	row t3ThreadRow,
	messages []ParsedMessage,
	stamps t3MessageStamps,
	dbPath string,
	info os.FileInfo,
	machine string,
) ParseResult {
	// Pre-canonicalization databases carry a bare model column instead of the
	// selection blob; either one names the model the thread last ran on.
	model := t3ParseModelSelection(row.modelSelectionJSON).Model
	if model == "" {
		model = strings.TrimSpace(row.legacyModel)
	}
	if model != "" {
		for i := range messages {
			if messages[i].Role == RoleAssistant {
				messages[i].Model = model
			}
		}
	}

	// A worktree thread runs against its own checkout; the project's
	// workspace_root is the repository it was cut from. The worktree is the
	// directory the agent actually worked in, so it wins as the cwd.
	cwd := strings.TrimSpace(row.worktreePath)
	if cwd == "" {
		cwd = strings.TrimSpace(row.workspaceRoot)
	}
	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = "unknown"
	}

	startedAt := parseTimestamp(row.createdAt)
	// A message can carry a timestamp past the thread's updated_at (the
	// defensive case the change token covers), so session activity ends at
	// the latest of the thread's and its messages' stamps. Project stamps
	// stay out: a workspace rename is not conversation activity, so they
	// feed only the change token below.
	endedAt := t3LatestTime(row.updatedAt, stamps.maxCreated, stamps.maxUpdated)
	if startedAt.IsZero() {
		startedAt = endedAt
	} else if endedAt.IsZero() {
		endedAt = startedAt
	}

	var firstMessage string
	var userCount int
	for _, msg := range messages {
		if msg.Role != RoleUser {
			continue
		}
		userCount++
		if firstMessage == "" && strings.TrimSpace(msg.Content) != "" {
			firstMessage = truncate(
				strings.ReplaceAll(msg.Content, "\n", " "), 300,
			)
		}
	}

	// The persisted mtime is the same change token discovery computes (see
	// T3ThreadMeta.FileMtime): built from the identical timestamps, so a
	// fingerprint-triggered reparse is never discarded as unchanged.
	mtime := t3LatestNanos(
		row.updatedAt, row.createdAt,
		stamps.maxUpdated, stamps.maxCreated,
		row.projectUpdatedAt, row.projectCreatedAt,
	)

	sess := ParsedSession{
		ID:               t3IDPrefix + row.threadID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentT3,
		Cwd:              cwd,
		GitBranch:        strings.TrimSpace(row.branch),
		FirstMessage:     firstMessage,
		SessionName:      strings.TrimSpace(row.title),
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  T3VirtualPath(dbPath, row.threadID),
			Size:  info.Size(),
			Mtime: mtime,
		},
	}
	return ParseResult{Session: sess, Messages: messages}
}
