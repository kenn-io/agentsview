package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/shellquote"
)

// PRLink is a pull or merge request named in a structured record or retained
// user prompt.
type PRLink = parser.PRLink

// EncodePRLinks returns the stored text form of links: a JSON array, or
// the empty string when there are none.
func EncodePRLinks(links []PRLink) string {
	if len(links) == 0 {
		return ""
	}
	b, err := json.Marshal(links)
	if err != nil {
		return ""
	}
	return string(b)
}

var malformedPRLinksWarning sync.Once

// DecodePRLinks parses the stored text form. Empty or malformed text
// decodes to nil so one damaged row cannot break a session listing.
func DecodePRLinks(text string) []PRLink {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var links []PRLink
	if err := json.Unmarshal([]byte(text), &links); err != nil {
		malformedPRLinksWarning.Do(func() { log.Print("invalid stored PR-link JSON; links omitted") })
		return nil
	}
	if len(links) == 0 {
		return nil
	}
	return links
}

// prLinksColumn scans the stored pr_links text into a slice.
type prLinksColumn struct{ dst *[]PRLink }

func (c prLinksColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.dst = nil
	case string:
		*c.dst = DecodePRLinks(v)
	case []byte:
		*c.dst = DecodePRLinks(string(v))
	default:
		return fmt.Errorf("scanning pr_links: unsupported type %T", src)
	}
	return nil
}

// PRFilter selects sessions by repository or pull request number, optionally narrowed to one host.
type PRFilter struct {
	Host       string
	Repository string
	Number     int
}

// IsZero reports whether the filter selects nothing.
func (f PRFilter) IsZero() bool { return f.Repository == "" && f.Number <= 0 }

// ErrInvalidPRFilter identifies a pull request filter ParsePRFilter rejects.
var ErrInvalidPRFilter = errors.New("invalid pr filter")

// ParsePRFilter accepts #123, owner/repo[#123], or host::owner/repo[#123].
func ParsePRFilter(value string) (PRFilter, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return PRFilter{}, nil
	}
	repo, num, hasNum := strings.Cut(value, "#")
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	host, path, qualified := strings.Cut(repo, "::")
	if qualified {
		repo = strings.Trim(path, "/")
		if host == "" || strings.ContainsAny(host, "/ \t\r\n") {
			return PRFilter{}, fmt.Errorf("%w %q: invalid host", ErrInvalidPRFilter, value)
		}
	}
	if (repo != "" || !hasNum || qualified) && (!strings.Contains(repo, "/") || strings.Contains(repo, "://")) {
		return PRFilter{}, fmt.Errorf(
			"%w %q: want #123, owner/repo, owner/repo#123, or host::owner/repo#123", ErrInvalidPRFilter, value,
		)
	}
	f := PRFilter{Repository: strings.ToLower(repo)}
	if qualified {
		f.Host = strings.ToLower(host)
	}
	if hasNum {
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil || n <= 0 {
			return PRFilter{}, fmt.Errorf(
				"%w %q: invalid pull request number", ErrInvalidPRFilter, value,
			)
		}
		f.Number = n
	}
	return f, nil
}

// labelsColumn scans a JSON array of labels into a slice.
type labelsColumn struct{ dst *[]string }

// LabelsScanner scans a JSON array of labels into dst. An empty array scans
// to nil, the value an unlabeled session carries on every backend.
func LabelsScanner(dst *[]string) sql.Scanner { return labelsColumn{dst} }

// LabelsArg returns labels as a non-nil slice for mirrors whose labels
// column is a non-null array, so an unlabeled session writes an empty array
// and fingerprints the same as one whose labels were all removed.
func LabelsArg(labels []string) []string {
	if labels == nil {
		return []string{}
	}
	return labels
}

func (c labelsColumn) Scan(src any) error {
	var text string
	switch v := src.(type) {
	case nil:
		*c.dst = nil
		return nil
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return fmt.Errorf("scanning labels: unsupported type %T", src)
	}
	var labels []string
	if err := json.Unmarshal([]byte(text), &labels); err != nil {
		return fmt.Errorf("scanning labels: %w", err)
	}
	if len(labels) == 0 {
		labels = nil
	}
	*c.dst = labels
	return nil
}

// GetSessionPRLinkURLs returns the structured pull request URLs for a session.
// A missing session yields an empty map.
func (db *DB) GetSessionPRLinkURLs(
	ctx context.Context, sessionID string,
) (map[string]struct{}, error) {
	var text string
	err := db.getReader().QueryRowContext(ctx,
		"SELECT structured_pr_links FROM sessions WHERE id = ?", sessionID,
	).Scan(&text)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reading pr links for %s: %w", sessionID, err)
	}
	seen := make(map[string]struct{})
	for _, link := range DecodePRLinks(text) {
		seen[link.URL] = struct{}{}
	}
	return seen, nil
}

// PromptPRLinks finds links in retained user dialogue.
func PromptPRLinks(messages []Message) []PRLink {
	links, _ := PromptPRLinksContext(context.Background(), messages)
	return links
}

// PromptPRLinksContext allows hosted projection to stop when its deadline expires.
func PromptPRLinksContext(ctx context.Context, messages []Message) ([]PRLink, error) {
	var lists [][]PRLink
	for _, msg := range messages {
		if msg.Role != "user" || msg.IsSystem || msg.IsCompactBoundary || msg.SourceSubtype == parser.SourceSubtypeToolResult || IsSystemPrefixed(msg.Content, msg.Role) {
			continue
		}
		content := msg.Content
		if msg.DialogueText != nil {
			content = *msg.DialogueText
		}
		trimmed := strings.TrimSpace(content)
		if strings.HasPrefix(trimmed, "<bash-stdout>") || strings.HasPrefix(trimmed, "<bash-stderr>") {
			continue
		}
		content = withoutSystemReminders(content)
		links, err := parser.PromptPRLinksContext(ctx, content)
		if err != nil {
			return nil, err
		}
		lists = append(lists, links)
	}
	return parser.CombinePRLinks(lists...), nil
}

func withoutSystemReminders(text string) string {
	const open = "<system-reminder"
	var result strings.Builder
	for {
		start := strings.Index(text, open)
		if start < 0 {
			break
		}
		endName := start + len(open)
		if endName == len(text) || !strings.ContainsRune("> \t\r\n", rune(text[endName])) {
			result.WriteString(text[:endName])
			text = text[endName:]
			continue
		}
		tagEnd := strings.IndexByte(text[endName:], '>')
		if tagEnd < 0 {
			break
		}
		body := endName + tagEnd + 1
		end := strings.Index(text[body:], systemReminderCloseTag)
		if end < 0 {
			break
		}
		result.WriteString(text[:start])
		result.WriteByte(' ')
		text = text[body+end+len(systemReminderCloseTag):]
	}
	result.WriteString(text)
	return result.String()
}

func writeSessionPromptPRLinksTx(tx transactionQueries, id, stored string, links []PRLink) (bool, error) {
	encoded := EncodePRLinks(links)
	if encoded == stored {
		return false, nil
	}
	_, err := tx.Exec(`UPDATE sessions SET pr_links=?, local_modified_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, encoded, id)
	return err == nil, err
}

const promptPRLinkCandidateSQL = `role='user' AND (COALESCE(` + sqliteDialogueExprBare + `, content) LIKE '%http://%' OR COALESCE(` + sqliteDialogueExprBare + `, content) LIKE '%https://%')`

func refreshSessionPromptPRLinksTx(ctx context.Context, tx transactionQueries, id string) error {
	_, err := refreshSessionPromptPRLinksChangedTx(ctx, tx, id)
	return err
}

func refreshSessionPromptPRLinksChangedTx(ctx context.Context, tx transactionQueries, id string) (bool, error) {
	var stored, structured string
	if err := tx.QueryRow(`SELECT pr_links, structured_pr_links FROM sessions WHERE id=?`, id).Scan(&stored, &structured); err != nil {
		return false, err
	}
	rows, err := tx.Query(`SELECT `+selectMessageCols+` FROM messages WHERE session_id=? AND `+promptPRLinkCandidateSQL+` ORDER BY ordinal`, id)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	messages, err := scanMessages(rows)
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	tools, err := storedToolCallPRLinksTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	links := parser.CombinePRLinks(DecodePRLinks(structured), PromptPRLinks(messages), tools)
	return writeSessionPromptPRLinksTx(tx, id, stored, links)
}

func appendSessionPromptPRLinksTx(ctx context.Context, tx transactionQueries, id string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	prompts, err := PromptPRLinksContext(ctx, messages)
	if err != nil {
		return err
	}
	tools, err := ToolCallPRLinks(ctx, messages)
	if err != nil {
		return err
	}
	derived := parser.CombinePRLinks(prompts, tools)
	if len(derived) == 0 {
		return nil
	}
	var stored string
	if err := tx.QueryRow(`SELECT pr_links FROM sessions WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	_, err = writeSessionPromptPRLinksTx(tx, id, stored, parser.CombinePRLinks(DecodePRLinks(stored), derived))
	return err
}

// GetSessionStructuredPRLinks returns parser-recorded links without prompt links.
func (db *DB) GetSessionStructuredPRLinks(ctx context.Context, id string) ([]PRLink, error) {
	var encoded string
	if err := db.getReader().QueryRowContext(ctx, `SELECT structured_pr_links FROM sessions WHERE id=?`, id).Scan(&encoded); err != nil {
		return nil, err
	}
	return DecodePRLinks(encoded), nil
}

// promptPRLinksDataVersion is the first data version whose pr_links include
// links found in retained prompts.
const promptPRLinksDataVersion = 184

const (
	prLinksBackfillStateKey   = "pr_links_backfill_state"
	prLinksBackfillVersion    = "1"
	prLinksBackfillScopeAll   = "all"
	prLinksBackfillScopeTools = "tools"
	prLinksBackfillBatchSize  = 256
)

func formatPRLinksBackfillState(scope, cursor string) string {
	return prLinksBackfillVersion + ":" + scope + ":" + cursor
}

func parsePRLinksBackfillState(raw string) (scope, cursor string, known bool) {
	version, rest, ok := strings.Cut(raw, ":")
	if !ok || version != prLinksBackfillVersion {
		return "", "", false
	}
	scope, cursor, ok = strings.Cut(rest, ":")
	if !ok || (scope != prLinksBackfillScopeAll && scope != prLinksBackfillScopeTools) {
		return "", "", false
	}
	return scope, cursor, true
}

// upgradePRLinksTx copies legacy structured links and queues a resumable
// PR-link backfill before user_version advances. Older binaries may have
// rewritten pr_links without updating structured_pr_links, so copying inside
// the version transaction keeps legacy structured links intact while the
// derivation itself runs in batches after startup (or inline for non-daemon
// opens).
func (db *DB) upgradePRLinksTx(ctx context.Context, tx *sql.Tx) error {
	version, _, err := readUserVersion(ctx, tx)
	if err != nil || version >= toolPRLinksDataVersion {
		return err
	}
	if version < promptPRLinksDataVersion {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET structured_pr_links=pr_links WHERE structured_pr_links IS NOT pr_links`); err != nil {
			return fmt.Errorf("copying structured PR links: %w", err)
		}
	}
	if db.usageOnlyStorage() {
		return nil
	}
	var hasSessions bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions)`).Scan(&hasSessions); err != nil {
		return fmt.Errorf("checking sessions for PR link backfill: %w", err)
	}
	if !hasSessions {
		return nil
	}
	if version < promptPRLinksDataVersion {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO stats(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			prLinksBackfillStateKey, formatPRLinksBackfillState(prLinksBackfillScopeAll, ""),
		)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO stats(key, value) VALUES (?, ?)`,
			prLinksBackfillStateKey, formatPRLinksBackfillState(prLinksBackfillScopeTools, ""),
		)
	}
	if err != nil {
		return fmt.Errorf("queueing PR link backfill: %w", err)
	}
	return nil
}

// PRLinksBackfillPending reports whether a deferred PR-link backfill is owed.
func (db *DB) PRLinksBackfillPending() bool {
	return db.prLinksBackfillPending.Load()
}

// preparePRLinksBackfill checks whether a PR-link backfill is pending and
// either defers it for the daemon or finishes it synchronously before Open
// returns.
func (db *DB) preparePRLinksBackfill(ctx context.Context, progress OpenProgressFunc, deferBackfill bool) error {
	if db.usageOnlyStorage() {
		db.prLinksBackfillPending.Store(false)
		return nil
	}
	var raw string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT CAST(value AS TEXT) FROM stats WHERE key = ?`,
		prLinksBackfillStateKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		db.prLinksBackfillPending.Store(false)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading PR link backfill state: %w", err)
	}
	if _, _, known := parsePRLinksBackfillState(raw); !known {
		db.prLinksBackfillPending.Store(false)
		return nil
	}
	db.prLinksBackfillPending.Store(true)
	if deferBackfill {
		return nil
	}
	progress.report("Backfilling pull request links")
	return db.BackfillPRLinks(ctx)
}

// BackfillPRLinks derives prompt and tool-call pull request links for sessions
// queued by an archive upgrade. Each batch reads and parses on the reader pool
// without holding the write lock, then commits only changed sessions and
// advances its durable cursor in stats so restarts resume where they left off.
func (db *DB) BackfillPRLinks(ctx context.Context) error {
	if !db.prLinksBackfillPending.Load() || db.usageOnlyStorage() {
		return nil
	}
	start := time.Now()
	var totalScanned, totalUpdated int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		scanned, updated, done, err := db.backfillPRLinksBatch(ctx)
		if err != nil {
			return err
		}
		totalScanned += scanned
		totalUpdated += updated
		if done {
			if totalScanned > 0 || totalUpdated > 0 {
				log.Printf(
					"backfill: pull request links scanned %d sessions, updated %d in %s",
					totalScanned, totalUpdated, time.Since(start).Round(time.Millisecond),
				)
			}
			return nil
		}
	}
}

type prLinkBackfillSession struct {
	id         string
	stored     string
	structured string
}

func (db *DB) backfillPRLinksBatch(ctx context.Context) (scanned, updated int, done bool, err error) {
	if db.usageOnlyStorage() {
		db.prLinksBackfillPending.Store(false)
		return 0, 0, true, nil
	}
	rawState, scope, batch, dirtyIDs, done, err := db.scanPRLinksBackfillBatch(ctx)
	if err != nil || done {
		if done && err == nil {
			db.prLinksBackfillPending.Store(false)
		}
		return 0, 0, done, err
	}
	batchDone, batchUpdated, err := db.commitPRLinksBackfillBatch(ctx, rawState, scope, batch, dirtyIDs)
	if err != nil {
		return 0, 0, false, err
	}
	if batchDone {
		db.prLinksBackfillPending.Store(false)
	}
	return len(batch), batchUpdated, batchDone, nil
}

func (db *DB) scanPRLinksBackfillBatch(ctx context.Context) (
	rawState, scope string, batch []prLinkBackfillSession, dirtyIDs []string, done bool, err error,
) {
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return "", "", nil, nil, false, err
	}
	defer release()

	r := db.getReader()
	err = r.QueryRowContext(ctx,
		`SELECT CAST(value AS TEXT) FROM stats WHERE key = ?`,
		prLinksBackfillStateKey,
	).Scan(&rawState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil, nil, true, nil
	}
	if err != nil {
		return "", "", nil, nil, false, fmt.Errorf("reading PR link backfill state: %w", err)
	}
	var cursor string
	var known bool
	scope, cursor, known = parsePRLinksBackfillState(rawState)
	if !known {
		return "", "", nil, nil, true, nil
	}

	rows, err := r.QueryContext(ctx,
		`SELECT id, pr_links, structured_pr_links FROM sessions WHERE id > ? ORDER BY id LIMIT ?`,
		cursor, prLinksBackfillBatchSize,
	)
	if err != nil {
		return "", "", nil, nil, false, fmt.Errorf("listing PR link backfill batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s prLinkBackfillSession
		if err := rows.Scan(&s.id, &s.stored, &s.structured); err != nil {
			return "", "", nil, nil, false, fmt.Errorf("scanning PR link backfill session: %w", err)
		}
		batch = append(batch, s)
	}
	if err := rows.Err(); err != nil {
		return "", "", nil, nil, false, err
	}
	if err := rows.Close(); err != nil {
		return "", "", nil, nil, false, err
	}
	if len(batch) == 0 {
		return rawState, scope, nil, nil, false, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
	args := make([]any, len(batch))
	for i, s := range batch {
		args[i] = s.id
	}

	toolLinks, err := scanBatchToolCallPRLinks(ctx, r, placeholders, args)
	if err != nil {
		return "", "", nil, nil, false, err
	}
	var promptLinks map[string][]PRLink
	if scope == prLinksBackfillScopeAll {
		promptLinks, err = scanBatchPromptPRLinks(ctx, r, placeholders, args)
		if err != nil {
			return "", "", nil, nil, false, err
		}
	}

	for _, s := range batch {
		if err := ctx.Err(); err != nil {
			return "", "", nil, nil, false, err
		}
		var derived []PRLink
		if scope == prLinksBackfillScopeAll {
			derived = parser.CombinePRLinks(DecodePRLinks(s.structured), promptLinks[s.id], toolLinks[s.id])
		} else {
			tools := toolLinks[s.id]
			if len(tools) == 0 {
				continue
			}
			derived = parser.CombinePRLinks(DecodePRLinks(s.stored), tools)
		}
		if EncodePRLinks(derived) != s.stored {
			dirtyIDs = append(dirtyIDs, s.id)
		}
	}
	return rawState, scope, batch, dirtyIDs, false, nil
}

func scanBatchToolCallPRLinks(ctx context.Context, r *readerHandle, placeholders string, args []any) (map[string][]PRLink, error) {
	rows, err := r.QueryContext(ctx,
		`SELECT session_id, category, input_json FROM tool_calls WHERE session_id IN (`+placeholders+`) AND category IN ('Bash','Task') AND input_json IS NOT NULL`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("scanning batch tool calls for PR links: %w", err)
	}
	defer rows.Close()
	bySession := make(map[string][]PRLink)
	for rows.Next() {
		var sessionID, category, inputJSON string
		if err := rows.Scan(&sessionID, &category, &inputJSON); err != nil {
			return nil, fmt.Errorf("scanning tool call for PR links: %w", err)
		}
		links, err := callPRLinks(ctx, category, inputJSON)
		if err != nil {
			return nil, err
		}
		if len(links) > 0 {
			bySession[sessionID] = parser.CombinePRLinks(bySession[sessionID], links)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bySession, rows.Close()
}

func scanBatchPromptPRLinks(ctx context.Context, r *readerHandle, placeholders string, args []any) (map[string][]PRLink, error) {
	rows, err := r.QueryContext(ctx,
		`SELECT session_id, role, is_system, is_compact_boundary, source_subtype, content, `+sqliteDialogueExprBare+`
		 FROM messages
		 WHERE session_id IN (`+placeholders+`)
		   AND is_system = 0
		   AND is_compact_boundary = 0
		   AND source_subtype <> 'tool_result'
		   AND `+promptPRLinkCandidateSQL+`
		 ORDER BY session_id, ordinal`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("scanning batch messages for PR links: %w", err)
	}
	defer rows.Close()
	bySession := make(map[string][]PRLink)
	for rows.Next() {
		var msg Message
		var dialogue sql.NullString
		if err := rows.Scan(
			&msg.SessionID, &msg.Role, &msg.IsSystem, &msg.IsCompactBoundary,
			&msg.SourceSubtype, &msg.Content, &dialogue,
		); err != nil {
			return nil, fmt.Errorf("scanning message for PR links: %w", err)
		}
		if dialogue.Valid {
			msg.DialogueText = &dialogue.String
		}
		links, err := PromptPRLinksContext(ctx, []Message{msg})
		if err != nil {
			return nil, err
		}
		if len(links) > 0 {
			bySession[msg.SessionID] = parser.CombinePRLinks(bySession[msg.SessionID], links)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bySession, rows.Close()
}

func (db *DB) commitPRLinksBackfillBatch(
	ctx context.Context, rawState, scope string, batch []prLinkBackfillSession, dirtyIDs []string,
) (done bool, updated int, err error) {
	type commitResult struct {
		done    bool
		updated int
	}
	res, err := RetryAcrossHandoff(ctx, db, func() (commitResult, error) {
		if err := db.lockWriterCtx(ctx); err != nil {
			return commitResult{}, err
		}
		defer db.mu.Unlock()
		if db.writerClosed.Load() {
			return commitResult{}, ErrWriterClosed
		}
		if db.usageOnlyStorage() {
			return commitResult{done: true}, nil
		}
		tx, err := db.getWriter().BeginTx(ctx, nil)
		if err != nil {
			return commitResult{}, err
		}
		defer func() { _ = tx.Rollback() }()
		// Lock before reading state so concurrent writers or restarts cannot
		// race on cursor progression.
		if _, err := tx.ExecContext(ctx, `UPDATE stats SET value = value WHERE key = ?`, prLinksBackfillStateKey); err != nil {
			return commitResult{}, err
		}
		var currentState string
		err = tx.QueryRowContext(ctx,
			`SELECT CAST(value AS TEXT) FROM stats WHERE key = ?`,
			prLinksBackfillStateKey,
		).Scan(&currentState)
		if errors.Is(err, sql.ErrNoRows) {
			return commitResult{done: true}, tx.Commit()
		}
		if err != nil {
			return commitResult{}, fmt.Errorf("re-reading PR link backfill state: %w", err)
		}
		if currentState != rawState {
			return commitResult{done: false}, tx.Commit()
		}
		var batchUpdated int
		txq := contextTransaction{ctx: ctx, tx: tx}
		for _, id := range dirtyIDs {
			changed, err := refreshSessionPromptPRLinksChangedTx(ctx, txq, id)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return commitResult{}, fmt.Errorf("backfilling PR links: %w", err)
			}
			if changed {
				batchUpdated++
			}
		}
		if len(batch) < prLinksBackfillBatchSize {
			if _, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE key = ?`, prLinksBackfillStateKey); err != nil {
				return commitResult{}, fmt.Errorf("clearing PR link backfill state: %w", err)
			}
			return commitResult{done: true, updated: batchUpdated}, tx.Commit()
		}
		nextState := formatPRLinksBackfillState(scope, batch[len(batch)-1].id)
		if _, err := tx.ExecContext(ctx, `UPDATE stats SET value = ? WHERE key = ?`, nextState, prLinksBackfillStateKey); err != nil {
			return commitResult{}, fmt.Errorf("advancing PR link backfill cursor: %w", err)
		}
		return commitResult{done: false, updated: batchUpdated}, tx.Commit()
	})
	return res.done, res.updated, err
}

const maxPRCommandInputBytes = 256 * 1024

func callPRLinks(ctx context.Context, category, input string) ([]PRLink, error) {
	if category == "Task" {
		if !strings.Contains(input, "://") && !strings.Contains(input, `\u`) {
			return nil, ctx.Err()
		}
		return taskPRLinks(ctx, input)
	}
	if category != "Bash" || len(input) > maxPRCommandInputBytes {
		return nil, ctx.Err()
	}
	if !strings.Contains(input, `\u`) && !parser.MayContainPRCommand(input) {
		return nil, ctx.Err()
	}
	commands, ok := shellInputCommands(input)
	if !ok {
		return nil, ctx.Err()
	}
	var lists [][]PRLink
	for _, command := range commands {
		links, err := parser.AnalyzePRCommand(ctx, command)
		if err != nil {
			return nil, err
		}
		lists = append(lists, links)
	}
	return parser.CombinePRLinks(lists...), nil
}

// taskPRLinks finds links in the prompt fields of a delegated task, including
// each agent a spawn names, and ignores its descriptions.
func taskPRLinks(ctx context.Context, input string) ([]PRLink, error) {
	var payload struct {
		Prompt  string         `json:"prompt"`
		Task    string         `json:"task"`
		Message string         `json:"message"`
		Content jsontext.Value `json:"content"`
		Agents  []struct {
			Prompt string `json:"prompt"`
		} `json:"agents"`
	}
	if json.Unmarshal([]byte(input), &payload) != nil {
		return nil, ctx.Err()
	}
	var content string
	_ = json.Unmarshal(payload.Content, &content)
	text := []string{payload.Prompt, payload.Message, payload.Task, content}
	for _, agent := range payload.Agents {
		text = append(text, agent.Prompt)
	}
	return parser.PromptPRLinksContext(ctx, strings.Join(text, "\n"))
}

// shellInputCommands lists literal commands in a shell tool input.
func shellInputCommands(input string) ([]string, bool) {
	if !jsontext.Value(input).IsValid() {
		if commitToolCallRE.MatchString(input) {
			return literalCommands(input), true
		}
		text := strings.TrimSpace(input)
		return []string{input}, text != ""
	}
	var payload struct {
		Command     jsontext.Value `json:"command"`
		CommandLine string         `json:"CommandLine"`
		Cmd         string         `json:"cmd"`
		Commands    []string       `json:"commands"`
	}
	if json.Unmarshal([]byte(input), &payload) != nil {
		return nil, false
	}
	if len(payload.Command) > 0 && string(payload.Command) != "null" {
		command, ok := commandValue(payload.Command)
		return []string{command}, ok
	}
	if payload.CommandLine != "" {
		return []string{payload.CommandLine}, true
	}
	if payload.Commands != nil {
		return payload.Commands, true
	}
	return []string{payload.Cmd}, true
}

// commandValue decodes a command given as shell text or as an argv list.
func commandValue(raw jsontext.Value) (string, bool) {
	var command string
	if json.Unmarshal(raw, &command) == nil {
		return command, true
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil || len(argv) == 0 {
		return "", false
	}
	for i, arg := range argv {
		argv[i] = shellquote.POSIX(arg)
	}
	return strings.Join(argv, " "), true
}

// ToolCallPRLinks derives links from each independent tool call.
func ToolCallPRLinks(ctx context.Context, messages []Message) ([]PRLink, error) {
	var lists [][]PRLink
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			links, err := callPRLinks(ctx, call.Category, call.InputJSON)
			if err != nil {
				return nil, err
			}
			lists = append(lists, links)
		}
	}
	return parser.CombinePRLinks(lists...), nil
}

func storedToolCallsTx(tx transactionQueries, id string) ([]ToolCall, error) {
	rows, err := tx.Query(`SELECT t.category, t.tool_name, COALESCE(t.input_json,'') FROM tool_calls t JOIN messages m ON m.id=t.message_id WHERE t.session_id=? AND t.category IN ('Bash','Task') AND t.input_json IS NOT NULL ORDER BY m.ordinal, t.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []ToolCall
	for rows.Next() {
		var call ToolCall
		if err := rows.Scan(&call.Category, &call.ToolName, &call.InputJSON); err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

func storedToolCallPRLinksTx(ctx context.Context, tx transactionQueries, id string) ([]PRLink, error) {
	calls, err := storedToolCallsTx(tx, id)
	if err != nil {
		return nil, err
	}
	return ToolCallPRLinks(ctx, []Message{{ToolCalls: calls}})
}

const toolPRLinksDataVersion = 190
