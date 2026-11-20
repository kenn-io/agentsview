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
		lists = append(lists, parser.PromptPRLinks(withoutSystemReminders(content)))
	}
	return parser.MergePRLinks(lists...)
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

func writeSessionPromptPRLinksTx(tx transactionQueries, id, stored string, links []PRLink) error {
	encoded := EncodePRLinks(links)
	if encoded == stored {
		return nil
	}
	_, err := tx.Exec(`UPDATE sessions SET pr_links=?, local_modified_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, encoded, id)
	return err
}

const promptPRLinkCandidateSQL = `role='user' AND (COALESCE(` + sqliteDialogueExprBare + `, content) LIKE '%http://%' OR COALESCE(` + sqliteDialogueExprBare + `, content) LIKE '%https://%')`

func refreshSessionPromptPRLinksTx(tx transactionQueries, id string) error {
	var stored, structured string
	if err := tx.QueryRow(`SELECT pr_links, structured_pr_links FROM sessions WHERE id=?`, id).Scan(&stored, &structured); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT `+selectMessageCols+` FROM messages WHERE session_id=? AND `+promptPRLinkCandidateSQL+` ORDER BY ordinal`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	messages, err := scanMessages(rows)
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
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

// promptPRLinksDataVersion is the first data version whose pr_links include
// links found in retained prompts.
const promptPRLinksDataVersion = 184

// upgradePromptPRLinksTx indexes stored prompts in the transaction that raises
// the archive to promptPRLinksDataVersion. An older binary may have rewritten
// pr_links without structured_pr_links since the column was added, so the
// structured column is refreshed first. Doing both with the version change
// means an archive an older binary still accepts never holds prompt links.
func (db *DB) upgradePromptPRLinksTx(ctx context.Context, tx *sql.Tx) error {
	version, _, err := readUserVersion(ctx, tx)
	if err != nil || version >= promptPRLinksDataVersion {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET structured_pr_links=pr_links WHERE structured_pr_links IS NOT pr_links`); err != nil {
		return fmt.Errorf("copying structured PR links: %w", err)
	}
	if db.usageOnlyStorage() {
		return nil
	}
	ids, err := sessionIDsTx(ctx, tx, "id IN (SELECT session_id FROM messages WHERE "+promptPRLinkCandidateSQL+")")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshSessionPromptPRLinksTx(tx, id); err != nil {
			return fmt.Errorf("backfilling prompt PR links: %w", err)
		}
	}
	return nil
}
