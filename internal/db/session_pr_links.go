package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/parser"
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

// PRLinksScanner scans pr_links text written by EncodePRLinks into dst, for
// mirrors that store the same encoding.
func PRLinksScanner(dst *[]PRLink) sql.Scanner { return prLinksColumn{dst} }

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

// GetSessionPRLinkURLs returns the pull request URLs stored for a session.
// A missing session yields an empty map.
func (db *DB) GetSessionPRLinkURLs(
	ctx context.Context, sessionID string,
) (map[string]struct{}, error) {
	var text string
	err := db.getReader().QueryRowContext(ctx,
		"SELECT pr_links FROM sessions WHERE id = ?", sessionID,
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
	var lists [][]PRLink
	for _, msg := range messages {
		if msg.Role != "user" || msg.IsSystem || msg.IsCompactBoundary || msg.SourceSubtype == parser.SourceSubtypeToolResult || IsSystemPrefixed(msg.Content, msg.Role) {
			continue
		}
		content := msg.Content
		if msg.DialogueText != nil {
			content = *msg.DialogueText
		}
		for {
			start := strings.Index(content, systemReminderOpenTag)
			if start < 0 {
				break
			}
			end := strings.Index(content[start+len(systemReminderOpenTag):], systemReminderCloseTag)
			if end < 0 {
				break
			}
			end += start + len(systemReminderOpenTag) + len(systemReminderCloseTag)
			content = content[:start] + " " + content[end:]
		}
		trimmed := strings.TrimSpace(content)
		if strings.HasPrefix(trimmed, "<bash-stdout>") || strings.HasPrefix(trimmed, "<bash-stderr>") {
			continue
		}
		lists = append(lists, parser.PromptPRLinks(content))
	}
	return parser.MergePRLinks(lists...)
}

func writeSessionPromptPRLinksTx(tx transactionQueries, id, stored string, links []PRLink) error {
	encoded := EncodePRLinks(links)
	if encoded == stored {
		return nil
	}
	_, err := tx.Exec(`UPDATE sessions SET pr_links=?, local_modified_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, encoded, id)
	return err
}

func refreshSessionPromptPRLinksTx(tx transactionQueries, id string) error {
	var stored, structured string
	if err := tx.QueryRow(`SELECT pr_links, structured_pr_links FROM sessions WHERE id=?`, id).Scan(&stored, &structured); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT m.role, m.is_system, m.is_compact_boundary, m.source_subtype, m.content, `+sqliteDialogueExprM+` FROM messages m WHERE m.session_id=? AND m.role='user' ORDER BY m.ordinal`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var msg Message
		var dialogue sql.NullString
		if err := rows.Scan(&msg.Role, &msg.IsSystem, &msg.IsCompactBoundary, &msg.SourceSubtype, &msg.Content, &dialogue); err != nil {
			rows.Close()
			return err
		}
		if dialogue.Valid {
			msg.DialogueText = &dialogue.String
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return writeSessionPromptPRLinksTx(tx, id, stored, parser.MergePRLinks(DecodePRLinks(structured), PromptPRLinks(messages)))
}

func appendSessionPromptPRLinksTx(tx transactionQueries, id string, messages []Message) error {
	derived := PromptPRLinks(messages)
	if len(derived) == 0 {
		return nil
	}
	var stored string
	if err := tx.QueryRow(`SELECT pr_links FROM sessions WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	return writeSessionPromptPRLinksTx(tx, id, stored, parser.MergePRLinks(DecodePRLinks(stored), derived))
}

// GetSessionStructuredPRLinks returns parser-recorded links without prompt links.
func (db *DB) GetSessionStructuredPRLinks(ctx context.Context, id string) ([]PRLink, error) {
	var encoded string
	if err := db.getReader().QueryRowContext(ctx, `SELECT structured_pr_links FROM sessions WHERE id=?`, id).Scan(&encoded); err != nil {
		return nil, err
	}
	return DecodePRLinks(encoded), nil
}

func (db *DB) backfillPromptPRLinksLocked(ctx context.Context, w *writerHandle) error {
	const key = "prompt_pr_links_backfill_v1"
	if db.usageOnlyStorage() {
		return nil
	}
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	version, archiveOnly, err := readUserVersion(ctx, tx)
	if err != nil {
		return err
	}
	// Older populated archives must retain their version until their full rebuild.
	if version < fullRebuildDataVersion() {
		var hasSessions bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions)`).Scan(&hasSessions); err != nil {
			return err
		}
		if hasSessions {
			return nil
		}
	}
	var done bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM stats WHERE key=? AND value!=0)`, key).Scan(&done); err != nil {
		return err
	}
	if !done {
		ids, err := sessionIDsTx(ctx, tx, "id IN (SELECT session_id FROM messages WHERE role='user')")
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := refreshSessionPromptPRLinksTx(tx, id); err != nil {
				return fmt.Errorf("backfilling prompt PR links: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO stats(key,value) VALUES (?,1)`, key); err != nil {
			return err
		}
	}
	if version >= fullRebuildDataVersion() && version < 185 {
		// Archive-only upgrades need a preservation rebuild even when no
		// providers need reparsing. Record it before raising the version.
		if archiveOnly && len(ScopedDataVersionProviders(version, 185)) == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO stats(key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = max(value, excluded.value)`, scopedUpgradePendingKey, 185); err != nil {
				return fmt.Errorf("recording pending archive rebuild: %w", err)
			}
		}
		if err := advanceDataVersion(ctx, tx, 185); err != nil {
			return err
		}
	}
	return tx.Commit()
}
