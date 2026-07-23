// ABOUTME: Parser for omnigent (github.com/omnigent-ai/omnigent), an open-source
// ABOUTME: meta-harness, reading its SQLite chat.db (conversations + items).
package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
)

// omnigent orchestrates other coding agents (Claude Code, Codex, OpenCode, ...)
// and persists no per-session files. The canonical transcript is a single
// SQLite DB at {OMNIGENT_DATA_DIR|~/.omnigent}/chat.db. Sessions live in a
// conversations table; every transcript event is one conversation_items row
// whose plaintext-JSON `data` column is shaped by its `type`. `position` is
// unique and monotonic per conversation and maps to the AgentsView Ordinal.
//
// The schema has evolved: older builds store one `conversations` table with
// VARCHAR enum columns; newer builds encode `type`/`kind` as SMALLINT codes and
// split session metadata into `omnigent_conversation_metadata` (and agent
// binding into `agent_configuration`). The `data` JSON shape is identical
// across generations, so a single decoder is wrapped in a schema-shape adapter
// resolved by feature detection (omnigentSchema).
const (
	omnigentIDPrefix = "omnigent:"
	omnigentDBName   = OmnigentDBName
)

// OmnigentDBName is the standalone SQLite database exported by remote sync.
const OmnigentDBName = "chat.db"

// omnigentAgent is the AgentType for omnigent sessions.
const omnigentAgent AgentType = "omnigent"

// omnigent conversation_items.type names (the string form; newer DBs store the
// integer code, mapped back to these via omnigentItemTypeByCode).
const (
	omnigentTypeMessage      = "message"
	omnigentTypeFuncCall     = "function_call"
	omnigentTypeFuncOutput   = "function_call_output"
	omnigentTypeReasoning    = "reasoning"
	omnigentTypeError        = "error"
	omnigentTypeCompaction   = "compaction"
	omnigentTypeNativeTool   = "native_tool"
	omnigentTypeResource     = "resource_event"
	omnigentTypeRouting      = "routing_decision"
	omnigentTypeSlashCommand = "slash_command"
	omnigentTypeTerminal     = "terminal_command"
)

// omnigentItemTypeByCode maps the SMALLINT ITEM_TYPE codes (omnigent
// db/enum_codecs.py, stable and append-only) to their string names.
var omnigentItemTypeByCode = map[int]string{
	1:  omnigentTypeMessage,
	2:  omnigentTypeFuncCall,
	3:  omnigentTypeFuncOutput,
	4:  omnigentTypeReasoning,
	5:  omnigentTypeError,
	6:  omnigentTypeCompaction,
	7:  omnigentTypeNativeTool,
	8:  omnigentTypeResource,
	9:  omnigentTypeRouting,
	10: omnigentTypeSlashCommand,
	11: omnigentTypeTerminal,
}

// omnigent CONVERSATION_KIND codes.
const (
	omnigentKindDefaultCode  = "1"
	omnigentKindSubAgentCode = "2"
	omnigentKindSubAgentName = "sub_agent"
)

// ErrOmnigentUnsupportedSchema is returned when a chat.db carries a schema this
// parser cannot read (e.g. session metadata relocated to a separate physical
// database). The sync layer treats it as a skip, not a hard failure.
type ErrOmnigentUnsupportedSchema struct {
	Reason string
}

func (e ErrOmnigentUnsupportedSchema) Error() string {
	return "omnigent: unsupported schema: " + e.Reason
}

// omnigentSchema captures the on-disk shape resolved by feature detection.
type omnigentSchema struct {
	// splitMetadata is true when session metadata (kind, workspace, git_branch,
	// session_usage, ...) lives in omnigent_conversation_metadata rather than on
	// the conversations table.
	splitMetadata bool
	// intEnums is true when conversation_items.type is a SMALLINT code rather
	// than a VARCHAR name.
	intEnums bool
	// hasAgentConfig is true when agent_configuration (model_override, ...)
	// exists as a separate table (newest generation).
	hasAgentConfig bool
	// hasSessionUsage is true when a session_usage column is readable.
	hasSessionUsage bool
	// binaryIDs is true when id columns hold 16-byte uuid BLOBs (omnigent
	// migration z7a2b3c4d5e6) rather than text. Queries read them as bare
	// lowercase hex, the form omnigent's own app layer presents.
	binaryIDs bool
	// changeIndexName identifies the persistent conversations index used for
	// bounded updated_at discovery.
	changeIndexName string
	// changeIndexArchived is true when archived is between workspace_id and
	// updated_at in the split-schema change index.
	changeIndexArchived bool
}

// omnigentIDExpr yields a SELECT expression reading an id column as text:
// the raw column for text-id generations, lowercase hex for binary-id ones.
func omnigentIDExpr(s omnigentSchema, col string) string {
	if s.binaryIDs {
		return "LOWER(HEX(" + col + "))"
	}
	return col
}

// omnigentIDArg converts a member raw ID to the bind representation of an id
// column. A hex-invalid id against a binary-id database binds as text and
// matches nothing, which retires it as a legacy-generation member.
func omnigentIDArg(s omnigentSchema, rawID string) any {
	if !s.binaryIDs {
		return rawID
	}
	decoded, err := hex.DecodeString(rawID)
	if err != nil {
		return rawID
	}
	return decoded
}

type omnigentMemberID struct {
	workspaceID int64
	rawID       string
}

func omnigentMemberForSchema(s omnigentSchema, value string) (omnigentMemberID, error) {
	if !s.splitMetadata {
		if !IsValidSessionID(value) {
			return omnigentMemberID{}, fmt.Errorf("invalid omnigent session ID: %s", value)
		}
		return omnigentMemberID{rawID: value}, nil
	}
	workspace, rawID, ok := strings.Cut(value, ":")
	if !ok || rawID == "" || !IsValidSessionID(rawID) {
		return omnigentMemberID{}, fmt.Errorf("invalid omnigent member ID: %s", value)
	}
	workspaceID, err := strconv.ParseInt(workspace, 10, 64)
	if err != nil {
		return omnigentMemberID{}, fmt.Errorf("invalid omnigent workspace ID %q: %w", workspace, err)
	}
	return omnigentMemberID{workspaceID: workspaceID, rawID: rawID}, nil
}

func (m omnigentMemberID) key(s omnigentSchema) string {
	if !s.splitMetadata {
		return m.rawID
	}
	return fmt.Sprintf("%d:%s", m.workspaceID, m.rawID)
}

func (m omnigentMemberID) sessionID(s omnigentSchema) string {
	return omnigentIDPrefix + m.key(s)
}

// openOmnigentDB opens chat.db read-only. Callers own Close.
func openOmnigentDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening omnigent db %s: %w", dbPath, err)
	}
	return db, nil
}

// detectOmnigentSchema resolves the on-disk shape. It fails closed with
// ErrOmnigentUnsupportedSchema when the database is not a recognizable omnigent
// store or when session metadata is not co-located in this file.
func detectOmnigentSchema(conn *sql.DB) (omnigentSchema, error) {
	for _, table := range []string{
		"alembic_version", "conversations", "conversation_items",
	} {
		exists, err := omnigentTableExists(conn, table)
		if err != nil {
			return omnigentSchema{}, fmt.Errorf(
				"inspect omnigent table %s: %w", table, err)
		}
		if !exists {
			return omnigentSchema{}, ErrOmnigentUnsupportedSchema{
				Reason: "missing core omnigent tables",
			}
		}
	}

	var s omnigentSchema
	var err error
	s.intEnums, err = omnigentColumnIsInteger(conn, "conversation_items", "type")
	if err != nil {
		return omnigentSchema{}, err
	}
	s.binaryIDs, err = omnigentColumnIsBinary(conn, "conversations", "id")
	if err != nil {
		return omnigentSchema{}, err
	}
	kindColumn, err := omnigentColumnExists(conn, "conversations", "kind")
	if err != nil {
		return omnigentSchema{}, err
	}
	metadataTable, err := omnigentTableExists(conn, "omnigent_conversation_metadata")
	if err != nil {
		return omnigentSchema{}, err
	}

	switch {
	case kindColumn:
		// Older single-table shape: metadata columns live on conversations.
		s.splitMetadata = false
		s.hasSessionUsage, err = omnigentColumnExists(
			conn, "conversations", "session_usage")
	case metadataTable:
		// Split shape: metadata is co-located in this file.
		s.splitMetadata = true
		s.hasAgentConfig, err = omnigentTableExists(conn, "agent_configuration")
		if err == nil {
			s.hasSessionUsage, err = omnigentColumnExists(
				conn, "omnigent_conversation_metadata", "session_usage")
		}
	default:
		// Split shape but metadata not in this database (multi-physical-DB
		// deployment). We cannot recover kind/workspace/usage, so skip.
		return omnigentSchema{}, ErrOmnigentUnsupportedSchema{
			Reason: "session metadata table not present in this database",
		}
	}
	if err != nil {
		return omnigentSchema{}, fmt.Errorf("inspect omnigent schema: %w", err)
	}
	itemPrefix := []string{"conversation_id", "position"}
	if s.splitMetadata {
		itemPrefix = []string{"workspace_id", "conversation_id", "position"}
	}
	itemIndex, err := omnigentIndexWithPrefix(conn, "conversation_items", itemPrefix)
	if err != nil {
		return omnigentSchema{}, fmt.Errorf("inspect omnigent item indexes: %w", err)
	}
	if itemIndex == "" {
		return omnigentSchema{}, ErrOmnigentUnsupportedSchema{
			Reason: "missing bounded conversation item lookup index",
		}
	}

	changePrefix := []string{"updated_at"}
	if s.splitMetadata {
		hasArchived, columnErr := omnigentColumnExists(
			conn, "conversations", "archived",
		)
		if columnErr != nil {
			return omnigentSchema{}, fmt.Errorf(
				"inspect omnigent archived column: %w", columnErr,
			)
		}
		changePrefix = []string{"workspace_id", "updated_at"}
		if hasArchived {
			changePrefix = []string{"workspace_id", "archived", "updated_at"}
			s.changeIndexArchived = true
		}
	}
	name, indexErr := omnigentIndexWithPrefix(
		conn, "conversations", changePrefix,
	)
	if indexErr != nil {
		return omnigentSchema{}, fmt.Errorf(
			"inspect omnigent conversation indexes: %w", indexErr,
		)
	}
	if name == "" {
		return omnigentSchema{}, ErrOmnigentUnsupportedSchema{
			Reason: "missing bounded conversation change index",
		}
	}
	s.changeIndexName = name
	return s, nil
}

func omnigentTableExists(conn *sql.DB, table string) (bool, error) {
	var name string
	err := conn.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
		table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func omnigentColumnExists(conn *sql.DB, table, column string) (bool, error) {
	_, ok, err := omnigentColumnType(conn, table, column)
	return ok, err
}

// omnigentColumnIsInteger reports whether a column's declared type is an
// integer affinity (INTEGER, SMALLINT, ...). Absent columns report false.
func omnigentColumnIsInteger(
	conn *sql.DB, table, column string,
) (bool, error) {
	declType, ok, err := omnigentColumnType(conn, table, column)
	if err != nil || !ok {
		return false, err
	}
	return strings.Contains(strings.ToUpper(declType), "INT"), nil
}

// omnigentColumnIsBinary reports whether a column's declared type is a BLOB
// (the shape omnigent's Uuid16 columns take on SQLite). Absent columns report
// false.
func omnigentColumnIsBinary(
	conn *sql.DB, table, column string,
) (bool, error) {
	declType, ok, err := omnigentColumnType(conn, table, column)
	if err != nil || !ok {
		return false, err
	}
	return strings.Contains(strings.ToUpper(declType), "BLOB"), nil
}

func omnigentColumnType(
	conn *sql.DB, table, column string,
) (string, bool, error) {
	// PRAGMA table_info is not parameterizable; the table name is an internal
	// literal (never user input), so interpolation is safe here.
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			declType   string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(
			&cid, &name, &declType, &notNull, &dfltValue, &primaryKey,
		); err != nil {
			return "", false, err
		}
		if name == column {
			return declType, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

func omnigentIndexWithPrefix(
	conn *sql.DB, table string, prefix []string,
) (string, error) {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA index_list(%q)", table))
	if err != nil {
		return "", err
	}
	var names []string
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return "", err
		}
		if partial == 0 {
			names = append(names, name)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	for _, name := range names {
		indexRows, err := conn.Query(
			fmt.Sprintf("PRAGMA index_info(%q)", name),
		)
		if err != nil {
			return "", err
		}
		columns := make([]string, 0, len(prefix))
		for indexRows.Next() {
			var seq, cid int
			var column sql.NullString
			if err := indexRows.Scan(&seq, &cid, &column); err != nil {
				_ = indexRows.Close()
				return "", err
			}
			columns = append(columns, column.String)
		}
		if err := indexRows.Err(); err != nil {
			_ = indexRows.Close()
			return "", err
		}
		if err := indexRows.Close(); err != nil {
			return "", err
		}
		if len(columns) >= len(prefix) &&
			slices.Equal(columns[:len(prefix)], prefix) {
			return name, nil
		}
	}
	return "", nil
}

// omnigentConversationExists reports whether a conversation ID is present.
func omnigentConversationExists(dbPath, memberKey string) bool {
	conn, err := openOmnigentDB(dbPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return false
	}
	member, err := omnigentMemberForSchema(schema, memberKey)
	if err != nil {
		return false
	}
	var one int
	if schema.splitMetadata {
		err = conn.QueryRow(
			`SELECT 1 FROM conversations WHERE workspace_id = ? AND id = ? LIMIT 1`,
			member.workspaceID, omnigentIDArg(schema, member.rawID),
		).Scan(&one)
	} else {
		err = conn.QueryRow(
			`SELECT 1 FROM conversations WHERE id = ? LIMIT 1`,
			omnigentIDArg(schema, member.rawID),
		).Scan(&one)
	}
	return err == nil
}

// omnigentMeta is a lightweight per-conversation descriptor used for
// incremental sync: FileMtime tracks updated_at and Fingerprint is a cheap
// classification digest (updated_at + item count + max position) that catches
// supported Omnigent writes without reading every item. Full parses compute a
// separate semantic fingerprint over metadata and raw item content.
type omnigentMeta struct {
	rowID       int64
	workspaceID int64
	rawID       string
	updatedAt   int64
	itemCount   int
	maxPosition int
}

func (m omnigentMeta) member() omnigentMemberID {
	return omnigentMemberID{workspaceID: m.workspaceID, rawID: m.rawID}
}

func (m omnigentMeta) fingerprint() string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%d|%d", m.updatedAt, m.itemCount, m.maxPosition)
	return strconv.FormatUint(h.Sum64(), 16)
}

// listOmnigentConversationMetas returns one meta per conversation with a cheap
// aggregate fingerprint. The query touches only conversations and
// conversation_items, which exist in every generation.
func listOmnigentConversationMetas(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
) ([]omnigentMeta, error) {
	idExpr := omnigentIDExpr(schema, "c.id")
	query := `
		SELECT c.rowid, 0, ` + idExpr + `, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 GROUP BY c.id`
	if schema.splitMetadata {
		query = `
			SELECT c.rowid, c.workspace_id, ` + idExpr + `, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 GROUP BY c.workspace_id, c.id`
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing omnigent conversation metas: %w", err)
	}
	defer rows.Close()

	var out []omnigentMeta
	for rows.Next() {
		var m omnigentMeta
		if err := rows.Scan(
			&m.rowID, &m.workspaceID, &m.rawID, &m.updatedAt,
			&m.itemCount, &m.maxPosition,
		); err != nil {
			return nil, fmt.Errorf("scanning omnigent meta: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// omnigentConversationRow holds a conversation's session-level metadata,
// gathered from either the single conversations table or the split tables.
type omnigentConversationRow struct {
	workspaceID   int64
	id            string
	rootID        string
	createdAt     int64
	updatedAt     int64
	title         string
	kindRaw       string
	modelOverride string
	parentID      string
	subAgentName  string
	workspace     string
	gitBranch     string
	sessionUsage  []byte
}

// omnigentConvSelect builds the schema-appropriate SELECT for one conversation.
func omnigentConvSelect(s omnigentSchema) string {
	usage := "'' AS session_usage"
	if s.hasSessionUsage {
		if s.splitMetadata {
			usage = "COALESCE(m.session_usage, '') AS session_usage"
		} else {
			usage = "COALESCE(c.session_usage, '') AS session_usage"
		}
	}
	idExpr := omnigentIDExpr(s, "c.id")
	rootExpr := omnigentIDExpr(s, "c.root_conversation_id")
	parentExpr := omnigentIDExpr(s, "c.parent_conversation_id")
	if !s.splitMetadata {
		return `
			SELECT 0, ` + idExpr + `, COALESCE(` + rootExpr + `, ''),
			       COALESCE(c.created_at, 0), COALESCE(c.updated_at, 0),
			       COALESCE(c.title, ''), COALESCE(c.kind, ''),
			       COALESCE(c.model_override, ''),
			       COALESCE(` + parentExpr + `, ''),
			       COALESCE(c.sub_agent_name, ''), COALESCE(c.workspace, ''),
			       COALESCE(c.git_branch, ''), ` + usage + `
			  FROM conversations c
			 WHERE c.id = ?`
	}
	model := "'' AS model_override"
	if s.hasAgentConfig {
		model = "COALESCE(a.model_override, '') AS model_override"
	}
	join := ""
	if s.hasAgentConfig {
		join = ` LEFT JOIN agent_configuration a
		               ON a.workspace_id = c.workspace_id
		              AND a.conversation_id = c.id`
	}
	return `
		SELECT c.workspace_id, ` + idExpr + `, COALESCE(` + rootExpr + `, ''),
		       COALESCE(c.created_at, 0), COALESCE(c.updated_at, 0),
		       COALESCE(c.title, ''), COALESCE(CAST(m.kind AS TEXT), ''),
		       ` + model + `,
		       COALESCE(` + parentExpr + `, ''),
		       COALESCE(m.sub_agent_name, ''), COALESCE(m.workspace, ''),
		       COALESCE(m.git_branch, ''), ` + usage + `
		  FROM conversations c
		  LEFT JOIN omnigent_conversation_metadata m
		    ON m.workspace_id = c.workspace_id AND m.id = c.id` +
		join + `
		 WHERE c.workspace_id = ? AND c.id = ?`
}

func loadOmnigentConversation(
	ctx context.Context, conn *sql.DB,
	s omnigentSchema, member omnigentMemberID,
) (omnigentConversationRow, error) {
	row := omnigentConversationRow{}
	args := []any{omnigentIDArg(s, member.rawID)}
	if s.splitMetadata {
		args = []any{member.workspaceID, omnigentIDArg(s, member.rawID)}
	}
	err := conn.QueryRowContext(ctx, omnigentConvSelect(s), args...).Scan(
		&row.workspaceID, &row.id, &row.rootID, &row.createdAt, &row.updatedAt, &row.title,
		&row.kindRaw, &row.modelOverride, &row.parentID, &row.subAgentName,
		&row.workspace, &row.gitBranch, &row.sessionUsage,
	)
	if err != nil {
		return omnigentConversationRow{}, err
	}
	return row, nil
}

// omnigentIsSubAgent normalizes the kind column across encodings.
func omnigentIsSubAgent(kindRaw string) bool {
	return kindRaw == omnigentKindSubAgentName || kindRaw == omnigentKindSubAgentCode
}

// omnigentItemTypeName normalizes conversation_items.type to its string name.
func omnigentItemTypeName(s omnigentSchema, raw string) string {
	if !s.intEnums {
		return raw
	}
	if code, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		if name, ok := omnigentItemTypeByCode[code]; ok {
			return name
		}
	}
	return raw
}

// ParseOmnigentDB parses every conversation in a chat.db. Used by the container
// parse path and the opt-in real-data test.
func ParseOmnigentDB(dbPath, machine string) ([]ParseResult, error) {
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	conn, err := openOmnigentDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	metas, err := listOmnigentConversationMetas(
		context.Background(), conn, schema,
	)
	if err != nil {
		return nil, err
	}

	var results []ParseResult
	for _, meta := range metas {
		res, err := parseOmnigentConversationFromDB(
			context.Background(), conn, schema, dbPath,
			meta.member(), machine, dbInfo,
		)
		if err != nil {
			return nil, err
		}
		if res != nil {
			results = append(results, *res)
		}
	}
	InferRelationshipTypes(results)
	return results, nil
}

// parseOmnigentConversationFromDB parses one conversation using an already-open
// connection. The stored fingerprint covers conversation metadata and raw item
// content; the cheaper omnigentMeta fingerprint is used only for classification.
func parseOmnigentConversationFromDB(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	dbPath string, member omnigentMemberID, machine string, dbInfo os.FileInfo,
) (*ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conv, err := loadOmnigentConversation(ctx, conn, schema, member)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	messages, itemFingerprint, err := loadOmnigentMessages(
		ctx, conn, schema, member,
	)
	if err != nil {
		return nil, err
	}

	workspace, gitBranch, err := omnigentResolveWorkspace(
		ctx, conn, schema, conv,
	)
	if err != nil {
		return nil, err
	}
	fingerprint := omnigentSemanticFingerprint(
		conv, workspace, gitBranch, itemFingerprint,
	)

	var firstUser string
	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser {
			userCount++
			if firstUser == "" && m.Content != "" {
				firstUser = m.Content
			}
		}
	}

	sess := ParsedSession{
		ID:               member.sessionID(schema),
		Agent:            omnigentAgent,
		Machine:          machine,
		Project:          ExtractProjectFromCwd(workspace),
		Cwd:              workspace,
		GitBranch:        gitBranch,
		SessionName:      omnigentSessionName(conv),
		FirstMessage:     firstUser,
		StartedAt:        omnigentTime(conv.createdAt),
		EndedAt:          omnigentTime(conv.updatedAt),
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  VirtualSourcePath(dbPath, member.key(schema)),
			Size:  dbInfo.Size(),
			Mtime: conv.updatedAt * int64(time.Second),
			Hash:  fingerprint,
		},
	}
	if conv.parentID != "" {
		sess.ParentSessionID = omnigentMemberID{
			workspaceID: conv.workspaceID, rawID: conv.parentID,
		}.sessionID(schema)
	}
	if omnigentIsSubAgent(conv.kindRaw) {
		sess.RelationshipType = RelSubagent
	} else if sess.ParentSessionID != "" {
		sess.RelationshipType = RelContinuation
	}

	usageEvents := omnigentUsageEvents(
		sess.ID, conv.modelOverride, conv.sessionUsage,
	)
	accumulateMessageTokenUsage(&sess, messages)
	totalUsageOutput := 0
	hasUsageOutput := false
	for _, event := range usageEvents {
		if event.OutputTokens > 0 {
			hasUsageOutput = true
			totalUsageOutput += event.OutputTokens
		}
	}
	if hasUsageOutput {
		sess.HasTotalOutputTokens = true
		sess.TotalOutputTokens = totalUsageOutput
	}

	return &ParseResult{
		Session:     sess,
		Messages:    messages,
		UsageEvents: usageEvents,
	}, nil
}

func omnigentSemanticFingerprint(
	conv omnigentConversationRow, workspace, gitBranch, itemFingerprint string,
) string {
	h := sha256.New()
	for _, value := range []string{
		strconv.FormatInt(conv.workspaceID, 10),
		conv.id,
		conv.rootID,
		strconv.FormatInt(conv.createdAt, 10),
		strconv.FormatInt(conv.updatedAt, 10),
		conv.title,
		conv.kindRaw,
		conv.modelOverride,
		conv.parentID,
		conv.subAgentName,
		conv.workspace,
		conv.gitBranch,
		string(conv.sessionUsage),
		workspace,
		gitBranch,
		itemFingerprint,
	} {
		omnigentWriteFingerprintField(h, value)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func omnigentWriteFingerprintField(h hash.Hash, value string) {
	_, _ = fmt.Fprintf(h, "%d:", len(value))
	_, _ = h.Write([]byte(value))
}

// omnigentResolveWorkspace inherits cwd/branch from the root conversation when
// a sub-agent conversation carries none of its own.
func omnigentResolveWorkspace(
	ctx context.Context, conn *sql.DB,
	schema omnigentSchema, conv omnigentConversationRow,
) (string, string, error) {
	if conv.workspace != "" || conv.rootID == "" || conv.rootID == conv.id {
		return conv.workspace, conv.gitBranch, nil
	}
	root, err := loadOmnigentConversation(
		ctx, conn, schema,
		omnigentMemberID{
			workspaceID: conv.workspaceID, rawID: conv.rootID,
		},
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", ctxErr
		}
		return conv.workspace, conv.gitBranch, nil
	}
	workspace := conv.workspace
	if workspace == "" {
		workspace = root.workspace
	}
	branch := conv.gitBranch
	if branch == "" {
		branch = root.gitBranch
	}
	return workspace, branch, nil
}

func omnigentSessionName(c omnigentConversationRow) string {
	if c.title != "" {
		return c.title
	}
	return c.subAgentName
}

// omnigentTime converts an epoch-seconds stamp to UTC. Zero stays zero.
func omnigentTime(epochSec int64) time.Time {
	if epochSec == 0 {
		return time.Time{}
	}
	return time.Unix(epochSec, 0).UTC()
}

// omnigentMessageData mirrors a `message` item. The author-agent is serialized
// as "agent" (older builds) or "model" (newer builds, via serialization_alias).
type omnigentMessageData struct {
	Role    string `json:"role"`
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type omnigentFuncCallData struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
}

type omnigentFuncOutputData struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type omnigentReasoningData struct {
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type omnigentErrorData struct {
	Source  string `json:"source"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type omnigentSlashCommandData struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// loadOmnigentMessages decodes a conversation's items into messages in position
// order, folding function_call_output onto its originating call. The returned
// fingerprint covers the raw fields so in-place edits to decoded or fallback
// content remain visible during periodic full sync.
func loadOmnigentMessages(
	ctx context.Context, conn *sql.DB,
	schema omnigentSchema, member omnigentMemberID,
) ([]ParsedMessage, string, error) {
	query := `
		SELECT position, type, COALESCE(data, ''), COALESCE(search_text, '')
		  FROM conversation_items
		 WHERE conversation_id = ?
		 ORDER BY position ASC`
	args := []any{omnigentIDArg(schema, member.rawID)}
	if schema.splitMetadata {
		query = `
			SELECT position, type, COALESCE(data, ''), COALESCE(search_text, '')
			  FROM conversation_items
			 WHERE workspace_id = ? AND conversation_id = ?
			 ORDER BY position ASC`
		args = []any{member.workspaceID, omnigentIDArg(schema, member.rawID)}
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf(
			"listing omnigent items for %s: %w", member.key(schema), err)
	}
	defer rows.Close()

	var messages []ParsedMessage
	callMsgIndex := map[string]int{}
	h := sha256.New()
	for rows.Next() {
		var (
			position   int
			rawType    string
			data       string
			searchText string
		)
		if err := rows.Scan(&position, &rawType, &data, &searchText); err != nil {
			return nil, "", fmt.Errorf("scanning omnigent item: %w", err)
		}
		for _, value := range []string{
			strconv.Itoa(position), rawType, data, searchText,
		} {
			omnigentWriteFingerprintField(h, value)
		}
		typeName := omnigentItemTypeName(schema, rawType)
		decodeOmnigentItem(
			position, typeName, data, searchText, &messages, callMsgIndex)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return messages, fmt.Sprintf("%x", h.Sum(nil)), nil
}

// decodeOmnigentItem appends the ParsedMessage(s) for one item, or folds a tool
// output onto its call. callMsgIndex maps a call_id to the index of the message
// carrying that tool call.
func decodeOmnigentItem(
	position int, typeName, data, searchText string,
	messages *[]ParsedMessage, callMsgIndex map[string]int,
) {
	switch typeName {
	case omnigentTypeMessage:
		var md omnigentMessageData
		if json.Unmarshal([]byte(data), &md) != nil {
			return
		}
		content := omnigentJoinText(md.Content)
		role := RoleAssistant
		if md.Role == "user" {
			role = RoleUser
		}
		*messages = append(*messages, ParsedMessage{
			Ordinal:       position,
			Role:          role,
			Content:       content,
			ContentLength: len(content),
		})

	case omnigentTypeFuncCall:
		var fc omnigentFuncCallData
		if json.Unmarshal([]byte(data), &fc) != nil {
			return
		}
		*messages = append(*messages, ParsedMessage{
			Ordinal:    position,
			Role:       RoleAssistant,
			HasToolUse: true,
			ToolCalls: []ParsedToolCall{{
				ToolUseID: fc.CallID,
				ToolName:  fc.Name,
				Category:  NormalizeToolCategory(fc.Name),
				InputJSON: fc.Arguments,
			}},
		})
		if fc.CallID != "" {
			callMsgIndex[fc.CallID] = len(*messages) - 1
		}

	case omnigentTypeFuncOutput:
		var fo omnigentFuncOutputData
		if json.Unmarshal([]byte(data), &fo) != nil {
			return
		}
		quoted, err := json.Marshal(fo.Output)
		if err != nil {
			return
		}
		result := ParsedToolResult{
			ToolUseID:     fo.CallID,
			ContentLength: len(fo.Output),
			ContentRaw:    string(quoted),
		}
		if idx, ok := callMsgIndex[fo.CallID]; ok {
			msg := &(*messages)[idx]
			msg.ToolResults = append(msg.ToolResults, result)
			return
		}
		// Orphan output (no matching call in this conversation): keep it
		// visible as its own tool-role message.
		*messages = append(*messages, ParsedMessage{
			Ordinal:       position,
			Role:          RoleTool,
			Content:       fo.Output,
			ContentLength: len(fo.Output),
			ToolResults:   []ParsedToolResult{result},
		})

	case omnigentTypeReasoning:
		var rd omnigentReasoningData
		if json.Unmarshal([]byte(data), &rd) != nil {
			return
		}
		var b strings.Builder
		for _, s := range rd.Summary {
			if s.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s.Text)
		}
		thinking := b.String()
		if thinking == "" {
			return
		}
		*messages = append(*messages, ParsedMessage{
			Ordinal:      position,
			Role:         RoleAssistant,
			ThinkingText: thinking,
			HasThinking:  true,
		})

	default:
		// error, compaction, routing_decision, slash_command,
		// terminal_command, resource_event, native_tool: surface a concise
		// system line rather than dropping the event.
		content := omnigentSystemLine(typeName, data, searchText)
		if content == "" {
			return
		}
		*messages = append(*messages, ParsedMessage{
			Ordinal:       position,
			Role:          RoleSystem,
			IsSystem:      true,
			Content:       content,
			ContentLength: len(content),
		})
	}
}

func omnigentJoinText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var b strings.Builder
	for _, blk := range content {
		if blk.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// omnigentSystemLine renders a human-readable one-liner for the item types the
// parser does not model as first-class messages.
func omnigentSystemLine(typeName, data, searchText string) string {
	switch typeName {
	case omnigentTypeError:
		var ed omnigentErrorData
		if json.Unmarshal([]byte(data), &ed) == nil && ed.Message != "" {
			return "[error] " + ed.Message
		}
	case omnigentTypeSlashCommand:
		var sc omnigentSlashCommandData
		if json.Unmarshal([]byte(data), &sc) == nil && sc.Name != "" {
			kind := sc.Kind
			if kind == "" {
				kind = "command"
			}
			return "[" + kind + "] " + sc.Name
		}
	}
	if searchText != "" {
		return "[" + typeName + "] " + searchText
	}
	return ""
}

// omnigentUsageEvents decodes the session_usage blob (zstd-framed on newer
// builds, plaintext JSON on older ones) into a single session-level usage
// event, plus per-model breakdown when present.
func omnigentUsageEvents(
	sessionID, fallbackModel string, raw []byte,
) []ParsedUsageEvent {
	text, err := decodeOmnigentCompressed(raw)
	if err != nil || strings.TrimSpace(text) == "" {
		return nil
	}
	// Cost fields are *float64 to track presence: omnigent only records
	// total_cost_usd when the child harness prices its own usage, and an
	// absent cost must stay NULL so catalog-based token pricing applies
	// instead of an authoritative $0.
	var usage struct {
		InputTokens  int      `json:"input_tokens"`
		OutputTokens int      `json:"output_tokens"`
		TotalCostUSD *float64 `json:"total_cost_usd"`
		ByModel      map[string]struct {
			InputTokens  int      `json:"input_tokens"`
			OutputTokens int      `json:"output_tokens"`
			TotalCostUSD *float64 `json:"total_cost_usd"`
		} `json:"by_model"`
	}
	if json.Unmarshal([]byte(text), &usage) != nil {
		return nil
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.TotalCostUSD == nil && len(usage.ByModel) == 0 {
		return nil
	}

	if len(usage.ByModel) > 0 {
		models := make([]string, 0, len(usage.ByModel))
		for model := range usage.ByModel {
			models = append(models, model)
		}
		slices.Sort(models)

		events := make([]ParsedUsageEvent, 0, len(models))
		var explicitCost float64
		var missingCostIndexes []int
		var missingCostWeight int
		for _, model := range models {
			m := usage.ByModel[model]
			events = append(events, ParsedUsageEvent{
				SessionID:    sessionID,
				Source:       "session",
				Model:        model,
				InputTokens:  m.InputTokens,
				OutputTokens: m.OutputTokens,
				CostUSD:      m.TotalCostUSD,
				DedupKey:     sessionID + "|usage|" + model,
			})
			if m.TotalCostUSD != nil {
				explicitCost += *m.TotalCostUSD
				continue
			}
			missingCostIndexes = append(missingCostIndexes, len(events)-1)
			missingCostWeight += max(0, m.InputTokens+m.OutputTokens)
		}
		if usage.TotalCostUSD != nil && len(missingCostIndexes) > 0 {
			remainingCost := max(0, *usage.TotalCostUSD-explicitCost)
			remainingWeight := missingCostWeight
			for i, eventIndex := range missingCostIndexes {
				cost := remainingCost
				if i < len(missingCostIndexes)-1 {
					weight := max(
						0,
						events[eventIndex].InputTokens+
							events[eventIndex].OutputTokens,
					)
					if remainingWeight > 0 {
						cost = remainingCost *
							float64(weight) / float64(remainingWeight)
					} else {
						cost = remainingCost /
							float64(len(missingCostIndexes)-i)
					}
				}
				events[eventIndex].CostUSD = &cost
				remainingCost -= cost
				remainingWeight -= max(
					0,
					events[eventIndex].InputTokens+
						events[eventIndex].OutputTokens,
				)
			}
		}
		return events
	}

	fallbackModel = strings.TrimSpace(fallbackModel)
	if fallbackModel == "" {
		fallbackModel = "unknown"
	}
	return []ParsedUsageEvent{{
		SessionID:    sessionID,
		Source:       "session",
		Model:        fallbackModel,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      usage.TotalCostUSD,
		DedupKey:     sessionID + "|usage|" + fallbackModel,
	}}
}

// omnigent compression framing (db/compression.py): a value starting with a
// 0x00 sentinel is framed as sentinel + codec_id + payload, where codec 0x01 is
// zstd and 0x00 is raw. Anything else is legacy unframed UTF-8 text.
const (
	omnigentCompressSentinel = 0x00
	omnigentCodecRaw         = 0x00
	omnigentCodecZstd        = 0x01
)

var omnigentZstdReader, _ = zstd.NewReader(nil)

func decodeOmnigentCompressed(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] != omnigentCompressSentinel {
		// Legacy unframed UTF-8 text (also the SQLite dynamic-typing case where
		// the value arrives as plain text).
		return string(raw), nil
	}
	if len(raw) < 2 {
		return "", nil
	}
	codec := raw[1]
	payload := raw[2:]
	switch codec {
	case omnigentCodecRaw:
		return string(payload), nil
	case omnigentCodecZstd:
		out, err := omnigentZstdReader.DecodeAll(payload, nil)
		if err != nil {
			return "", fmt.Errorf("omnigent zstd decode: %w", err)
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("omnigent: unknown compression codec %d", codec)
	}
}

// omnigentDBPath returns <root>/chat.db when it is a regular file.
func omnigentDBPath(root string) string {
	dbPath := filepath.Join(root, omnigentDBName)
	if IsRegularFile(dbPath) {
		return dbPath
	}
	return ""
}
