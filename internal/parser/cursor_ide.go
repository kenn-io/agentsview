package parser

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Cursor IDE (the GUI editor) is a completely different session store from
// Cursor Agent (the CLI, see cursor.go): it is a VS Code-style global-state
// SQLite database, state.vscdb, whose cursorDiskKV table holds one JSON blob
// per key. A "composerData:<uuid>" key is one chat session; its
// fullConversationHeadersOnly array lists that session's turns in order, each
// pointing at a sibling "bubbleId:<composerId>:<bubbleUuid>" row holding the
// turn's text. conversationMap, the alternative inline-message field, was
// observed empty on every real (including large) conversation, so it is not a
// usable source in this schema version (_v: 16 composerData / _v: 3 bubbles).
const (
	cursorIDEIDPrefix = "cursor-ide:"
	// CursorIDEDBRelPath is the container's path relative to the provider
	// root: Cursor IDE's default dirs point straight at globalStorage, and
	// state.vscdb lives directly inside it (no subdirectory).
	CursorIDEDBRelPath         = "state.vscdb"
	cursorIDEComposerKeyPrefix = "composerData:"
	cursorIDEBubbleKeyPrefix   = "bubbleId:"

	// cursorIDEBubbleTypeUser and cursorIDEBubbleTypeAssistant are the
	// cursorDiskKV bubble "type" values.
	cursorIDEBubbleTypeUser      = 1
	cursorIDEBubbleTypeAssistant = 2
)

// cursorIDEDefaultDirs returns platform-specific default directories holding
// state.vscdb.
func cursorIDEDefaultDirs() []string {
	return []string{
		// macOS
		"Library/Application Support/Cursor/User/globalStorage",
		// Linux
		".config/Cursor/User/globalStorage",
		// Windows
		"AppData/Roaming/Cursor/User/globalStorage",
	}
}

func openCursorIDEDB(dbPath string) (*sql.DB, error) {
	db, err := openSQLiteReadOnly(dbPath, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, fmt.Errorf("opening cursor IDE db %s: %w", dbPath, err)
	}
	return db, nil
}

// cursorIDEQuerier is the shared read surface of *sql.DB and *sql.Tx: the
// composer readers accept it so one snapshot transaction can serve the
// composer document, its bubbles, and the content digest together.
type cursorIDEQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// beginCursorIDESnapshot opens one deferred read transaction on the
// read-only connection. Cursor keeps writing to state.vscdb while agentsview
// reads it; without a transaction every query sees its own implicit
// snapshot, so the digest could hash newer bubble bytes than the messages
// just parsed, and the freshness gates would then trust a digest that does
// not describe the stored transcript.
func beginCursorIDESnapshot(
	ctx context.Context, conn *sql.DB, composerID string,
) (*sql.Tx, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf(
			"beginning cursor IDE snapshot for %s: %w", composerID, err)
	}
	return tx, nil
}

// CursorIDEComposerExists reports whether a composerData row with the given
// composer ID exists in state.vscdb.
func CursorIDEComposerExists(dbPath, composerID string) bool {
	if dbPath == "" || composerID == "" || !IsValidSessionID(composerID) {
		return false
	}
	conn, err := openCursorIDEDB(dbPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	var one int
	err = conn.QueryRow(
		`SELECT 1 FROM cursorDiskKV WHERE key = ? LIMIT 1`,
		cursorIDEComposerKeyPrefix+composerID,
	).Scan(&one)
	return err == nil
}

// cursorIDEComposerHeader is one entry of fullConversationHeadersOnly: the
// bubble to look up and its rendered role, in transcript order.
type cursorIDEComposerHeader struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"`
}

type cursorIDEGitBranch struct {
	BranchName        string `json:"branchName"`
	LastInteractionAt int64  `json:"lastInteractionAt"`
}

type cursorIDEGitRepo struct {
	RepoPath string               `json:"repoPath"`
	Branches []cursorIDEGitBranch `json:"branches"`
}

type cursorIDEWorkspaceIdentifier struct {
	URI struct {
		FSPath string `json:"fsPath"`
	} `json:"uri"`
}

// cursorIDEComposerDoc is the composerData:<uuid> document. Only the fields
// the parser needs are modeled; the rest of the blob (rich editor state,
// capability flags, encryption keys for Cursor's own cloud sync, ...) is
// ignored.
type cursorIDEComposerDoc struct {
	Headers             []cursorIDEComposerHeader    `json:"fullConversationHeadersOnly"`
	Name                string                       `json:"name"`
	CreatedAt           int64                        `json:"createdAt"`
	LastUpdatedAt       int64                        `json:"lastUpdatedAt"`
	WorkspaceIdentifier cursorIDEWorkspaceIdentifier `json:"workspaceIdentifier"`
	TrackedGitRepos     []cursorIDEGitRepo           `json:"trackedGitRepos"`
}

// cursorIDEComposerMeta is a per-composer descriptor for the engine's
// freshness check: the composer's lastUpdatedAt plus a content digest over
// every byte the parser reads for that composer.
type cursorIDEComposerMeta struct {
	rawID         string
	lastUpdatedAt int64
	digest        string
}

// cursorIDEComposerDigest hashes every parse input of one composer: the raw
// composerData document (covering name, cwd, git branch, timestamps, and the
// header order) and the key plus value bytes of every stored bubble row in
// the composer's key range. Any committed edit to the composer -- including
// an equal-length in-place bubble rewrite, a header reorder, or a
// metadata-only change that leaves lastUpdatedAt untouched -- changes the
// digest, so the engine's unchanged-result filter and freshness gates cannot
// discard it. The range scan (":" to its successor ";") stays on the
// cursorDiskKV key index and only ever reads the one composer's rows, never
// the whole multi-hundred-MB database.
func cursorIDEComposerDigest(
	ctx context.Context, q cursorIDEQuerier, composerID string, rawComposer []byte,
) (string, error) {
	h := fnv.New64a()
	_, _ = h.Write(rawComposer)
	rows, err := q.QueryContext(ctx,
		`SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key`,
		cursorIDEBubbleKeyPrefix+composerID+":",
		cursorIDEBubbleKeyPrefix+composerID+";",
	)
	if err != nil {
		return "", fmt.Errorf(
			"digesting cursor IDE bubbles for %s: %w", composerID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return "", fmt.Errorf(
				"scanning cursor IDE bubble for digest %s: %w", composerID, err)
		}
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(value)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf(
			"digesting cursor IDE bubbles for %s: %w", composerID, err)
	}
	return strconv.FormatUint(h.Sum64(), 16), nil
}

func loadCursorIDEComposerMeta(
	ctx context.Context, conn *sql.DB, composerID string,
) (cursorIDEComposerMeta, bool, error) {
	tx, err := beginCursorIDESnapshot(ctx, conn, composerID)
	if err != nil {
		return cursorIDEComposerMeta{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT value FROM cursorDiskKV WHERE key = ?`,
		cursorIDEComposerKeyPrefix+composerID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return cursorIDEComposerMeta{}, false, nil
	}
	if err != nil {
		return cursorIDEComposerMeta{}, false, fmt.Errorf(
			"loading cursor IDE composer meta %s: %w", composerID, err)
	}
	if len(raw) == 0 {
		// A composerData key whose value is NULL or empty holds no session
		// document, so there is nothing to fingerprint. This is the same fact
		// as the vanished row above, not the malformed case below: an absent
		// value yields no shorter transcript that could replace the archived
		// one. The engine proceeds to Parse, which routes the stored session
		// to the recoverable source-missing seam while state.vscdb is present.
		return cursorIDEComposerMeta{}, false, nil
	}
	var doc cursorIDEComposerDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Malformed is not missing: a row that exists but no longer decodes
		// must fail loudly instead of fingerprinting as a vanished composer,
		// which would let a complete container parse replace the archived
		// session.
		return cursorIDEComposerMeta{}, false, fmt.Errorf(
			"decoding cursor IDE composer %s: %w", composerID, err)
	}
	digest, err := cursorIDEComposerDigest(ctx, tx, composerID, raw)
	if err != nil {
		return cursorIDEComposerMeta{}, false, err
	}
	return cursorIDEComposerMeta{
		rawID:         composerID,
		lastUpdatedAt: doc.LastUpdatedAt,
		digest:        digest,
	}, true, nil
}

// listCursorIDEComposerIDs returns every composer ID in state.vscdb.
func listCursorIDEComposerIDs(ctx context.Context, conn *sql.DB) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT key FROM cursorDiskKV WHERE key LIKE ? ORDER BY key`,
		cursorIDEComposerKeyPrefix+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("listing cursor IDE composers: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scanning cursor IDE composer key: %w", err)
		}
		id := strings.TrimPrefix(key, cursorIDEComposerKeyPrefix)
		if IsValidSessionID(id) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// cursorIDEToolFormerData is a bubble's embedded tool call: unlike Claude's
// separate call/result blocks, Cursor stores one tool invocation (input,
// status, and output) inline on the assistant bubble that issued it.
type cursorIDEToolFormerData struct {
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name"`
	RawArgs    string `json:"rawArgs"`
	Result     string `json:"result"`
}

type cursorIDEBubble struct {
	Type           int                      `json:"type"`
	Text           string                   `json:"text"`
	CreatedAt      string                   `json:"createdAt"`
	ToolFormerData *cursorIDEToolFormerData `json:"toolFormerData"`
}

func loadCursorIDEBubble(
	ctx context.Context, q cursorIDEQuerier, composerID, bubbleID string,
) (*cursorIDEBubble, error) {
	var raw []byte
	err := q.QueryRowContext(ctx,
		`SELECT value FROM cursorDiskKV WHERE key = ?`,
		cursorIDEBubbleKeyPrefix+composerID+":"+bubbleID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		// Cursor version updates have been observed to shrink or wipe rows
		// out of cursorDiskKV; a missing bubble is a gap in the transcript,
		// not a fatal error.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"loading cursor IDE bubble %s:%s: %w", composerID, bubbleID, err)
	}
	if len(raw) == 0 {
		// A bubbleId row whose value is NULL or empty carries no turn text,
		// which is the same gap as the missing row above. The caller flags the
		// transcript truncated, and the engine's truncation guard
		// (dropShrinkingTruncatedCursorIDEResults) still refuses any result
		// that would drop an archived message.
		return nil, nil
	}
	var bubble cursorIDEBubble
	if err := json.Unmarshal(raw, &bubble); err != nil {
		// Malformed is not missing: silently skipping a stored bubble that
		// no longer decodes would emit a truncated transcript that
		// force-replaces the archived full one.
		return nil, fmt.Errorf(
			"decoding cursor IDE bubble %s:%s: %w", composerID, bubbleID, err)
	}
	return &bubble, nil
}

// cursorIDEMessageFromBubble converts one decoded bubble into a
// ParsedMessage. Returns ok=false for a bubble with no visible content (pure
// bookkeeping bubbles such as in-flight generation placeholders).
func cursorIDEMessageFromBubble(ordinal int, bubble cursorIDEBubble) (ParsedMessage, bool) {
	content := strings.TrimSpace(bubble.Text)
	msg := ParsedMessage{
		Ordinal:       ordinal,
		ContentLength: len(content),
		Timestamp:     parseTimestamp(bubble.CreatedAt),
	}

	switch bubble.Type {
	case cursorIDEBubbleTypeUser:
		if content == "" {
			return ParsedMessage{}, false
		}
		msg.Role = RoleUser
		msg.Content = content
		return msg, true

	case cursorIDEBubbleTypeAssistant:
		msg.Role = RoleAssistant
		msg.Content = content
		if bubble.ToolFormerData == nil {
			if content == "" {
				return ParsedMessage{}, false
			}
			return msg, true
		}
		tfd := bubble.ToolFormerData
		msg.HasToolUse = true
		msg.ToolCalls = []ParsedToolCall{{
			ToolUseID: tfd.ToolCallID,
			ToolName:  tfd.Name,
			Category:  NormalizeToolCategory(tfd.Name),
			InputJSON: tfd.RawArgs,
		}}
		if tfd.Result != "" {
			quoted, err := json.Marshal(tfd.Result)
			if err == nil {
				msg.ToolResults = []ParsedToolResult{{
					ToolUseID:     tfd.ToolCallID,
					ContentLength: len(tfd.Result),
					ContentRaw:    string(quoted),
				}}
			}
		}
		return msg, true

	default:
		return ParsedMessage{}, false
	}
}

// cursorIDELatestBranch picks the branch with the most recent
// lastInteractionAt across every tracked git repo, matching the workspace
// picker Cursor itself shows.
func cursorIDELatestBranch(repos []cursorIDEGitRepo) string {
	var branch string
	var latest int64
	for _, repo := range repos {
		for _, b := range repo.Branches {
			if b.BranchName == "" {
				continue
			}
			if branch == "" || b.LastInteractionAt > latest {
				branch = b.BranchName
				latest = b.LastInteractionAt
			}
		}
	}
	return branch
}

func cursorIDECwd(doc cursorIDEComposerDoc) string {
	if fsPath := doc.WorkspaceIdentifier.URI.FSPath; fsPath != "" {
		return fsPath
	}
	if len(doc.TrackedGitRepos) > 0 {
		return doc.TrackedGitRepos[0].RepoPath
	}
	return ""
}

// parseCursorIDEComposer decodes one composer into a ParseResult using an
// already-open connection. Returns (nil, nil) for a composer with zero
// renderable messages (an empty draft, or one whose bubbles were all wiped by
// a Cursor version update).
func parseCursorIDEComposer(
	ctx context.Context, conn *sql.DB, dbPath, composerID, machine string,
	dbInfo os.FileInfo,
) (*ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := beginCursorIDESnapshot(ctx, conn, composerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT value FROM cursorDiskKV WHERE key = ?`,
		cursorIDEComposerKeyPrefix+composerID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"loading cursor IDE composer %s: %w", composerID, err)
	}
	if len(raw) == 0 {
		// Same husk row as in loadCursorIDEComposerMeta: no document, so no
		// result. A nil result keeps the container fan-out running for every
		// sibling composer instead of failing the whole pass, and the stored
		// session is preserved through the source-missing seam.
		return nil, nil
	}
	var doc cursorIDEComposerDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Malformed is not missing: a nil result would read as a clean
		// no-session and retire the archived transcript.
		return nil, fmt.Errorf(
			"decoding cursor IDE composer %s: %w", composerID, err)
	}
	if len(doc.Headers) == 0 {
		return nil, nil
	}

	var messages []ParsedMessage
	truncated := false
	for _, header := range doc.Headers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bubble, err := loadCursorIDEBubble(ctx, tx, composerID, header.BubbleID)
		if err != nil {
			return nil, err
		}
		if bubble == nil {
			// The transcript has a gap: a header references a bubble row
			// that was never written or was wiped. Surface the remaining
			// content, flagged truncated so the engine will not let it
			// shrink an already archived fuller transcript.
			truncated = true
			continue
		}
		if bubble.Type == 0 {
			// A shrunk or partially-written bubble row can lose its type
			// field while the header still records the turn's role; fall
			// back so the turn is not silently dropped. A bubble that
			// carries its own type stays authoritative.
			bubble.Type = header.Type
		}
		msg, ok := cursorIDEMessageFromBubble(len(messages), *bubble)
		if !ok {
			if bubble.Type == cursorIDEBubbleTypeUser ||
				bubble.Type == cursorIDEBubbleTypeAssistant {
				// A conversational bubble that exists but renders nothing is
				// indistinguishable from a row emptied in place (e.g. "{}"),
				// so it takes the same truncation flag as a missing row and
				// the engine verifies against the archive before the
				// transcript can lose an archived turn. In-flight generation
				// placeholders flag transiently and clear on the next parse;
				// non-conversational bookkeeping bubble types stay unflagged.
				truncated = true
			}
			continue
		}
		// The bubble UUID is each message's stable identity; the engine's
		// truncation guard compares it against the archived messages so a
		// gap transcript can never drop archived content unnoticed.
		msg.SourceUUID = header.BubbleID
		messages = append(messages, msg)
	}
	if len(messages) == 0 {
		return nil, nil
	}

	var firstMessage string
	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser {
			userCount++
			if firstMessage == "" && m.Content != "" {
				firstMessage = truncate(
					strings.ReplaceAll(m.Content, "\n", " "), 300,
				)
			}
		}
	}

	startedAt := cursorIDETime(doc.CreatedAt)
	if startedAt.IsZero() && len(messages) > 0 {
		startedAt = messages[0].Timestamp
	}
	// lastUpdatedAt has been observed lagging behind the bubbles' own
	// timestamps, so the session ends at the later of the two: a stale
	// composer stamp must not place EndedAt before the final message.
	endedAt := cursorIDETime(doc.LastUpdatedAt)
	for _, m := range messages {
		if m.Timestamp.After(endedAt) {
			endedAt = m.Timestamp
		}
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	digest, err := cursorIDEComposerDigest(ctx, tx, composerID, raw)
	if err != nil {
		return nil, err
	}
	cwd := cursorIDECwd(doc)
	sess := ParsedSession{
		ID:               cursorIDEIDPrefix + composerID,
		Agent:            AgentCursorIDE,
		Machine:          machine,
		Project:          ExtractProjectFromCwd(cwd),
		Cwd:              cwd,
		GitBranch:        cursorIDELatestBranch(doc.TrackedGitRepos),
		SessionName:      doc.Name,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		IsTruncated:      truncated,
		File: FileInfo{
			Path:  VirtualSourcePath(dbPath, composerID),
			Size:  dbInfo.Size(),
			Mtime: endedAt.UnixNano(),
			Hash:  digest,
		},
	}

	return &ParseResult{Session: sess, Messages: messages}, nil
}

// cursorIDETime converts an epoch-milliseconds stamp (composerData
// createdAt/lastUpdatedAt) to UTC. Zero stays zero. Bubble timestamps use a
// different encoding (ISO-8601 strings) and go through parseTimestamp
// instead.
func cursorIDETime(epochMS int64) time.Time {
	if epochMS == 0 {
		return time.Time{}
	}
	return time.UnixMilli(epochMS).UTC()
}
