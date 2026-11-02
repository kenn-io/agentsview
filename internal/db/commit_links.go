package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	MaxCommitSHAs          = 200
	commitLinksBackfillKey = "commit_links_backfill_v1"
	commitCommandToken     = `(?:"(?:\\[\s\S]|[^"\\])*"|'[^']*'|[^\s;&|"']+)`
	commitGlobalOption     = `(?:-[cC]\s+` + commitCommandToken + `|--(?:git-dir|work-tree|namespace|config-env)(?:=|\s+)` + commitCommandToken + `|--(?:no-pager|paginate|bare|no-replace-objects|literal-pathspecs|glob-pathspecs|noglob-pathspecs|icase-pathspecs|no-optional-locks|no-lazy-fetch))`
	commitJSString         = `(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|` + "`(?:\\\\.|[^`\\\\])*`)"
)

var (
	ErrInvalidCommitSHA       = errors.New("commit SHA must contain 7 to 64 hexadecimal characters")
	ErrCommitLinksUnavailable = errors.New("commit links are unavailable from this backend")
	commitSHARE               = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	commitSummaryRE           = regexp.MustCompile(`(?m)^\[(.+?) (?:\(root-commit\) )?([0-9a-f]{7,64})\] ([^\r\n]*)\r?$`)
	commitInvocationRE        = regexp.MustCompile(`(?:^|[\s;&|('"/]|\\[nrt])git\s+(?:` + commitGlobalOption + `\s+)*commit(?:$|[\s;&|)'"\\])`)
	commitArgumentListRE      = regexp.MustCompile(`\[\s*["']git["']\s*,\s*["']commit["']`)
	commitWindowsDriveRE      = regexp.MustCompile(`^[A-Za-z]:`)
	commitWorkdirRE           = regexp.MustCompile(`["']?(?:workdir|cwd)["']?\s*:\s*("(?:\\.|[^"\\])*"|'[^']*')`)
	commitCDRE                = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|[^\s;&|]+)`)
	commitGitDirRE            = regexp.MustCompile(`\bgit\s+(?:` + commitGlobalOption + `\s+)*?-C\s+("[^"]*"|'[^']*'|[^\s;&|]+)`)
	commitOperationRE         = regexp.MustCompile(`\btools\.(exec_command|write_stdin)\s*\(\s*(\{(?:` + commitJSString + "|[^{}\"'`])*\\})\\s*\\)")
	commitPropertyRE          = regexp.MustCompile(`("(?:cmd|command|workdir|cwd|session_id)"|'(?:cmd|command|workdir|cwd|session_id)'|\b(?:cmd|command|workdir|cwd|session_id))\s*:\s*(` + commitJSString + `|[0-9]+)\s*[,}]`)
	commitRunningRE           = regexp.MustCompile(`(?m)^Process running with session ID ([0-9]+)\r?$`)
	commitQuietRE             = regexp.MustCompile(`\bcommit\s+[^;&|\n]*(?:-q\b|--quiet\b)`)
	commitReadbackRE          = regexp.MustCompile(`\bcommit\s+((?:[^;&|\n"'\\]|"(?:\\[\s\S]|[^"\\])*"|'[^']*')*)&&\s*(git\s+(?:rev-parse|log)\b[^;&|\n]*)`)
	commitArgumentRE          = regexp.MustCompile(commitCommandToken)
	commitRevParseRE          = regexp.MustCompile(`^git\s+rev-parse\s+HEAD\s*$`)
	commitLogRE               = regexp.MustCompile(`^git\s+log\s+(?:-1\s+--oneline|--oneline\s+(?:-1|-n\s+1)|-n\s+1\s+--oneline)(?:\s+HEAD)?\s*$`)
	commitFullLineRE          = regexp.MustCompile(`(?m)^([0-9a-f]{40}|[0-9a-f]{64})\r?$`)
	commitLogLineRE           = regexp.MustCompile(`(?m)^([0-9a-f]{7,64}) ([^\r\n]*)\r?$`)
)

// CommitLinkReader is the optional archive capability used by HTTP and CLI reads.
type CommitLinkReader interface {
	SessionCommitLinks(context.Context, string) (*SessionCommitList, error)
	CommitSessionLinks(context.Context, []string) (*CommitSessionList, error)
}

type CommitTurn struct {
	Ordinal int    `json:"ordinal"`
	Text    string `json:"text"`
}

type CommitLink struct {
	SessionID      string      `json:"session_id"`
	SHA            string      `json:"sha"`
	Branch         string      `json:"branch"`
	Subject        string      `json:"subject"`
	Repository     string      `json:"repository"`
	MessageOrdinal int         `json:"message_ordinal"`
	CallIndex      int         `json:"call_index"`
	ToolUseID      string      `json:"tool_use_id"`
	CommittedAt    string      `json:"committed_at"`
	Source         string      `json:"source"`
	Missing        bool        `json:"missing"`
	Ambiguous      bool        `json:"ambiguous"`
	Turn           *CommitTurn `json:"turn,omitempty"`
}

type SessionCommitList struct {
	Commits []CommitLink `json:"commits"`
}
type CommitSessionMatch struct {
	SHA   string       `json:"sha"`
	Links []CommitLink `json:"links"`
}
type CommitSessionList struct {
	Commits []CommitSessionMatch `json:"commits"`
}

// commitRepository records the first directory described by a call.
func commitRepository(input, cwd string) string {
	resolve := func(base, dir string) string {
		dir = strings.Trim(dir, "\"'")
		if path.IsAbs(dir) || strings.HasPrefix(dir, "~") || commitWindowsDriveRE.MatchString(dir) || strings.HasPrefix(dir, `\\`) {
			return dir
		}
		if !strings.ContainsAny(base+dir, `\`) && !commitWindowsDriveRE.MatchString(base) {
			return path.Join(base, dir)
		}
		// filepath follows the host OS; transcript paths can come from any OS.
		backslashes := strings.ContainsAny(base+dir, `\`)
		base = strings.ReplaceAll(base, `\`, "/")
		dir = strings.ReplaceAll(dir, `\`, "/")
		volume := ""
		if commitWindowsDriveRE.MatchString(base) {
			volume, base = base[:2], base[2:]
		} else if strings.HasPrefix(base, "//") {
			parts := strings.SplitN(base[2:], "/", 3)
			if len(parts) >= 2 {
				volume, base = "//"+parts[0]+"/"+parts[1], "/"
				if len(parts) == 3 {
					base += parts[2]
				}
			}
		}
		if path.IsAbs(dir) {
			base = ""
		}
		joined := volume + path.Join(base, dir)
		if backslashes {
			joined = strings.ReplaceAll(joined, "/", `\`)
		}
		return joined
	}
	var fields struct {
		Workdir string `json:"workdir"`
		Cwd     string `json:"cwd"`
	}
	_ = json.Unmarshal([]byte(input), &fields)
	dir := fields.Workdir
	if dir == "" {
		dir = fields.Cwd
	}
	if dir == "" && !jsontext.Value(input).IsValid() {
		if match := commitWorkdirRE.FindStringSubmatch(input); match != nil {
			dir = match[1]
			if strings.HasPrefix(dir, "\"") {
				_ = json.Unmarshal([]byte(match[1]), &dir)
			}
		}
	}
	if dir != "" {
		cwd = resolve(cwd, dir)
	}
	command := signals.CommandText(input)
	if match := commitCDRE.FindStringSubmatch(command); match != nil {
		cwd = resolve(cwd, match[1])
	}
	if match := commitGitDirRE.FindStringSubmatch(commitInvocationRE.FindString(command)); match != nil {
		cwd = resolve(cwd, match[1])
	}
	return cwd
}

type commitCall struct {
	CommitLink
	input, result, toolName string
}

// commitOperations pairs literal code-mode calls with their ordered result
// blocks. Leave ambiguous batches unlinked rather than guessing ownership.
func commitOperations(call commitCall) []commitCall {
	if call.toolName != "exec" {
		return []commitCall{call}
	}
	operations := commitOperationRE.FindAllStringSubmatch(call.input, -1)
	if len(operations) == 0 {
		return []commitCall{call}
	}
	results := []string{call.result}
	if len(operations) > 1 {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(call.result), &blocks) != nil || len(blocks) != len(operations) {
			return nil
		}
		results = make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "input_text" {
				return nil
			}
			results = append(results, block.Text)
		}
	}
	var calls []commitCall
	for i, operation := range operations {
		args := map[string]any{}
		for _, property := range commitPropertyRE.FindAllStringSubmatch(operation[2], -1) {
			key, value := strings.Trim(property[1], `"'`), property[2]
			if value[0] == '"' || value[0] == '\'' || value[0] == '`' {
				quote := value[0]
				rest := value[1 : len(value)-1]
				var decoded strings.Builder
				for rest != "" {
					if quote == '`' {
						if strings.HasPrefix(rest, "${") {
							return nil
						}
						if strings.HasPrefix(rest, "\\`") || strings.HasPrefix(rest, `\$`) {
							decoded.WriteByte(rest[1])
							rest = rest[2:]
							continue
						}
					}
					r, _, tail, err := strconv.UnquoteChar(rest, quote)
					if err != nil {
						return nil
					}
					decoded.WriteRune(r)
					rest = tail
				}
				args[key] = decoded.String()
			} else {
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil
				}
				args[key] = n
			}
		}
		input, err := json.Marshal(args)
		if err != nil {
			return nil
		}
		nested := call
		nested.input, nested.result, nested.toolName = string(input), results[i], operation[1]
		calls = append(calls, nested)
	}
	return calls
}

// callOutput decodes shell results and their running process IDs.
func callOutput(result string) (string, int64) {
	var payload struct {
		Output      *string `json:"output"`
		OutputDelta *string `json:"output_delta"`
		SessionID   int64   `json:"session_id"`
	}
	if json.Unmarshal([]byte(result), &payload) == nil {
		if payload.Output != nil {
			return *payload.Output, payload.SessionID
		}
		if payload.OutputDelta != nil {
			return *payload.OutputDelta, payload.SessionID
		}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(result), &blocks) == nil && len(blocks) > 0 && blocks[0].Type == "input_text" {
		var outputs []string
		var running int64
		for _, block := range blocks {
			output, process := callOutput(block.Text)
			outputs = append(outputs, output)
			if process != 0 {
				running = process
			}
		}
		return strings.Join(outputs, "\n"), running
	}
	if match := commitRunningRE.FindStringSubmatch(result); match != nil {
		process, _ := strconv.ParseInt(match[1], 10, 64)
		return result, process
	}
	return result, 0
}

func commitMessageSubject(arguments string) string {
	tokens := commitArgumentRE.FindAllString(arguments, -1)
	for i, token := range tokens {
		if (token == "-m" || token == "--message") && i+1 < len(tokens) {
			message := tokens[i+1]
			if len(message) >= 2 && strings.ContainsAny(message[:1], "\"'") {
				quote := message[0]
				message = message[1 : len(message)-1]
				if quote == '"' {
					var decoded strings.Builder
					for j := 0; j < len(message); j++ {
						if message[j] == '\\' && j+1 < len(message) && strings.ContainsRune("\"\\$`\n", rune(message[j+1])) {
							j++
							if message[j] == '\n' {
								continue
							}
						}
						decoded.WriteByte(message[j])
					}
					message = decoded.String()
				}
			}
			subject, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
			return strings.TrimSpace(subject)
		}
	}
	return ""
}

func commitLinks(calls []commitCall, cwd string) []CommitLink {
	var operations []commitCall
	for _, call := range calls {
		operations = append(operations, commitOperations(call)...)
	}
	var links []CommitLink
	pending := map[string]commitCall{}
	seen := map[string]bool{}
	for _, call := range operations {
		output, running := callOutput(call.result)
		owner := call
		command := signals.CommandText(call.input)
		isCommit := commitInvocationRE.MatchString(command) || commitArgumentListRE.MatchString(command)
		pollID := ""
		isPoll := call.toolName == "write_stdin"
		if isPoll {
			var args struct {
				SessionID int64 `json:"session_id"`
			}
			if json.Unmarshal([]byte(call.input), &args) == nil {
				pollID = strconv.FormatInt(args.SessionID, 10)
			}
		}
		if isPoll {
			original, ok := pending[pollID]
			if !ok {
				continue
			}
			owner = original
			command = signals.CommandText(owner.input)
			if running == 0 {
				delete(pending, pollID)
			}
		} else if !isCommit {
			continue
		}
		matches := commitSummaryRE.FindAllStringSubmatch(output, -1)
		readback := commitReadbackRE.FindStringSubmatch(command)
		if commitQuietRE.MatchString(command) && readback != nil {
			var quiet [][]string
			if commitRevParseRE.MatchString(readback[2]) {
				quiet = commitFullLineRE.FindAllStringSubmatch(output, -1)
			} else if commitLogRE.MatchString(readback[2]) {
				quiet = commitLogLineRE.FindAllStringSubmatch(output, -1)
			}
			if len(quiet) == 1 {
				subject := ""
				if len(quiet[0]) > 2 {
					subject = quiet[0][2]
				}
				if subject == "" {
					subject = commitMessageSubject(readback[1])
				}
				matches = append(matches, []string{"", "", quiet[0][1], subject})
			}
		}
		if running != 0 {
			pending[strconv.FormatInt(running, 10)] = owner
		}
		for _, match := range matches {
			if seen[match[2]] {
				continue
			}
			seen[match[2]] = true
			link := owner.CommitLink
			link.SHA, link.Branch, link.Subject = match[2], match[1], match[3]
			link.Repository = commitRepository(owner.input, cwd)
			links = append(links, link)
		}
	}
	return links
}

func refreshSessionCommitLinksTx(tx transactionQueries, sessionID string) error {
	rows, err := tx.Query(`SELECT m.ordinal, COALESCE(t.call_index,0), COALESCE(t.tool_use_id,''),
 COALESCE(m.timestamp,''), t.input_json, `+ToolCallResultContentSQL("t", "m.ordinal")+`, COALESCE(s.cwd,''), t.tool_name
 FROM tool_calls t JOIN messages m ON m.id=t.message_id JOIN sessions s ON s.id=t.session_id
 WHERE t.session_id=? AND t.category='Bash' AND t.input_json IS NOT NULL AND ((t.input_json LIKE '%git%' AND t.input_json LIKE '%commit%') OR t.input_json LIKE '%write_stdin%' OR t.tool_name='write_stdin')
 ORDER BY m.ordinal,t.call_index`, sessionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var calls []commitCall
	var cwd string
	for rows.Next() {
		var call commitCall
		if err := rows.Scan(&call.MessageOrdinal, &call.CallIndex, &call.ToolUseID, &call.CommittedAt, &call.input, &call.result, &cwd, &call.toolName); err != nil {
			return err
		}
		calls = append(calls, call)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM commit_links WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	for _, l := range commitLinks(calls, cwd) {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at) VALUES (?,?,?,?,?,?,?,?,?)`, sessionID, l.SHA, l.Branch, l.Subject, l.Repository, l.MessageOrdinal, l.CallIndex, l.ToolUseID, l.CommittedAt); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) backfillCommitLinksLocked(ctx context.Context, w *writerHandle) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM stats WHERE key=? AND value!=0`, commitLinksBackfillKey).Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM tool_calls WHERE category='Bash' AND input_json LIKE '%commit%'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshSessionCommitLinksTx(tx, id); err != nil {
			return fmt.Errorf("backfilling commit links: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO stats(key,value) VALUES (?,1)`, commitLinksBackfillKey); err != nil {
		return err
	}
	return tx.Commit()
}

const commitLinkFields = `l.session_id,l.sha,l.branch,l.subject,l.repository,l.message_ordinal,l.call_index,l.tool_use_id,l.committed_at`

const commitLinkSelect = `SELECT ` + commitLinkFields + `,
 (s.id IS NULL OR s.deleted_at IS NOT NULL),COALESCE(s.project,''),turn.ordinal,turn.content` + commitLinkFrom

const commitLinkFrom = ` FROM commit_links l LEFT JOIN sessions s ON s.id=l.session_id
 LEFT JOIN messages turn ON turn.session_id=s.id AND turn.ordinal=(
 SELECT m.ordinal FROM messages m WHERE m.session_id=s.id AND m.ordinal<=l.message_ordinal
 AND m.role='user' AND m.is_system=0 AND COALESCE(m.source_subtype,'')<>'tool_result' AND TRIM(m.content)<>''
 ORDER BY m.ordinal DESC LIMIT 1)
 WHERE (s.id IS NOT NULL OR EXISTS(SELECT 1 FROM excluded_sessions e WHERE e.id=l.session_id)) AND `

func readCommitLinks(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]CommitLink, error) {
	rows, err := sessionCommitLinkRows(ctx, tx, where, args...)
	if err != nil {
		return nil, err
	}
	links := make([]CommitLink, 0, len(rows))
	for _, r := range rows {
		l := r.CommitLink
		if !l.Missing && r.TurnOrdinal != nil {
			l.Turn = &CommitTurn{Ordinal: *r.TurnOrdinal, Text: r.TurnText}
		}
		links = append(links, l)
	}
	return links, nil
}

func (d *DB) SessionCommitLinks(ctx context.Context, id string) (*SessionCommitList, error) {
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	links, err := readCommitLinks(ctx, tx, "l.session_id=?", id)
	if err != nil {
		return nil, err
	}
	return &SessionCommitList{Commits: links}, tx.Commit()
}

func (d *DB) CommitSessionLinks(ctx context.Context, shas []string) (*CommitSessionList, error) {
	queries, err := NormalizeCommitSHAs(shas)
	if err != nil {
		return nil, err
	}
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := &CommitSessionList{Commits: make([]CommitSessionMatch, 0, len(shas))}
	for _, q := range queries {
		args := []any{q, q + "g"}
		placeholders := []string{}
		for i := 7; i <= len(q); i++ {
			placeholders = append(placeholders, "?")
			args = append(args, q[:i])
		}
		links, err := readCommitLinks(ctx, tx, `(l.sha>=? AND l.sha<? OR l.sha IN (`+strings.Join(placeholders, ",")+`))`, args...)
		if err != nil {
			return nil, err
		}
		MarkCommitAmbiguity(links)
		out.Commits = append(out.Commits, CommitSessionMatch{SHA: q, Links: links})
	}
	return out, tx.Commit()
}

// NormalizeCommitSHAs validates and lowercases lookup queries.
func NormalizeCommitSHAs(shas []string) ([]string, error) {
	if len(shas) > MaxCommitSHAs {
		return nil, fmt.Errorf("provide at most %d commit SHAs: %w", MaxCommitSHAs, ErrInvalidCommitSHA)
	}
	queries := make([]string, len(shas))
	for i, sha := range shas {
		queries[i] = strings.ToLower(sha)
		if !commitSHARE.MatchString(queries[i]) {
			return nil, ErrInvalidCommitSHA
		}
	}
	return queries, nil
}

// MarkCommitAmbiguity marks matches that cannot describe a single commit.
func MarkCommitAmbiguity(links []CommitLink) {
	longest := ""
	for _, link := range links {
		if len(link.SHA) > len(longest) {
			longest = link.SHA
		}
	}
	ambiguous := false
	for _, link := range links {
		if !strings.HasPrefix(longest, link.SHA) {
			ambiguous = true
		}
	}
	for i := range links {
		links[i].Ambiguous = ambiguous
	}
}

// CommitLinkRow carries a prompt copied with its commit link for replica reads.
type CommitLinkRow struct {
	CommitLink
	TurnOrdinal *int
	TurnText    string
	Project     string
}

func (d *DB) SessionCommitLinkRows(ctx context.Context, id string) ([]CommitLinkRow, error) {
	return sessionCommitLinkRows(ctx, d.getReader(), "l.session_id=?", id)
}

// RetainedCommitLinkRows returns links whose sessions were permanently deleted.
func (d *DB) RetainedCommitLinkRows(ctx context.Context) ([]CommitLinkRow, error) {
	return sessionCommitLinkRows(ctx, d.getReader(), "s.id IS NULL")
}

func sessionCommitLinkRows(ctx context.Context, q messageRowsQuerier, where string, args ...any) ([]CommitLinkRow, error) {
	rows, err := q.QueryContext(ctx, commitLinkSelect+where+" ORDER BY l.session_id,l.message_ordinal,l.call_index,l.sha", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitLinkRow
	for rows.Next() {
		var r CommitLinkRow
		var ordinal sql.NullInt64
		var text sql.NullString
		if err := rows.Scan(&r.SessionID, &r.SHA, &r.Branch, &r.Subject, &r.Repository, &r.MessageOrdinal, &r.CallIndex, &r.ToolUseID, &r.CommittedAt, &r.Missing, &r.Project, &ordinal, &text); err != nil {
			return nil, err
		}
		r.Source = "transcript"
		if ordinal.Valid {
			r.TurnOrdinal = new(int(ordinal.Int64))
			r.TurnText = stringutil.SafeTruncate(text.String, 2000)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
