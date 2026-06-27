package parser

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"os"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const (
	shelleyIDPrefix = "shelley:"
	shelleyDBName   = "shelley.db"
)

// Shelley (exe.dev) stores every conversation in a single SQLite DB
// (~/.config/shelley/shelley.db) with two tables that matter here:
//
//	conversations(conversation_id, slug, user_initiated, created_at,
//	              updated_at, cwd, parent_conversation_id, model,
//	              current_generation, ...)
//	messages(message_id, conversation_id, sequence_id, type,
//	         llm_data, user_data, usage_data, display_data,
//	         created_at, generation, excluded_from_context)
//
// Like Zed, each conversation is addressed by a virtual source path of
// the form "<dbPath>#<conversationID>". sequence_id is monotonic and
// unique per conversation (never reused across generations), so it maps
// directly to the AgentsView message Ordinal.

// Shelley llm_data content block type tags. Upstream these are the
// llm.ContentType iota constants; the enum has no JSON marshaler, so
// they serialize as bare integers. Note the values start at 2, not 0:
// ContentType shares its const block with MessageRole (User=0,
// Assistant=1) and iota does not reset between the two type groups.
// These values are verified against real shelley.db data.
const (
	shelleyContentText                = 2
	shelleyContentThinking            = 3
	shelleyContentRedactedThinking    = 4
	shelleyContentToolUse             = 5
	shelleyContentToolResult          = 6
	shelleyContentServerToolUse       = 7
	shelleyContentWebSearchToolResult = 8
	shelleyContentWebSearchResult     = 9
)

// Shelley message-table row types. Other types (system, error,
// gitinfo) are handled by the default branch in decodeShelleyMessage.
const (
	shelleyTypeUser  = "user"
	shelleyTypeAgent = "agent"
	shelleyTypeTool  = "tool"
)

// shelleyLLMMessage mirrors the serialized llm.Message stored in the
// messages.llm_data column. Field names are PascalCase to match the
// upstream Go struct's default JSON encoding.
type shelleyLLMMessage struct {
	Role    int              `json:"Role"`
	Content []shelleyContent `json:"Content"`
}

// shelleyContent mirrors the serialized llm.Content block.
type shelleyContent struct {
	ID         string           `json:"ID"`
	Type       int              `json:"Type"`
	Text       string           `json:"Text"`
	Thinking   string           `json:"Thinking"`
	ToolName   string           `json:"ToolName"`
	ToolInput  json.RawMessage  `json:"ToolInput"`
	ToolUseID  string           `json:"ToolUseID"`
	ToolError  bool             `json:"ToolError"`
	ToolResult []shelleyContent `json:"ToolResult"`
	Title      string           `json:"Title"`
	URL        string           `json:"URL"`
}

// shelleyUsage mirrors the serialized llm.Usage stored in
// messages.usage_data. The token keys are already the AgentsView
// canonical Anthropic names, so the raw blob is stored verbatim for
// cost pricing. json.Number keeps decoding tolerant of string- or
// float-encoded counts.
type shelleyUsage struct {
	InputTokens              json.Number `json:"input_tokens"`
	CacheCreationInputTokens json.Number `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     json.Number `json:"cache_read_input_tokens"`
	OutputTokens             json.Number `json:"output_tokens"`
	Model                    string      `json:"model"`
}

// ShelleyVirtualPath gives each conversation in the shared Shelley DB a
// stable source identity for the AgentsView archive.
func ShelleyVirtualPath(dbPath, conversationID string) string {
	return dbPath + "#" + conversationID
}

// ShelleyConversationExists reports whether the Shelley DB has a
// conversation row with the given ID.
func ShelleyConversationExists(dbPath, conversationID string) bool {
	if dbPath == "" || conversationID == "" || !IsRegularFile(dbPath) {
		return false
	}
	conn, err := openShelleyDB(dbPath)
	if err != nil {
		return false
	}
	defer conn.Close()

	var found int
	err = conn.QueryRow(
		`SELECT 1 FROM conversations WHERE conversation_id = ? LIMIT 1`,
		conversationID,
	).Scan(&found)
	return err == nil
}

// ShelleyConversationMeta holds per-conversation state used by the sync
// engine for per-session skip detection.
type ShelleyConversationMeta struct {
	RawID       string
	VirtualPath string
	// FileMtime is the conversation's updated_at as a nanosecond
	// timestamp. It stays a real (whole-second) timestamp so the
	// modified-between range queries that drive PG/DuckDB push never
	// see a Shelley row as "future"; see shelleyChangeMtime.
	FileMtime int64
	// Fingerprint is a digest over every parser-observed conversation and
	// message field. It is stored in file_hash and compared in the sync
	// skip to catch same-second changes FileMtime's second precision
	// cannot; see shelleyDigestConversation and shelleyDigestMessage.
	Fingerprint string
}

// Shelley change detection uses two per-conversation signals, because
// Shelley's updated_at is only whole-second precision (it writes it as
// SQLite CURRENT_TIMESTAMP) and so cannot, on its own, distinguish two
// writes in the same wall-clock second:
//
//   - File.Mtime / ShelleyConversationMeta.FileMtime is the conversation's
//     updated_at as a real nanosecond timestamp. The sync skip compares it
//     for equality, but it must also stay a true timestamp because
//     ListSessionsModifiedBetween filters file_mtime <= now for PG/DuckDB
//     push; a synthetic future value would drop a just-synced Shelley row
//     from a same-second push until a later run.
//   - Fingerprint, a digest stored in file_hash, distinguishes any
//     same-second parser-visible change the timestamp cannot: metadata
//     edits, appends, in-place rewrites, and even length-preserving edits
//     (the digest length-frames each field). The skip re-parses whenever
//     it differs.
//
// Computing the digest means reading message payloads, which the cheaper
// siblings (Zed, Kiro) avoid because their sub-second / millisecond
// updated_at already distinguishes same-second writes. Shelley's
// second-precision timestamp does not, so the content read is the price of
// not silently skipping a rewrite.
//
// shelleyDigestConversation and shelleyDigestMessage fold every
// parser-observed Shelley field into a running FNV-1a hash. Each field is
// length-framed so equal total byte counts cannot collide by moving bytes
// between fields. Callers must feed messages in sequence_id order so the
// parse, skip, and source-mtime paths agree.
func shelleyDigestConversation(h hash.Hash64, conv shelleyConversationRow) {
	shelleyDigestFields(
		h,
		"conversation",
		conv.conversationID,
		conv.slug,
		strconv.FormatBool(conv.userInitiated),
		conv.createdAt,
		conv.updatedAt,
		conv.cwd,
		conv.parentConversationID,
		conv.model,
	)
}

func shelleyDigestMessage(h hash.Hash64, r shelleyMessageRow) {
	shelleyDigestFields(
		h,
		"message",
		strconv.FormatInt(r.sequenceID, 10),
		r.msgType,
		r.llmData,
		r.userData,
		r.usageData,
		r.createdAt,
	)
}

func shelleyDigestFields(h hash.Hash64, fields ...string) {
	var n [8]byte
	for _, s := range fields {
		binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(s))
	}
}

// shelleyFingerprint renders a content-digest hash as the stable string
// stored in file_hash and compared by the sync skip.
func shelleyFingerprint(h hash.Hash64) string {
	return strconv.FormatUint(h.Sum64(), 16)
}

// ListShelleyConversationMetas returns the skip state for every
// conversation: its updated_at timestamp (FileMtime) and parser-visible
// digest (Fingerprint). It scans message payloads ordered by conversation
// and sequence_id, hashing each conversation's rows the same way the parse
// loop does, so the meta digest and the stored file_hash match for
// unchanged conversations. It shares the caller's open connection.
func ListShelleyConversationMetas(
	conn *sql.DB, dbPath string,
) ([]ShelleyConversationMeta, error) {
	rows, err := conn.Query(
		`SELECT c.conversation_id, COALESCE(c.slug, ''),
		        COALESCE(c.user_initiated, 1),
		        COALESCE(c.created_at, ''), COALESCE(c.updated_at, ''),
		        COALESCE(c.cwd, ''), COALESCE(c.parent_conversation_id, ''),
		        COALESCE(c.model, ''),
		        COALESCE(m.sequence_id, 0), COALESCE(m.type, ''),
		        COALESCE(m.llm_data, ''), COALESCE(m.user_data, ''),
		        COALESCE(m.usage_data, ''), COALESCE(m.created_at, '')
		   FROM conversations c
		   JOIN messages m ON m.conversation_id = c.conversation_id
		  ORDER BY c.conversation_id, m.sequence_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing shelley conversations: %w", err)
	}
	defer rows.Close()

	var (
		metas []ShelleyConversationMeta
		curID string
		conv  shelleyConversationRow
	)
	h := fnv.New64a()
	flush := func() {
		// curID is "" before the first row; IsValidSessionID rejects it.
		if !IsValidSessionID(curID) {
			return
		}
		metas = append(metas, ShelleyConversationMeta{
			RawID:       curID,
			VirtualPath: ShelleyVirtualPath(dbPath, curID),
			FileMtime:   parseTimestamp(conv.updatedAt).UnixNano(),
			Fingerprint: shelleyFingerprint(h),
		})
	}
	for rows.Next() {
		var rowConv shelleyConversationRow
		var msg shelleyMessageRow
		var userInitiated int64
		if err := rows.Scan(
			&rowConv.conversationID, &rowConv.slug,
			&userInitiated, &rowConv.createdAt,
			&rowConv.updatedAt, &rowConv.cwd,
			&rowConv.parentConversationID, &rowConv.model,
			&msg.sequenceID, &msg.msgType, &msg.llmData,
			&msg.userData, &msg.usageData, &msg.createdAt,
		); err != nil {
			return nil, fmt.Errorf("scanning shelley conversation meta: %w", err)
		}
		rowConv.userInitiated = userInitiated != 0
		if rowConv.conversationID != curID {
			flush()
			curID, conv, h = rowConv.conversationID, rowConv, fnv.New64a()
			shelleyDigestConversation(h, conv)
		}
		shelleyDigestMessage(h, msg)
	}
	flush()
	return metas, rows.Err()
}

// ShelleySourceMtime resolves the per-conversation change signal for a
// virtual Shelley source path. Used by the live watcher, which compares it
// for inequality and treats a zero/error result as "source gone". It
// returns the conversation's updated_at plus a sub-second term derived
// from the parser-visible digest, so the watcher reacts to same-second
// edits.
// This value is watcher-only and never written to file_mtime or
// range-filtered, so the sub-second term is harmless here.
func ShelleySourceMtime(path string) (int64, error) {
	dbPath, conversationID, ok := parseShelleyVirtualPath(path)
	if !ok {
		return 0, fmt.Errorf("not a shelley virtual path: %s", path)
	}
	conn, err := openShelleyDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	conv, err := loadShelleyConversation(conn, conversationID)
	if err != nil {
		return 0, fmt.Errorf(
			"loading shelley conversation mtime %s: %w",
			conversationID, err,
		)
	}

	rows, err := conn.Query(
		`SELECT COALESCE(sequence_id, 0), COALESCE(type, ''),
		        COALESCE(llm_data, ''), COALESCE(user_data, ''),
		        COALESCE(usage_data, ''), COALESCE(created_at, '')
		   FROM messages WHERE conversation_id = ?
		  ORDER BY sequence_id`,
		conversationID,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"loading shelley messages %s: %w", conversationID, err,
		)
	}
	defer rows.Close()
	h := fnv.New64a()
	shelleyDigestConversation(h, conv)
	for rows.Next() {
		var r shelleyMessageRow
		if err := rows.Scan(
			&r.sequenceID, &r.msgType, &r.llmData,
			&r.userData, &r.usageData, &r.createdAt,
		); err != nil {
			return 0, fmt.Errorf("scanning shelley message: %w", err)
		}
		shelleyDigestMessage(h, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return parseTimestamp(conv.updatedAt).UnixNano() +
		int64(h.Sum64()%1_000_000_000), nil
}

// OpenShelleyDB opens the Shelley shelley.db file read-only. Callers are
// responsible for calling Close on the returned *sql.DB.
func OpenShelleyDB(dbPath string) (*sql.DB, error) {
	return openShelleyDB(dbPath)
}

func openShelleyDB(dbPath string) (*sql.DB, error) {
	dsn := dbPath + "?mode=ro&_journal_mode=WAL&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening shelley db %s: %w", dbPath, err)
	}
	return db, nil
}

// parseShelleyConversationFromDB parses one conversation using an
// already-open connection. Callers parsing multiple conversations should
// open the DB once and call this in a loop.
func parseShelleyConversationFromDB(
	conn *sql.DB, dbPath, rawID, machine string, dbInfo os.FileInfo,
) (*ParseResult, error) {
	conv, err := loadShelleyConversation(conn, rawID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	messages, fingerprint, err := loadShelleyMessages(conn, conv)
	if err != nil {
		return nil, err
	}
	result, ok := buildShelleyParseResult(
		conv, messages, fingerprint, dbPath, dbInfo, machine,
	)
	if !ok {
		return nil, nil
	}
	return &result, nil
}

type shelleyConversationRow struct {
	conversationID       string
	slug                 string
	userInitiated        bool
	createdAt            string
	updatedAt            string
	cwd                  string
	parentConversationID string
	model                string
}

func loadShelleyConversation(
	conn *sql.DB, conversationID string,
) (shelleyConversationRow, error) {
	row := shelleyConversationRow{conversationID: conversationID}
	var userInitiated int64
	err := conn.QueryRow(
		`SELECT COALESCE(slug, ''), COALESCE(user_initiated, 1),
		        COALESCE(created_at, ''), COALESCE(updated_at, ''),
		        COALESCE(cwd, ''), COALESCE(parent_conversation_id, ''),
		        COALESCE(model, '')
		   FROM conversations
		  WHERE conversation_id = ?`,
		conversationID,
	).Scan(
		&row.slug, &userInitiated, &row.createdAt, &row.updatedAt,
		&row.cwd, &row.parentConversationID, &row.model,
	)
	if err != nil {
		return shelleyConversationRow{}, err
	}
	row.userInitiated = userInitiated != 0
	return row, nil
}

type shelleyMessageRow struct {
	sequenceID int64
	msgType    string
	llmData    string
	userData   string
	usageData  string
	createdAt  string
}

func loadShelleyMessages(
	conn *sql.DB, conv shelleyConversationRow,
) ([]ParsedMessage, string, error) {
	// All generations are included, ordered by sequence_id. A generation
	// bump is a context-reset/compaction boundary within the conversation
	// (e.g. distillation); older-generation rows remain as real history
	// and must not be hidden. sequence_id is unique per conversation
	// across generations, so it is a safe Ordinal.
	rows, err := conn.Query(
		`SELECT COALESCE(sequence_id, 0), COALESCE(type, ''),
		        COALESCE(llm_data, ''), COALESCE(user_data, ''),
		        COALESCE(usage_data, ''), COALESCE(created_at, '')
		   FROM messages
		  WHERE conversation_id = ?
		  ORDER BY sequence_id ASC`,
		conv.conversationID,
	)
	if err != nil {
		return nil, "", fmt.Errorf(
			"listing shelley messages for %s: %w", conv.conversationID, err,
		)
	}
	defer rows.Close()

	var messages []ParsedMessage
	h := fnv.New64a()
	shelleyDigestConversation(h, conv)
	for rows.Next() {
		var r shelleyMessageRow
		if err := rows.Scan(
			&r.sequenceID, &r.msgType, &r.llmData,
			&r.userData, &r.usageData, &r.createdAt,
		); err != nil {
			return nil, "", fmt.Errorf("scanning shelley message: %w", err)
		}
		// Digest every parser-observed row field (even for rows
		// decodeShelleyMessage drops) in sequence_id order, exactly as
		// ListShelleyConversationMetas does, so the stored fingerprint
		// matches the meta skip query; a mismatch would re-parse forever.
		shelleyDigestMessage(h, r)
		if msg, ok := decodeShelleyMessage(r, conv.model); ok {
			messages = append(messages, msg)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return messages, shelleyFingerprint(h), nil
}

func decodeShelleyMessage(
	r shelleyMessageRow, convModel string,
) (ParsedMessage, bool) {
	var role RoleType
	isSystem := false
	switch r.msgType {
	case shelleyTypeAgent:
		role = RoleAssistant
	case shelleyTypeUser, shelleyTypeTool:
		role = RoleUser
	default:
		// system, error, gitinfo, and any future bookkeeping types.
		role = RoleUser
		isSystem = true
	}

	var (
		textParts     []string
		thinkingParts []string
		toolCalls     []ParsedToolCall
		toolResults   []ParsedToolResult
	)

	if r.llmData != "" {
		var msg shelleyLLMMessage
		if err := json.Unmarshal([]byte(r.llmData), &msg); err == nil {
			for _, c := range msg.Content {
				switch c.Type {
				case shelleyContentText:
					if c.Text != "" {
						textParts = append(textParts, c.Text)
					}
				case shelleyContentThinking:
					if c.Thinking != "" {
						thinkingParts = append(thinkingParts, c.Thinking)
					}
				case shelleyContentRedactedThinking:
					thinkingParts = append(thinkingParts, "[redacted thinking]")
				case shelleyContentToolUse, shelleyContentServerToolUse:
					if c.ToolName != "" {
						toolCalls = append(toolCalls, ParsedToolCall{
							ToolUseID: c.ID,
							ToolName:  c.ToolName,
							Category:  NormalizeToolCategory(c.ToolName),
							InputJSON: shelleyToolInput(c.ToolInput),
						})
					}
				case shelleyContentToolResult, shelleyContentWebSearchToolResult:
					text := shelleyToolResultText(c.ToolResult)
					quoted, _ := json.Marshal(text)
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     c.ToolUseID,
						ContentRaw:    string(quoted),
						ContentLength: len(text),
					})
				case shelleyContentWebSearchResult:
					if label := strings.TrimSpace(c.Title + " " + c.URL); label != "" {
						textParts = append(textParts, label)
					}
				}
			}
		}
	}

	content := strings.TrimSpace(strings.Join(textParts, ""))
	thinking := strings.TrimSpace(strings.Join(thinkingParts, "\n"))

	// User-typed text may live only in user_data for some message types.
	if content == "" && len(toolCalls) == 0 && len(toolResults) == 0 {
		content = shelleyUserDataText(r.userData)
	}

	msg := ParsedMessage{
		Ordinal:       int(r.sequenceID),
		Role:          role,
		Content:       content,
		ThinkingText:  thinking,
		HasThinking:   thinking != "",
		HasToolUse:    len(toolCalls) > 0,
		IsSystem:      isSystem,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
		ToolResults:   toolResults,
		Timestamp:     parseTimestamp(r.createdAt),
	}

	// Apply usage for any row that carries it, not just agent rows:
	// Shelley records token usage on errored assistant turns too (stored
	// as type="error"), and applyShelleyUsage no-ops on the all-zero
	// usage blobs Shelley writes for user/tool messages.
	if r.usageData != "" {
		applyShelleyUsage(&msg, r.usageData, convModel)
	}
	if content == "" && thinking == "" &&
		len(toolCalls) == 0 && len(toolResults) == 0 {
		if !msg.HasContextTokens && !msg.HasOutputTokens {
			return ParsedMessage{}, false
		}
		msg.IsSystem = true
	}
	return msg, true
}

// shelleyToolInput returns the raw tool input JSON, normalizing the
// absent/null cases to an empty string.
func shelleyToolInput(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	return s
}

// shelleyToolResultText flattens the nested tool_result content blocks
// into a single string that DecodeContent can later decode via its
// string branch. Blocks are joined with newlines so multiple results
// (notably web_search_result lists) stay readable instead of running
// together.
func shelleyToolResultText(blocks []shelleyContent) string {
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Text != "":
			parts = append(parts, b.Text)
		case len(b.ToolResult) > 0:
			if nested := shelleyToolResultText(b.ToolResult); nested != "" {
				parts = append(parts, nested)
			}
		case b.Title != "" || b.URL != "":
			// web_search_result blocks (inside a web_search_tool_result)
			// carry their payload in Title/URL rather than Text. Without
			// this branch the whole result would be stored empty and the
			// carrier message could be dropped.
			if label := strings.TrimSpace(b.Title + " " + b.URL); label != "" {
				parts = append(parts, label)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// shelleyUserDataText best-effort extracts displayable text from a
// message's user_data JSON. user_data is UI-display data whose shape
// varies by message type, so this only recovers obvious text fields.
func shelleyUserDataText(userData string) string {
	if userData == "" {
		return ""
	}
	var raw any
	if err := json.Unmarshal([]byte(userData), &raw); err != nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"text", "content", "prompt", "message"} {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func applyShelleyUsage(msg *ParsedMessage, usageData, convModel string) {
	var u shelleyUsage
	if err := json.Unmarshal([]byte(usageData), &u); err != nil {
		return
	}
	context := shelleyTokenCount(u.InputTokens) +
		shelleyTokenCount(u.CacheCreationInputTokens) +
		shelleyTokenCount(u.CacheReadInputTokens)
	output := shelleyTokenCount(u.OutputTokens)

	// Shelley writes an all-zero usage blob on user/tool rows; skip those
	// so they do not produce spurious token-presence flags or empty
	// token_usage rows in the cost UNION.
	if context == 0 && output == 0 {
		return
	}

	// The usage_data keys are already the AgentsView canonical names, so
	// the raw blob is stored verbatim for catalog cost pricing.
	//
	// usage_data also carries an exact gateway cost_usd. It is not yet
	// surfaced: capturing it cleanly (without double-counting cost
	// against the catalog-priced per-message tokens) needs a dedicated
	// usage-event path and is left as a follow-up. Standard gateway
	// models are priced correctly by the catalog today.
	msg.TokenUsage = json.RawMessage(usageData)
	msg.ContextTokens = context
	msg.OutputTokens = output
	msg.HasContextTokens = context > 0
	msg.HasOutputTokens = output > 0
	msg.tokenPresenceKnown = true

	model := strings.TrimSpace(u.Model)
	if model == "" {
		model = strings.TrimSpace(convModel)
	}
	if model != "" {
		msg.Model = model
	}
}

// shelleyTokenCount tolerantly decodes a token count, mapping
// empty/garbage/negative values to 0 and bounding implausibly large
// values so a single corrupt count cannot poison aggregate totals.
func shelleyTokenCount(n json.Number) int {
	if n == "" {
		return 0
	}
	v, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil {
			return 0
		}
		v = int64(f)
	}
	const maxPlausible = 1 << 40 // ~1.1e12 tokens; far above any real session
	if v < 0 || v > maxPlausible {
		return 0
	}
	return int(v)
}

func buildShelleyParseResult(
	conv shelleyConversationRow,
	messages []ParsedMessage,
	fingerprint string,
	dbPath string,
	info os.FileInfo,
	machine string,
) (ParseResult, bool) {
	if len(messages) == 0 {
		return ParseResult{}, false
	}
	hasContent := false
	for _, m := range messages {
		if m.Content != "" || m.HasThinking ||
			m.HasToolUse || len(m.ToolResults) > 0 {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return ParseResult{}, false
	}

	startedAt := parseTimestamp(conv.createdAt)
	endedAt := parseTimestamp(conv.updatedAt)
	if startedAt.IsZero() {
		startedAt = messages[0].Timestamp
	}
	if endedAt.IsZero() {
		endedAt = messages[len(messages)-1].Timestamp
	}
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	cwd := strings.TrimSpace(conv.cwd)
	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = "unknown"
	}

	// Count only genuine user turns: tool-result messages also carry
	// the user role but hold their payload in ToolResults with empty
	// Content, so the Content != "" guard excludes them (matching the
	// Claude parser's firstMessageAndUserCount convention).
	var firstMessage string
	var userCount int
	for _, m := range messages {
		if m.IsSystem || m.Role != RoleUser || m.Content == "" {
			continue
		}
		userCount++
		if firstMessage == "" {
			firstMessage = truncate(
				strings.ReplaceAll(m.Content, "\n", " "), 300,
			)
		}
	}
	sessionName := strings.TrimSpace(conv.slug)
	if firstMessage == "" {
		firstMessage = truncate(sessionName, 300)
	}

	sessionID := shelleyIDPrefix + conv.conversationID
	sess := ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentShelley,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  ShelleyVirtualPath(dbPath, conv.conversationID),
			Size:  info.Size(),
			Mtime: endedAt.UnixNano(),
			Hash:  fingerprint,
		},
	}
	if parent := strings.TrimSpace(conv.parentConversationID); parent != "" {
		sess.ParentSessionID = shelleyIDPrefix + parent
		if conv.userInitiated {
			sess.RelationshipType = RelContinuation
		} else {
			sess.RelationshipType = RelSubagent
		}
	}
	accumulateMessageTokenUsage(&sess, messages)

	return ParseResult{Session: sess, Messages: messages}, true
}
