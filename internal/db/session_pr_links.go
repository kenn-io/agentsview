package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/export"
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
	var stored, structured, cwd, remote string
	if err := tx.QueryRow(`SELECT pr_links, structured_pr_links, COALESCE(cwd,''), `+checkoutRemoteSQL+` FROM sessions WHERE id=?`, id, id).Scan(&stored, &structured, &cwd, &remote); err != nil {
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
	tools, err := storedToolCallPRLinksTx(ctx, tx, id, cwd, remote, math.MaxInt, &sharedPRContext{})
	if err != nil {
		return err
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
	var cwd, remote string
	if err := tx.QueryRow(`SELECT COALESCE(cwd,''), `+checkoutRemoteSQL+` FROM sessions WHERE id=?`, id, id).Scan(&cwd, &remote); err != nil {
		return err
	}
	sharedContext := sharedPRContext{}
	var depends []PRLink
	tools, err := toolCallPRLinks(ctx, messages, cwd, remote, &sharedContext, &depends)
	if err != nil {
		return err
	}
	if len(prompts) == 0 && len(tools) == 0 {
		return nil
	}
	var stored string
	if err := tx.QueryRow(`SELECT pr_links FROM sessions WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	storedLinks := DecodePRLinks(stored)
	// Only shell calls add to depends, so a new remote-dependent link implies one ran.
	if len(parser.CombinePRLinks(storedLinks, depends)) > len(parser.CombinePRLinks(storedLinks)) {
		sharedContext = sharedPRContext{}
		if _, err := storedToolCallPRLinksTx(ctx, tx, id, cwd, remote, messages[0].Ordinal, &sharedContext); err != nil {
			return err
		}
		if sharedContext.directory || sharedContext.repository {
			tools, err = toolCallPRLinks(ctx, messages, cwd, remote, &sharedContext, nil)
			if err != nil {
				return err
			}
		}
	}
	derived := parser.CombinePRLinks(prompts, tools)
	if len(derived) == 0 {
		return nil
	}
	return writeSessionPromptPRLinksTx(tx, id, stored, parser.CombinePRLinks(storedLinks, derived))
}

// checkoutRemoteSQL selects a single checkout remote.
const checkoutRemoteSQL = `COALESCE((SELECT CASE WHEN remote_candidate_count=1 THEN git_remote END FROM session_project_identity_snapshots WHERE session_id=?),'')`

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

// upgradePRLinksTx copies legacy structured links and refreshes each candidate once.
// Older binaries may have rewritten pr_links without updating structured_pr_links.
// Copy and refresh in the version transaction so old archives never hold derived links.
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
	candidate := "id IN (SELECT session_id FROM tool_calls WHERE category IN ('Bash','Task') AND input_json IS NOT NULL)"
	if version < promptPRLinksDataVersion {
		candidate += " OR id IN (SELECT session_id FROM messages WHERE " + promptPRLinkCandidateSQL + ")"
	}
	ids, err := sessionIDsTx(ctx, tx, candidate)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshSessionPromptPRLinksTx(ctx, contextTransaction{ctx: ctx, tx: tx}, id); err != nil {
			return fmt.Errorf("backfilling PR links: %w", err)
		}
	}
	return nil
}

type sharedPRContext struct{ directory, repository, noBare bool }

func (c *sharedPRContext) add(directory, repository bool) {
	c.directory = c.directory || directory
	c.repository = c.repository || repository
}

// bareLinksAllowed reports whether a bare pull request number names the
// session's repository: a call's own workdir must be the session checkout,
// and otherwise no earlier command may have changed directory or repository.
func bareLinksAllowed(workdir, cwd string, shared sharedPRContext) bool {
	if shared.noBare {
		return false
	}
	if workdir != "" {
		return !shared.repository && parser.SamePRWorkingDirectory(cwd, workdir)
	}
	return !shared.directory && !shared.repository
}

// inputWorkdir returns the directory a call's input names: its structured
// workdir or a code-mode literal's, resolved without the session cwd so an
// absent workdir stays empty. Raw shell text names none.
func inputWorkdir(input string) string {
	if jsontext.Value(input).IsValid() {
		return parser.ToolInputWorkdir(input)
	}
	if commitToolCallRE.MatchString(input) {
		return commitRepository(input, "")
	}
	return ""
}

func callPRLinks(ctx context.Context, category, toolName, input, cwd, remote string, sharedContext sharedPRContext, change *sharedPRContext, depends *[]PRLink) ([]PRLink, error) {
	if category == "Task" {
		return taskPRLinks(ctx, input)
	}
	if category != "Bash" || len(input) > 256*1024 {
		change.add(false, true)
		return nil, ctx.Err()
	}
	commands, ok := shellInputCommands(input)
	if !ok {
		return nil, ctx.Err()
	}
	allowBare := bareLinksAllowed(inputWorkdir(input), cwd, sharedContext)
	var lists [][]PRLink
	for _, command := range commands {
		if command.codeMode {
			allowBare = bareLinksAllowed(command.workdir, cwd, sharedContext)
		}
		links, directory, repository, persistent, usesRemote, err := parser.AnalyzePRCommand(ctx, command.text, cwd, remote, allowBare, sharedContext.repository)
		change.add(directory, persistent)
		if depends != nil {
			*depends = append(*depends, usesRemote...)
		}
		if err != nil {
			return nil, err
		}
		lists = append(lists, links)
		if directory || repository {
			allowBare = false
		}
		sharedContext.repository = sharedContext.repository || repository
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

type prShellCommand struct {
	text, workdir string
	codeMode      bool
}

// codeModeShellCallRE matches literal object arguments to code-mode tool calls.
var codeModeShellCallRE = regexp.MustCompile("\\btools\\.\\w+\\s*\\(\\s*\\{(?:[^{}\"'`]|\"(?:\\\\.|[^\"\\\\])*\"|'(?:\\\\.|[^'\\\\])*'|`(?:\\\\.|[^`\\\\])*`)*\\}")

// shellInputCommands lists literal commands in a shell tool input.
func shellInputCommands(input string) ([]prShellCommand, bool) {
	if !jsontext.Value(input).IsValid() {
		if commitToolCallRE.MatchString(input) {
			var commands []prShellCommand
			for _, call := range codeModeShellCallRE.FindAllString(input, -1) {
				if command := commitCommandText(call); command != "" {
					commands = append(commands, prShellCommand{text: command, workdir: commitRepository(call, ""), codeMode: true})
				}
			}
			return commands, len(commands) > 0
		}
		text := strings.TrimSpace(input)
		return []prShellCommand{{text: input}}, text != "" && text[0] != '{' && text[0] != '['
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
		return []prShellCommand{{text: command}}, ok
	}
	if payload.CommandLine != "" {
		return []prShellCommand{{text: payload.CommandLine}}, true
	}
	if payload.Commands != nil {
		commands := make([]prShellCommand, len(payload.Commands))
		for i, command := range payload.Commands {
			commands[i].text = command
		}
		return commands, true
	}
	return []prShellCommand{{text: payload.Cmd}}, true
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

// ToolCallPRLinks derives links from a transcript's tool calls.
func ToolCallPRLinks(ctx context.Context, messages []Message, cwd, remote string) ([]PRLink, error) {
	changed := sharedPRContext{}
	return toolCallPRLinks(ctx, messages, cwd, remote, &changed, nil)
}

// ToolCallPRLinksForUsage uses a known host without inferring bare numbers
// after earlier shell state has been discarded.
func ToolCallPRLinksForUsage(ctx context.Context, messages []Message, remote string) ([]PRLink, error) {
	changed := sharedPRContext{noBare: true}
	return toolCallPRLinks(ctx, messages, "", remote, &changed, nil)
}

func toolCallPRLinks(ctx context.Context, messages []Message, cwd, remote string, sharedContext *sharedPRContext, depends *[]PRLink) ([]PRLink, error) {
	states, err := toolCallPRStates(ctx, messages, cwd, remote, sharedContext, depends)
	return states.links(), err
}

// callPRState is what one tool call adds: the links it names and the shell
// state it changes for later calls.
type callPRState struct {
	links  []PRLink
	change sharedPRContext
}

type callPRStates []callPRState

func (states callPRStates) links() []PRLink {
	lists := make([][]PRLink, len(states))
	for i, state := range states {
		lists[i] = state.links
	}
	return parser.CombinePRLinks(lists...)
}

// toolCallPRStates derives each tool call's state in order, carrying shell
// state from call to call.
func toolCallPRStates(ctx context.Context, messages []Message, cwd, remote string, sharedContext *sharedPRContext, depends *[]PRLink) (callPRStates, error) {
	var states callPRStates
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			state := sharedPRContext{repository: sharedContext.repository, noBare: sharedContext.noBare}
			if call.ToolName == "Bash" {
				state = *sharedContext
			}
			callState, err := callPRStateIn(ctx, call, cwd, remote, state, depends)
			if err != nil {
				return nil, err
			}
			states = append(states, callState)
			if call.Category == "Bash" {
				if call.ToolName == "Bash" {
					sharedContext.directory = sharedContext.directory || callState.change.directory
				}
				sharedContext.repository = sharedContext.repository || callState.change.repository
			}
		}
	}
	return states, nil
}

// callPRStateIn derives one call's state under the shell state before it. A
// call whose input the archive dropped resolves the links and state it kept.
func callPRStateIn(ctx context.Context, call ToolCall, cwd, remote string, state sharedPRContext, depends *[]PRLink) (callPRState, error) {
	if call.InputJSON == "" {
		var retained retainedPRState
		if call.PRLinks != "" && json.Unmarshal([]byte(call.PRLinks), &retained) == nil {
			links := retainedPRLinksIn(retained, remote, state, depends)
			return callPRState{links: links, change: sharedPRContext{directory: retained.Dir, repository: retained.Repo}}, nil
		}
		return callPRState{}, nil
	}
	var change sharedPRContext
	links, err := callPRLinks(ctx, call.Category, call.ToolName, call.InputJSON, cwd, remote, state, &change, depends)
	return callPRState{links, change}, err
}

// retainedPRLinksIn resolves retained links under the current checkout.
func retainedPRLinksIn(retained retainedPRState, remote string, state sharedPRContext, depends *[]PRLink) []PRLink {
	remoteHost := ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, _, _ = strings.Cut(normalized, "/")
	}
	var links []PRLink
	for _, link := range retained.Links {
		dependent := retained.Depends[link.URL]
		link, ok := parser.NewPRLink(link.URL, link.Repository, link.Number)
		if !ok {
			continue
		}
		if !dependent {
			links = append(links, link)
			continue
		}
		host := remoteHost
		if host == "" {
			host = "github.com"
		}
		if resolved, ok := parser.NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", host, link.Repository, link.Number), link.Repository, link.Number); ok && !state.repository {
			if depends != nil {
				*depends = append(*depends, resolved)
			}
			links = append(links, resolved)
		}
		if retained.Explicit[link.URL] {
			links = append(links, link)
		}
	}
	return links
}

// retainedPRState holds what a call named without its input. Depends marks a
// link whose host a hostless reference took from the checkout; Explicit marks
// such a link that a reference naming that host also produced.
type retainedPRState struct {
	Links    []PRLink        `json:"links,omitempty"`
	Depends  map[string]bool `json:"depends,omitempty"`
	Explicit map[string]bool `json:"explicit,omitempty"`
	Dir      bool            `json:"directory,omitempty"`
	Repo     bool            `json:"repository,omitempty"`
}

// markDepends records which of a call's links the checkout decides. Resolving
// the call again with no usable host leaves only the references naming one.
func (r *retainedPRState) markDepends(call ToolCall, depends []PRLink) {
	if len(depends) == 0 {
		return
	}
	var ignored sharedPRContext
	explicit, _ := callPRLinks(context.Background(), call.Category, call.ToolName, call.InputJSON, "", "", sharedPRContext{repository: true}, &ignored, nil)
	r.Depends = make(map[string]bool, len(depends))
	for _, link := range depends {
		r.Depends[link.URL] = true
	}
	for _, link := range explicit {
		if r.Depends[link.URL] {
			if r.Explicit == nil {
				r.Explicit = make(map[string]bool)
			}
			r.Explicit[link.URL] = true
		}
	}
}

// retainedCallPRLinks retains selectors and shell changes without the input.
func retainedCallPRLinks(call ToolCall) string {
	if call.InputJSON == "" || (call.Category != "Bash" && call.Category != "Task") {
		return call.PRLinks
	}
	var change sharedPRContext
	var depends []PRLink
	found, _ := callPRLinks(context.Background(), call.Category, call.ToolName, call.InputJSON, "", "", sharedPRContext{}, &change, &depends)
	if len(found) == 0 && !change.directory && !change.repository {
		return ""
	}
	retained := retainedPRState{Links: found, Dir: change.directory, Repo: change.repository}
	retained.markDepends(call, depends)
	encoded, _ := json.Marshal(retained)
	return string(encoded)
}

// retainCopiedToolPRLinksTx keeps the PR metadata of copied sessions before
// their inputs are dropped.
func retainCopiedToolPRLinksTx(ctx context.Context, tx *sql.Tx, tempIDsTable string) error {
	ids, err := sessionIDsTx(ctx, tx, "id IN (SELECT id FROM "+tempIDsTable+")")
	for _, id := range ids {
		if err = retainStoredToolPRLinksTx(contextTransaction{ctx: ctx, tx: tx}, id); err != nil {
			break
		}
	}
	return err
}

func retainCopiedUsageToolPRLinksTx(ctx context.Context, tx *sql.Tx, tempIDsTable string) error {
	ids, err := sessionIDsTx(ctx, tx, "id IN (SELECT id FROM "+tempIDsTable+")")
	if err != nil {
		return err
	}
	for _, id := range ids {
		stored, err := storedToolCallsTx(contextTransaction{ctx: ctx, tx: tx}, id, math.MaxInt)
		if err != nil {
			return err
		}
		message := Message{ToolCalls: make([]ToolCall, len(stored))}
		for i, call := range stored {
			message.ToolCalls[i] = call.call
		}
		if err := retainUsageToolPRLinksTx(ctx, tx, id, []Message{message}); err != nil {
			return err
		}
	}
	return nil
}

// storedToolCall is a stored tool call and the row it came from.
type storedToolCall struct {
	rowID int64
	call  ToolCall
}

// storedToolCallsTx lists a session's stored shell and delegation calls in
// order, as far as the ordinal, that hold an input or retained PR metadata.
func storedToolCallsTx(tx transactionQueries, id string, before int) ([]storedToolCall, error) {
	rows, err := tx.Query(`SELECT t.id, t.category, t.tool_name, COALESCE(t.input_json,''), COALESCE(t.pr_links,'') FROM tool_calls t JOIN messages m ON m.id=t.message_id WHERE t.session_id=? AND t.category IN ('Bash','Task') AND (t.input_json IS NOT NULL OR t.pr_links IS NOT NULL) AND m.ordinal<? ORDER BY m.ordinal, t.id`, id, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []storedToolCall
	for rows.Next() {
		var stored storedToolCall
		if err := rows.Scan(&stored.rowID, &stored.call.Category, &stored.call.ToolName, &stored.call.InputJSON, &stored.call.PRLinks); err != nil {
			return nil, err
		}
		calls = append(calls, stored)
	}
	return calls, rows.Err()
}

// storedToolCallPRLinksTx replays stored calls before an ordinal, leaving their shell changes in state.
func storedToolCallPRLinksTx(ctx context.Context, tx transactionQueries, id, cwd, remote string, before int, state *sharedPRContext) ([]PRLink, error) {
	stored, err := storedToolCallsTx(tx, id, before)
	if err != nil {
		return nil, err
	}
	var lists [][]PRLink
	for _, s := range stored {
		links, err := toolCallPRLinks(ctx, []Message{{ToolCalls: []ToolCall{s.call}}}, cwd, remote, state, nil)
		if err != nil {
			return nil, err
		}
		lists = append(lists, links)
	}
	return parser.CombinePRLinks(lists...), nil
}

// retainStoredToolPRLinksTx keeps the PR metadata of a session's stored calls
// that still hold an input, so a later refresh survives dropping it.
func retainStoredToolPRLinksTx(tx transactionQueries, id string) error {
	stored, err := storedToolCallsTx(tx, id, math.MaxInt)
	if err != nil {
		return err
	}
	for _, s := range stored {
		links := retainedCallPRLinks(s.call)
		if _, err := tx.Exec(`UPDATE tool_calls SET pr_links=? WHERE id=?`, nilIfEmpty(links), s.rowID); err != nil {
			return err
		}
	}
	return nil
}

const toolPRLinksDataVersion = 190
