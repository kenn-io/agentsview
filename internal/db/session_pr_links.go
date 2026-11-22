package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"path/filepath"
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

func writeSessionPromptPRLinksTx(tx transactionQueries, id, stored string, links []PRLink) error {
	encoded := EncodePRLinks(links)
	if encoded == stored {
		return nil
	}
	_, err := tx.Exec(`UPDATE sessions SET pr_links=?, local_modified_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, encoded, id)
	return err
}

const promptPRLinkCandidateSQL = `role='user' AND (COALESCE(` + sqliteDialogueExprBare + `, content) LIKE '%http://%' OR COALESCE(` + sqliteDialogueExprBare + `, content) LIKE '%https://%')`

func refreshSessionPromptPRLinksTx(ctx context.Context, tx transactionQueries, id string) error {
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
	var remote string
	if err := tx.QueryRow(`SELECT COALESCE(MAX(git_remote),'') FROM session_project_identity_snapshots WHERE session_id=? AND remote_candidate_count=1`, id).Scan(&remote); err != nil {
		return err
	}
	tools, err := storedToolCallPRLinksTx(ctx, tx, id, remote)
	if err != nil {
		return err
	}
	links := parser.CombinePRLinks(DecodePRLinks(structured), PromptPRLinks(messages), tools)
	var omitted bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tool_calls WHERE session_id=? AND category IN ('Bash','Task') AND input_json IS NULL)`, id).Scan(&omitted); err != nil {
		return err
	}
	if omitted {
		links = parser.CombinePRLinks(DecodePRLinks(stored), links)
	}
	return writeSessionPromptPRLinksTx(tx, id, stored, links)
}

func appendSessionPromptPRLinksTx(ctx context.Context, tx transactionQueries, id string, messages []Message) error {
	prompts, err := PromptPRLinksContext(ctx, messages)
	if err != nil {
		return err
	}
	hasTools := false
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Category == "Bash" || call.Category == "Task" {
				hasTools = true
				break
			}
		}
	}
	var tools []PRLink
	if hasTools {
		var cwd, remote string
		if err := tx.QueryRow(`SELECT COALESCE(cwd,''), COALESCE((SELECT MAX(git_remote) FROM session_project_identity_snapshots WHERE session_id=? AND remote_candidate_count=1),'') FROM sessions WHERE id=?`, id, id).Scan(&cwd, &remote); err != nil {
			return err
		}
		tools, err = ToolCallPRLinks(ctx, messages, cwd, remote)
		if err != nil {
			return err
		}
		if remote != "" {
			explicit, err := ToolCallPRLinks(ctx, messages, cwd, "")
			if err != nil {
				return err
			}
			if EncodePRLinks(tools) != EncodePRLinks(explicit) {
				// Earlier commands in a shared shell can change the checkout.
				rows, err := tx.Query(`SELECT t.tool_name, t.input_json FROM tool_calls t JOIN messages m ON m.id=t.message_id WHERE t.session_id=? AND t.category='Bash' AND t.tool_name NOT IN ('exec_command','shell_command','shell') AND m.ordinal < ? AND (t.input_json IS NULL OR `+shellTaintCandidateSQL+`) ORDER BY m.ordinal, t.id`, id, messages[0].Ordinal)
				if err != nil {
					return err
				}
				tainted := false
				for rows.Next() {
					var name string
					var input sql.NullString
					if err := rows.Scan(&name, &input); err != nil {
						rows.Close()
						return err
					}
					if !input.Valid {
						tainted = true
						break
					}
					_, changed, err := callPRLinks(ctx, "Bash", name, input.String, cwd, remote)
					if err != nil {
						rows.Close()
						return err
					}
					if changed {
						tainted = true
						break
					}
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return err
				}
				if err := rows.Close(); err != nil {
					return err
				}
				if tainted {
					tools = explicit
				}
			}
		}
	}
	derived := parser.CombinePRLinks(prompts, tools)
	if len(derived) == 0 {
		return nil
	}
	var stored string
	if err := tx.QueryRow(`SELECT pr_links FROM sessions WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	return writeSessionPromptPRLinksTx(tx, id, stored, parser.CombinePRLinks(DecodePRLinks(stored), derived))
}

// SessionPRLinkRemote returns the single-candidate remote recorded for a session.
func (db *DB) SessionPRLinkRemote(ctx context.Context, id string) (string, error) {
	var remote string
	err := db.getReader().QueryRowContext(ctx, `SELECT COALESCE(MAX(git_remote),'') FROM session_project_identity_snapshots WHERE session_id=? AND remote_candidate_count=1`, id).Scan(&remote)
	return remote, err
}

// GetSessionStructuredPRLinks returns only parser-recorded links.
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
		if err := refreshSessionPromptPRLinksTx(ctx, tx, id); err != nil {
			return fmt.Errorf("backfilling prompt PR links: %w", err)
		}
	}
	return nil
}

func callPRLinks(ctx context.Context, category, toolName, input, cwd, remote string) ([]PRLink, bool, error) {
	if category == "Task" {
		var payload struct {
			Prompt  string         `json:"prompt"`
			Task    string         `json:"task"`
			Message string         `json:"message"`
			Content jsontext.Value `json:"content"`
		}
		if json.Unmarshal([]byte(input), &payload) != nil {
			return nil, false, nil //nolint:nilerr // Skip malformed optional input without failing session processing.
		}
		var content string
		_ = json.Unmarshal(payload.Content, &content)
		links, err := parser.PromptPRLinksContext(ctx, payload.Prompt+"\n"+payload.Message+"\n"+payload.Task+"\n"+content)
		return links, false, err
	}
	if category != "Bash" {
		return nil, false, nil
	}
	if len(input) > 256*1024 {
		return nil, true, nil
	}
	var payload struct {
		Workdir          string `json:"workdir"`
		Cwd              string `json:"cwd"`
		WorkingDirectory string `json:"working_directory"`
	}
	_ = json.Unmarshal([]byte(input), &payload)
	dir := payload.Workdir
	if dir == "" {
		dir = payload.Cwd
	}
	if dir == "" {
		dir = payload.WorkingDirectory
	}
	if dir != "" {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		if cwd == "" || filepath.Clean(dir) != filepath.Clean(cwd) {
			remote = ""
		}
	}
	command := shellCommandText(input)
	if command == "" && !jsontext.Value(input).IsValid() && (toolName == "exec_command" || toolName == "shell_command") {
		command = input
	}
	return parser.CommandPRLinks(ctx, command, remote)
}

// ToolCallPRLinks derives links from a transcript's tool calls.
func ToolCallPRLinks(ctx context.Context, messages []Message, cwd, remote string) ([]PRLink, error) {
	var lists [][]PRLink
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			found, changed, err := callPRLinks(ctx, call.Category, call.ToolName, call.InputJSON, cwd, remote)
			if err != nil {
				return nil, err
			}
			lists = append(lists, found)
			if changed && call.ToolName != "exec_command" && call.ToolName != "shell_command" && call.ToolName != "shell" {
				remote = ""
				cwd = ""
			}
		}
	}
	return parser.CombinePRLinks(lists...), nil
}

const shellTaintCandidateSQL = `t.input_json LIKE '%cd%' OR t.input_json LIKE '%pushd%' OR t.input_json LIKE '%popd%' OR t.input_json LIKE '%eval%' OR t.input_json LIKE '%source%' OR t.input_json LIKE '%. %' OR t.input_json LIKE '%GH_REPO%' OR t.input_json LIKE '%GH_HOST%' OR t.input_json LIKE '%(%' OR t.input_json LIKE '%function %'`

func storedToolCallPRLinksTx(ctx context.Context, tx transactionQueries, id, remote string) ([]PRLink, error) {
	var cwd string
	if err := tx.QueryRow(`SELECT COALESCE(cwd,'') FROM sessions WHERE id=?`, id).Scan(&cwd); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT t.tool_name, t.category, COALESCE(t.input_json,'') FROM tool_calls t JOIN messages m ON m.id=t.message_id WHERE t.session_id=? AND t.category IN ('Bash','Task') AND (t.input_json LIKE '%gh%' OR t.input_json LIKE '%http%' OR (t.category='Bash' AND (`+shellTaintCandidateSQL+`))) ORDER BY m.ordinal, t.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var name, category, input string
		if err := rows.Scan(&name, &category, &input); err != nil {
			return nil, err
		}
		messages = append(messages, Message{ToolCalls: []ToolCall{{ToolName: name, Category: category, InputJSON: input}}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ToolCallPRLinks(ctx, messages, cwd, remote)
}

const toolPRLinksDataVersion = 186

func (db *DB) upgradeToolPRLinksTx(ctx context.Context, tx *sql.Tx) error {
	version, _, err := readUserVersion(ctx, tx)
	if err != nil || version >= toolPRLinksDataVersion {
		return err
	}
	ids, err := sessionIDsTx(ctx, tx, "id IN (SELECT session_id FROM tool_calls WHERE category IN ('Bash','Task') AND (input_json LIKE '%gh%' OR input_json LIKE '%http%'))")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshSessionPromptPRLinksTx(ctx, contextTransaction{ctx: ctx, tx: tx}, id); err != nil {
			return fmt.Errorf("backfilling tool PR links: %w", err)
		}
	}
	return nil
}
