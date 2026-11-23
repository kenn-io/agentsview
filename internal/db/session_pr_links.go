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

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
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
	var stored, structured, storedTools, cwd, remote string
	var missingInputs bool
	if err := tx.QueryRow(`SELECT pr_links, structured_pr_links, tool_pr_links, COALESCE(cwd,''), `+prLinkRemoteSQL+`, NOT EXISTS(SELECT 1 FROM tool_calls WHERE session_id=? AND category IN ('Bash','Task')) OR EXISTS(SELECT 1 FROM tool_calls WHERE session_id=? AND category IN ('Bash','Task') AND input_json IS NULL) FROM sessions WHERE id=?`, id, id, id, id).Scan(&stored, &structured, &storedTools, &cwd, &remote, &missingInputs); err != nil {
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
	tools, tainted, err := storedToolCallPRLinksTx(ctx, tx, id, cwd, remote, nil)
	if err != nil {
		return err
	}
	if err := writePRShellTaintTx(tx, id, tainted); err != nil {
		return err
	}
	if missingInputs {
		tools = parser.CombinePRLinks(DecodePRLinks(storedTools), tools)
	}
	if err := writeToolPRLinksTx(tx, id, tools); err != nil {
		return err
	}
	links := parser.CombinePRLinks(DecodePRLinks(structured), PromptPRLinks(messages), tools)
	return writeSessionPromptPRLinksTx(tx, id, stored, links)
}

func appendSessionPromptPRLinksTx(ctx context.Context, tx transactionQueries, id string, messages []Message) error {
	prompts, err := PromptPRLinksContext(ctx, messages)
	if err != nil {
		return err
	}
	hasTools := false
	firstOrdinal := 0
	for i, message := range messages {
		if i == 0 {
			firstOrdinal = message.Ordinal
		}
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
		var state sql.NullInt64
		if err := tx.QueryRow(`SELECT COALESCE(cwd,''), `+prLinkRemoteSQL+`, pr_shell_tainted FROM sessions WHERE id=?`, id, id).Scan(&cwd, &remote, &state); err != nil {
			return err
		}
		tainted := parser.PRShellContext{CheckoutUncertain: state.Int64&1 != 0, HostUncertain: state.Int64&2 != 0}
		if !state.Valid {
			_, tainted, err = storedToolCallPRLinksTx(ctx, tx, id, cwd, remote, &firstOrdinal)
			if err != nil {
				return err
			}
		}
		tools, tainted, err = toolCallPRLinksWithShellState(ctx, messages, cwd, remote, tainted)
		if err != nil {
			return err
		}
		if err := writePRShellTaintTx(tx, id, tainted); err != nil {
			return err
		}
		var storedTools string
		if err := tx.QueryRow(`SELECT tool_pr_links FROM sessions WHERE id=?`, id).Scan(&storedTools); err != nil {
			return err
		}
		if err := writeToolPRLinksTx(tx, id, parser.CombinePRLinks(DecodePRLinks(storedTools), tools)); err != nil {
			return err
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

func writeToolPRLinksTx(tx transactionQueries, id string, links []PRLink) error {
	encoded := EncodePRLinks(links)
	_, err := tx.Exec(`UPDATE sessions SET tool_pr_links=? WHERE id=? AND tool_pr_links IS NOT ?`, encoded, id, encoded)
	return err
}

// prLinkRemoteSQL prefers the transcript's remote to a single checkout remote.
const prLinkRemoteSQL = `COALESCE((SELECT COALESCE(NULLIF(recorded_git_remote,''), CASE WHEN remote_candidate_count=1 THEN git_remote END) FROM session_project_identity_snapshots WHERE session_id=?),'')`

// SessionPRLinkRemote returns the remote used to resolve bare PR numbers.
func (db *DB) SessionPRLinkRemote(ctx context.Context, id string) (string, error) {
	var remote string
	err := db.getReader().QueryRowContext(ctx, `SELECT `+prLinkRemoteSQL, id).Scan(&remote)
	return remote, err
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
		if err := refreshSessionPromptPRLinksTx(ctx, tx, id); err != nil {
			return fmt.Errorf("backfilling prompt PR links: %w", err)
		}
	}
	return nil
}

func callPRLinks(ctx context.Context, category, toolName, input, cwd, remote string, state parser.PRShellContext) ([]PRLink, parser.PRShellContext, error) {
	if category == "Task" {
		var payload struct {
			Prompt  string         `json:"prompt"`
			Task    string         `json:"task"`
			Message string         `json:"message"`
			Content jsontext.Value `json:"content"`
		}
		if json.Unmarshal([]byte(input), &payload) != nil {
			return nil, state, nil //nolint:nilerr // Skip malformed optional input without failing session processing.
		}
		var content string
		_ = json.Unmarshal(payload.Content, &content)
		links, err := parser.PromptPRLinksContext(ctx, payload.Prompt+"\n"+payload.Message+"\n"+payload.Task+"\n"+content)
		return links, state, err
	}
	if category != "Bash" {
		return nil, state, nil
	}
	if len(input) > 256*1024 {
		return nil, parser.UncertainPRShell(), nil
	}
	if toolName == "exec" && !jsontext.Value(input).IsValid() {
		links, err := codeModePRLinks(ctx, input, cwd, remote)
		return links, state, err
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
	if dir != "" && !parser.SamePRWorkingDirectory(cwd, dir) {
		remote = ""
	}
	command := shellCommandText(input)
	if command == "" && !jsontext.Value(input).IsValid() && (toolName == "exec_command" || toolName == "shell_command") {
		command = input
	}
	return parser.CommandPRLinksWithContext(ctx, command, cwd, remote, state)
}

var skippedCodeModeWarning sync.Once

// warnSkippedCodeMode reports once that code-mode input could not be read. It
// leaves the input out of the message because commands can hold private text.
func warnSkippedCodeMode() error {
	skippedCodeModeWarning.Do(func() { log.Print("PR links: skipped code-mode input that could not be parsed") })
	return nil
}

// codeModePRLinks reads each exec_command independently.
func codeModePRLinks(ctx context.Context, input, cwd, remote string) ([]PRLink, error) {
	ast, err := js.Parse(parse.NewInputString(input), js.Options{})
	if err != nil {
		return nil, warnSkippedCodeMode()
	}
	visitor := &codeModeVisitor{ctx: ctx, cwd: cwd, remote: remote}
	js.Walk(visitor, ast)
	return parser.CombinePRLinks(visitor.links...), ctx.Err()
}

type codeModeVisitor struct {
	ctx         context.Context
	cwd, remote string
	links       [][]PRLink
}

func (v *codeModeVisitor) Enter(node js.INode) js.IVisitor {
	switch node.(type) {
	case *js.FuncDecl, *js.ArrowFunc, *js.MethodDecl:
		return nil
	}
	call, ok := node.(*js.CallExpr)
	if !ok {
		return v
	}
	if dot, ok := call.X.(*js.DotExpr); ok && !dot.Optional && dot.Y.String() == "then" && !call.Optional {
		for _, arg := range call.Args.List {
			if arg.Rest {
				continue
			}
			switch callback := arg.Value.(type) {
			case *js.ArrowFunc:
				js.Walk(v, &callback.Body)
			case *js.FuncDecl:
				js.Walk(v, &callback.Body)
			}
		}
	}
	if len(call.Args.List) != 1 || call.Args.List[0].Rest {
		return v
	}
	dot, ok := call.X.(*js.DotExpr)
	if !ok || dot.Optional {
		return v
	}
	owner, ok := dot.X.(*js.Var)
	if !ok || string(owner.Data) != "tools" || dot.Y.String() != "exec_command" {
		return v
	}
	object, ok := call.Args.List[0].Value.(*js.ObjectExpr)
	if !ok {
		return v
	}
	command, dir := "", ""
	knownDir := true
	for _, property := range object.List {
		if property.Spread || property.Name == nil || property.Name.IsComputed() {
			return v
		}
		name := string(property.Name.Literal.Data)
		name = strings.Trim(name, "\"'")
		switch name {
		case "cmd", "command":
			command, _ = codeModeLiteral(property.Value)
		case "workdir", "cwd", "working_directory":
			var literal bool
			dir, literal = codeModeLiteral(property.Value)
			knownDir = knownDir && literal
		}
	}
	if command == "" {
		return v
	}
	remote := v.remote
	if !knownDir || dir != "" && !parser.SamePRWorkingDirectory(v.cwd, dir) {
		remote = ""
	}
	if links, _, err := parser.CommandPRLinks(v.ctx, command, v.cwd, remote); err == nil {
		v.links = append(v.links, links)
	}
	return v
}

func (v *codeModeVisitor) Exit(js.INode) {}

func codeModeLiteral(expr js.IExpr) (string, bool) {
	var literal string
	switch value := expr.(type) {
	case *js.LiteralExpr:
		if value.TokenType != js.StringToken {
			return "", false
		}
		literal = string(value.Data)
	case *js.TemplateExpr:
		if value.Tag != nil || len(value.List) != 0 {
			return "", false
		}
		literal = string(value.Tail)
	default:
		return "", false
	}
	return decodeJSLiteral(literal)
}

func decodeJSLiteral(literal string) (string, bool) {
	if len(literal) < 2 {
		return "", false
	}
	quote := literal[0]
	rest := literal[1 : len(literal)-1]
	var result strings.Builder
	for len(rest) > 0 {
		r, _, tail, err := strconv.UnquoteChar(rest, quote)
		if err != nil {
			return "", false
		}
		result.WriteRune(r)
		rest = tail
	}
	return result.String(), true
}

// ToolCallPRLinks derives links from a transcript's tool calls.
func ToolCallPRLinks(ctx context.Context, messages []Message, cwd, remote string) ([]PRLink, error) {
	links, _, err := toolCallPRLinksWithShellState(ctx, messages, cwd, remote, parser.PRShellContext{})
	return links, err
}

func independentShell(name string) bool {
	return name == "exec_command" || name == "shell_command" || name == "shell" || name == "exec"
}

func toolCallPRLinksWithShellState(ctx context.Context, messages []Message, cwd, remote string, tainted parser.PRShellContext) ([]PRLink, parser.PRShellContext, error) {
	var lists [][]PRLink
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if err := ctx.Err(); err != nil {
				return nil, tainted, err
			}
			state := tainted
			if independentShell(call.ToolName) {
				state = parser.PRShellContext{}
			}
			found, changed, err := callPRLinks(ctx, call.Category, call.ToolName, call.InputJSON, cwd, remote, state)
			if err != nil {
				return nil, tainted, err
			}
			lists = append(lists, found)
			if !independentShell(call.ToolName) {
				tainted = changed
			}
		}
	}
	return parser.CombinePRLinks(lists...), tainted, nil
}

// pr_shell_tainted stores checkout and host uncertainty in bits 0 and 1.
func writePRShellTaintTx(tx transactionQueries, id string, tainted parser.PRShellContext) error {
	state := 0
	if tainted.CheckoutUncertain {
		state |= 1
	}
	if tainted.HostUncertain {
		state |= 2
	}
	_, err := tx.Exec(`UPDATE sessions SET pr_shell_tainted=? WHERE id=? AND pr_shell_tainted IS NOT ?`, state, id, state)
	return err
}

// A NULL state is initialized once from retained history. New appends then
// advance it without reading earlier tool calls again.
func storedToolCallPRLinksTx(ctx context.Context, tx transactionQueries, id, cwd, remote string, before *int) ([]PRLink, parser.PRShellContext, error) {
	query := `SELECT t.tool_name, t.category, COALESCE(t.input_json,'') FROM tool_calls t JOIN messages m ON m.id=t.message_id WHERE t.session_id=? AND (t.category='Bash' OR (t.category='Task' AND t.input_json LIKE '%http%'))`
	args := []any{id}
	if before != nil {
		query += ` AND m.ordinal < ?`
		args = append(args, *before)
	}
	query += ` ORDER BY m.ordinal, t.id`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, parser.PRShellContext{}, err
	}
	defer rows.Close()
	var lists [][]PRLink
	tainted := parser.PRShellContext{}
	for rows.Next() {
		var name, category, input string
		if err := rows.Scan(&name, &category, &input); err != nil {
			return nil, parser.PRShellContext{}, err
		}
		messages := []Message{{ToolCalls: []ToolCall{{ToolName: name, Category: category, InputJSON: input}}}}
		links, next, err := toolCallPRLinksWithShellState(ctx, messages, cwd, remote, tainted)
		if err != nil {
			return nil, parser.PRShellContext{}, err
		}
		tainted = next
		lists = append(lists, links)
	}
	if err := rows.Err(); err != nil {
		return nil, parser.PRShellContext{}, err
	}
	return parser.CombinePRLinks(lists...), tainted, nil
}

const toolPRLinksDataVersion = 186

// toolPRLinkCandidateSQL matches gh pr commands, including a gh quoted or
// escaped in the shell, and URLs.
const toolPRLinkCandidateSQL = `input_json LIKE '%gh%pr%' OR input_json LIKE '%g''%h%pr%' OR input_json LIKE '%g\%h%pr%' OR input_json LIKE '%http%'`

func (db *DB) upgradeToolPRLinksTx(ctx context.Context, tx *sql.Tx) error {
	version, _, err := readUserVersion(ctx, tx)
	if err != nil || version >= toolPRLinksDataVersion {
		return err
	}
	condition := "id IN (SELECT session_id FROM tool_calls WHERE category IN ('Bash','Task') AND (" + toolPRLinkCandidateSQL + "))"
	if version < promptPRLinksDataVersion && !db.usageOnlyStorage() {
		// The prompt upgrade already refreshed these sessions, including tools.
		condition += " AND id NOT IN (SELECT session_id FROM messages WHERE " + promptPRLinkCandidateSQL + ")"
	}
	ids, err := sessionIDsTx(ctx, tx, condition)
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
