package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	MaxCommitSHAs          = 200
	commitLinksBackfillKey = "commit_links_backfill_v1"
	commitCommandToken     = `(?:"(?:\\[\s\S]|[^"\\])*"|'[^']*'|[^\s;&|"']+)`
	commitGlobalOption     = `(?:-[cC]\s+` + commitCommandToken + `|--(?:git-dir|work-tree|namespace|config-env)(?:=|\s+)` + commitCommandToken + `|--(?:no-pager|paginate|bare|no-replace-objects|literal-pathspecs|glob-pathspecs|noglob-pathspecs|icase-pathspecs|no-optional-locks|no-lazy-fetch))`
)

var (
	ErrInvalidCommitSHA       = errors.New("commit SHA must contain 7 to 64 hexadecimal characters")
	ErrCommitLinksUnavailable = errors.New("commit links are unavailable from this backend")
	commitSHARE               = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	commitSummaryRE           = regexp.MustCompile(`(?m)^\[(.+?) (?:\(root-commit\) )?([0-9a-f]{7,64})\] ([^\r\n]*)\r?$`)
	commitInvocationRE        = regexp.MustCompile(`(?:^|[\s;&|(/]|\\[nrt]|(?:(?:cmd|command)["']?\s*:|(?:^|[\s/])(?:ba|da|z|k)?sh\s+(?:-\w+\s+)*-\w*c\s|\()\s*["'\x60])git\s+(?:` + commitGlobalOption + `\s+)*commit(?:$|[\s;&|)'"\x60\\])`)
	commitArgumentListRE      = regexp.MustCompile(`\[\s*["']git["']\s*,\s*["']commit["']`)
	commitWindowsDriveRE      = regexp.MustCompile(`^[A-Za-z]:`)
	commitWorkdirRE           = regexp.MustCompile(`["']?(?:workdir|cwd)["']?\s*:\s*("(?:\\.|[^"\\])*"|'[^']*')`)
	commitCommandRE           = regexp.MustCompile(`["']?(?:cmd|command)["']?\s*:\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|\x60(?:\\.|[^\x60\\])*\x60)`)
	commitCDRE                = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|[^\s;&|]+)`)
	commitGitDirRE            = regexp.MustCompile(`\bgit\s+(?:` + commitGlobalOption + `\s+)*?-C\s+("[^"]*"|'[^']*'|[^\s;&|]+)`)
	commitPollRE              = regexp.MustCompile(`\btools\.write_stdin\s*\(\s*\{[^{}]*["']?session_id["']?\s*:\s*([0-9]+)`)
	commitRunningRE           = regexp.MustCompile(`(?m)^Process running with session ID ([0-9]+)\r?$`)
	commitCellRE              = regexp.MustCompile(`(?m)^Script running with cell ID (\S+)\r?$`)
)

// CommitLinkReader is the optional archive capability used by HTTP and CLI reads.
type CommitLinkReader interface {
	SessionCommitLinks(context.Context, string) (*SessionCommitList, error)
	CommitSessionLinks(context.Context, []string) (*CommitSessionList, error)
}

type CommitTurn struct {
	Heuristic bool   `json:"heuristic"`
	Ordinal   int    `json:"ordinal"`
	Text      string `json:"text"`
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
		if match := commitWorkdirRE.FindStringSubmatch(commitCommandRE.ReplaceAllString(input, "")); match != nil {
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
	if !jsontext.Value(input).IsValid() {
		if match := commitCommandRE.FindStringSubmatch(input); match != nil {
			literal := match[1]
			var decoded strings.Builder
			for rest := literal[1 : len(literal)-1]; rest != ""; {
				r, _, tail, err := strconv.UnquoteChar(rest, literal[0])
				if err != nil {
					break
				}
				decoded.WriteRune(r)
				rest = tail
				if rest == "" {
					command = decoded.String()
				}
			}
		}
	}
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

// commitOutput keeps an explicitly identified result separate from other results.
type commitOutput struct {
	text, process string
}

// callOutput decodes shell results and all their running process or cell IDs.
func callOutput(result string) ([]commitOutput, []string) {
	var payload struct {
		Output      *string `json:"output"`
		OutputDelta *string `json:"output_delta"`
		SessionID   int64   `json:"session_id"`
	}
	if json.Unmarshal([]byte(result), &payload) == nil {
		output := payload.Output
		if output == nil {
			output = payload.OutputDelta
		}
		if output != nil {
			var running []string
			process := ""
			if payload.SessionID != 0 {
				process = strconv.FormatInt(payload.SessionID, 10)
				running = append(running, process)
			}
			return []commitOutput{{text: *output, process: process}}, running
		}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(result), &blocks) == nil && len(blocks) > 0 && blocks[0].Type == "input_text" {
		var outputs []commitOutput
		var running []string
		for _, block := range blocks {
			output, processes := callOutput(block.Text)
			outputs = append(outputs, output...)
			running = append(running, processes...)
		}
		return outputs, running
	}
	var running []string
	for _, match := range commitRunningRE.FindAllStringSubmatch(result, -1) {
		running = append(running, match[1])
	}
	for _, match := range commitCellRE.FindAllStringSubmatch(result, -1) {
		running = append(running, "cell:"+match[1])
	}
	process := ""
	if len(running) == 1 {
		process = running[0]
	}
	return []commitOutput{{text: result, process: process}}, running
}

func commitPollIDs(input, tool string) []string {
	var args struct {
		SessionID int64  `json:"session_id"`
		CellID    string `json:"cell_id"`
	}
	switch tool {
	case "write_stdin", "wait":
		if json.Unmarshal([]byte(input), &args) == nil {
			if tool == "wait" && args.CellID != "" {
				return []string{"cell:" + args.CellID}
			}
			if tool == "write_stdin" && args.SessionID != 0 {
				return []string{strconv.FormatInt(args.SessionID, 10)}
			}
		}
	case "exec":
		var ids []string
		for _, match := range commitPollRE.FindAllStringSubmatch(input, -1) {
			if !slices.Contains(ids, match[1]) {
				ids = append(ids, match[1])
			}
		}
		return ids
	}
	return nil
}

// invokesGitCommit reports whether command runs git commit rather than quoting it as text.
func invokesGitCommit(command string) bool {
	return commitInvocationRE.MatchString(command) || commitArgumentListRE.MatchString(command)
}

func commitLinks(calls []commitCall, cwd string) []CommitLink {
	var links []CommitLink
	pending := map[string]commitCall{}
	seen := map[string]bool{}
	for _, call := range calls {
		outputs, running := callOutput(call.result)
		command := signals.CommandText(call.input)
		isCommit := invokesGitCommit(command)
		pollIDs := commitPollIDs(call.input, call.toolName)
		if isCommit {
			for _, process := range running {
				pending[process] = cmp.Or(pending[process], call)
			}
		} else if len(pollIDs) == 0 {
			continue
		}
		// One poll owns every output block, even when its process has finished.
		singleOwner, singlePoll := commitCall{}, !isCommit && len(pollIDs) == 1
		if singlePoll {
			var ok bool
			singleOwner, ok = pending[pollIDs[0]]
			if !ok {
				continue
			}
			delete(pending, pollIDs[0])
			for _, process := range running {
				pending[process] = singleOwner
			}
		}
		for _, output := range outputs {
			owner := cmp.Or(pending[output.process], call)
			if singlePoll {
				owner = singleOwner
			} else if !isCommit {
				pollID := output.process
				if !slices.Contains(pollIDs, pollID) {
					continue
				}
				original, ok := pending[pollID]
				if !ok {
					continue
				}
				owner = original
			}
			for _, match := range commitSummaryRE.FindAllStringSubmatch(output.text, -1) {
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
	}
	return links
}

func refreshSessionCommitLinksTx(tx transactionQueries, sessionID string) error {
	var calls []commitCall
	var cwd string
	load := func(filter string, args ...any) error {
		rows, err := tx.Query(`SELECT m.ordinal, COALESCE(t.call_index,0), COALESCE(t.tool_use_id,''),
 COALESCE(m.timestamp,''), t.input_json, `+ToolCallResultContentSQL("t", "m.ordinal")+`, COALESCE(s.cwd,''), t.tool_name
 FROM tool_calls t JOIN messages m ON m.id=t.message_id JOIN sessions s ON s.id=t.session_id
 WHERE t.session_id=? AND (t.category='Bash' OR t.tool_name='wait') AND t.input_json IS NOT NULL AND (`+filter+`)
 ORDER BY m.ordinal,t.call_index`, append([]any{sessionID}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var call commitCall
			if err := rows.Scan(&call.MessageOrdinal, &call.CallIndex, &call.ToolUseID, &call.CommittedAt, &call.input, &call.result, &cwd, &call.toolName); err != nil {
				return err
			}
			calls = append(calls, call)
		}
		return rows.Err()
	}
	if err := load(`t.input_json LIKE '%git%' AND t.input_json LIKE '%commit%'`); err != nil {
		return err
	}
	if len(calls) == 0 {
		var stored bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM commit_links WHERE session_id=?)`, sessionID).Scan(&stored); err != nil {
			return err
		}
		if !stored {
			return nil
		}
	}
	running := map[string]bool{}
	for _, call := range calls {
		command := signals.CommandText(call.input)
		if invokesGitCommit(command) {
			_, processes := callOutput(call.result)
			for _, process := range processes {
				running[process] = true
			}
		}
	}
	if len(running) > 0 {
		// Read poll inputs first so unrelated process output is never loaded.
		rows, err := tx.Query(`SELECT id, input_json, tool_name FROM tool_calls
 WHERE session_id=? AND input_json IS NOT NULL
 AND ((category='Bash' AND (tool_name='write_stdin' OR (tool_name='exec' AND input_json LIKE '%write_stdin%')))
 OR (tool_name='wait' AND input_json LIKE '%cell_id%'))
 AND NOT (input_json LIKE '%git%' AND input_json LIKE '%commit%')`, sessionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id, input, tool string
			if err := rows.Scan(&id, &input, &tool); err != nil {
				return err
			}
			for _, process := range commitPollIDs(input, tool) {
				if running[process] {
					ids = append(ids, id)
					break
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			for chunk := range slices.Chunk(ids, maxSQLVars-1) {
				placeholders, args := InPlaceholders(chunk)
				if err := load(`t.id IN `+placeholders, args...); err != nil {
					return err
				}
			}
			slices.SortFunc(calls, func(a, b commitCall) int {
				if order := cmp.Compare(a.MessageOrdinal, b.MessageOrdinal); order != 0 {
					return order
				}
				return cmp.Compare(a.CallIndex, b.CallIndex)
			})
		}
	}
	if _, err := tx.Exec(`DELETE FROM commit_links WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	for _, l := range commitLinks(calls, cwd) {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,project) SELECT ?,?,?,?,?,?,?,?,?,project FROM sessions WHERE id=?`, sessionID, l.SHA, l.Branch, l.Subject, l.Repository, l.MessageOrdinal, l.CallIndex, l.ToolUseID, l.CommittedAt, sessionID); err != nil {
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

// The time fallback adapts Pando's turnThatWrote; see
// docs/internal/pando-adaptation.md for attribution and changes.
const commitUserPrompt = `m.role='user' AND m.is_system=0 AND COALESCE(m.source_subtype,'')<>'tool_result' AND TRIM(m.content)<>''`

const commitLinkSelect = `WITH trailer_owners AS (SELECT key AS trailer_id,value AS session_id FROM json_each(?)),
 resolved_trailers AS (
 SELECT t.*,o.session_id AS owner_id,
 ROW_NUMBER() OVER (PARTITION BY o.session_id,t.sha ORDER BY t.session_id) AS owner_rank
 FROM commit_trailers t JOIN trailer_owners o ON o.trailer_id=t.session_id)
 SELECT l.session_id,l.sha,l.branch,l.subject,l.repository,l.message_ordinal,l.call_index,l.tool_use_id,l.committed_at,l.source,
 (s.id IS NULL OR s.deleted_at IS NOT NULL),COALESCE(s.project,l.project),
 turn.ordinal, turn.content
 FROM (
 SELECT session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,project,'transcript' AS source FROM commit_links
 UNION ALL
 SELECT t.owner_id,t.sha,t.branch,t.subject,t.repository,
 COALESCE((SELECT m.ordinal FROM messages m WHERE m.session_id=t.owner_id AND ` + commitUserPrompt + `
 AND julianday(m.timestamp)<=julianday(t.committed_at,'+5 minutes') ORDER BY m.ordinal DESC LIMIT 1),-1),
 -1,'',t.committed_at,t.project,'trailer' FROM resolved_trailers t
 WHERE t.owner_rank=1 AND NOT EXISTS(SELECT 1 FROM commit_links c WHERE c.session_id=t.owner_id AND substr(t.sha,1,length(c.sha))=c.sha)
 ) l LEFT JOIN sessions s ON s.id=l.session_id
 LEFT JOIN messages turn ON turn.session_id=s.id AND turn.ordinal=(
 SELECT m.ordinal FROM messages m WHERE m.session_id=s.id AND m.ordinal<=l.message_ordinal
 AND ` + commitUserPrompt + `
 ORDER BY m.ordinal DESC LIMIT 1)
 WHERE (s.id IS NOT NULL OR EXISTS(SELECT 1 FROM excluded_sessions e WHERE e.id=l.session_id)) AND `

// StoreTrailerCommitLinks adds or updates one repository's trailer evidence.
// Existing evidence is retained; an empty result leaves it unchanged.
// The stored session_id is the trailer's agent:ID, independent of archive owners.
func (d *DB) StoreTrailerCommitLinks(ctx context.Context, repo string, commits []gitlog.TrailerCommit) error {
	return d.Update(ctx, func(tx *sql.Tx) error {
		changed := map[string]bool{}
		if d.ArchiveContent().UsageOnly() {
			commits = nil
		}
		for _, c := range commits {
			for _, value := range c.Sessions {
				agent, _, ok := strings.Cut(value, ":")
				_, known := parser.AgentByPrefix(strings.TrimPrefix(agent+":", "claude:"))
				if !ok || !known {
					continue
				}
				result, err := tx.ExecContext(ctx, `INSERT INTO commit_trailers(session_id,sha,branch,subject,repository,committed_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(session_id,sha) DO UPDATE SET branch=COALESCE(NULLIF(commit_trailers.branch,''),excluded.branch),subject=excluded.subject,repository=COALESCE(NULLIF(commit_trailers.repository,''),excluded.repository),committed_at=excluded.committed_at
 WHERE (commit_trailers.branch='' AND excluded.branch<>'') OR commit_trailers.subject<>excluded.subject OR (commit_trailers.repository='' AND excluded.repository<>'') OR commit_trailers.committed_at<>excluded.committed_at`, value, c.SHA, c.Branch, c.Subject, repo, c.CommittedAt)
				if err != nil {
					return err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if n > 0 {
					changed[value] = true
				}
			}
		}
		if len(changed) == 0 {
			return nil
		}
		identities := make([]string, 0, len(changed))
		for id := range changed {
			identities = append(identities, id)
		}
		owners, err := trailerIdentityOwners(identities)
		if err != nil {
			return err
		}
		// Trailer changes can arrive without a transcript write. Advance the same
		// session cursor that makes transcript changes eligible for mirror pushes.
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET local_modified_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id IN (SELECT value FROM json_each(?))`, owners)
		return err
	})
}

type TrailerRepository struct{ Machine, Cwd string }

// Preserve the resolved owner's current project before its session is purged.
func retainTrailerProjectsTx(ctx context.Context, tx *sql.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var identities []string
	for _, id := range ids {
		identities = append(identities, sessionTrailerIdentities(id)...)
	}
	owners, err := trailerIdentityOwners(identities)
	if err != nil {
		return err
	}
	selected, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE commit_trailers SET project=(
 SELECT s.project FROM sessions s JOIN json_each(?1) o ON o.value=s.id WHERE o.key=commit_trailers.session_id)
 WHERE session_id IN (SELECT o.key FROM json_each(?1) o JOIN json_each(?2) selected ON selected.value=o.value)`, owners, string(selected))
	return err
}

func (d *DB) TrailerRepositories(ctx context.Context) ([]TrailerRepository, error) {
	rows, err := d.getReader().QueryContext(ctx, `SELECT DISTINCT machine,cwd FROM sessions WHERE cwd<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []TrailerRepository
	for rows.Next() {
		var r TrailerRepository
		if err := rows.Scan(&r.Machine, &r.Cwd); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

func readCommitLinks(ctx context.Context, tx *sql.Tx, owners, where string, args ...any) ([]CommitLink, error) {
	rows, err := sessionCommitLinkRows(ctx, tx, owners, where, args...)
	if err != nil {
		return nil, err
	}
	links := make([]CommitLink, 0, len(rows))
	for _, r := range rows {
		l := r.CommitLink
		if !l.Missing && r.TurnOrdinal != nil {
			l.Turn = &CommitTurn{Heuristic: l.Source == "trailer", Ordinal: *r.TurnOrdinal, Text: r.TurnText}
		}
		links = append(links, l)
	}
	return links, nil
}

// Resolve in the read snapshot so imports and restores apply without a Git scan.
func resolveTrailerOwners(ctx context.Context, tx *sql.Tx, where string, args ...any) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM commit_trailers WHERE `+where, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var identities []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return "", err
		}
		identities = append(identities, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	return trailerIdentityOwners(identities)
}

func trailerIdentityOwners(identities []string) (string, error) {
	owners := make(map[string]string, len(identities))
	for _, value := range identities {
		agent, id, ok := strings.Cut(value, ":")
		def, known := parser.AgentByPrefix(strings.TrimPrefix(agent+":", "claude:"))
		if ok && known {
			owners[value] = def.IDPrefix + id
		}
	}
	encoded, err := json.Marshal(owners)
	return string(encoded), err
}

// Enumerate the trailer identities for this archive ID.
func sessionTrailerIdentities(id string) []string {
	identities := []string{"claude:" + id}
	if def, ok := parser.AgentByPrefix(id); ok && def.IDPrefix != "" {
		identities = append(identities, id)
	}
	return identities
}

func (d *DB) SessionCommitLinks(ctx context.Context, id string) (*SessionCommitList, error) {
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	identities, err := json.Marshal(sessionTrailerIdentities(id))
	if err != nil {
		return nil, err
	}
	owners, err := resolveTrailerOwners(ctx, tx, `session_id IN (SELECT value FROM json_each(?))`, string(identities))
	if err != nil {
		return nil, err
	}
	links, err := readCommitLinks(ctx, tx, owners, "l.session_id=?", id)
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
		shaFilter := `(sha>=? AND sha<? OR sha IN (` + strings.Join(placeholders, ",") + `))`
		owners, err := resolveTrailerOwners(ctx, tx, shaFilter, args...)
		if err != nil {
			return nil, err
		}
		links, err := readCommitLinks(ctx, tx, owners, strings.ReplaceAll(shaFilter, "sha", "l.sha"), args...)
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
	rows, err := d.SessionCommitLinkRowsBySession(ctx, []string{id})
	return rows[id], err
}

// SessionCommitLinkRowsBySession reads mirror links in one archive snapshot.
func (d *DB) SessionCommitLinkRowsBySession(ctx context.Context, ids []string) (map[string][]CommitLinkRow, error) {
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var identities []string
	for _, id := range ids {
		identities = append(identities, sessionTrailerIdentities(id)...)
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return nil, err
	}
	owners, err := resolveTrailerOwners(ctx, tx, `session_id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return nil, err
	}
	selected, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := sessionCommitLinkRows(ctx, tx, owners, `l.session_id IN (SELECT value FROM json_each(?))`, string(selected))
	if err != nil {
		return nil, err
	}
	out := make(map[string][]CommitLinkRow)
	for _, row := range rows {
		out[row.SessionID] = append(out[row.SessionID], row)
	}
	return out, tx.Commit()
}

// RetainedCommitLinkRows returns links whose sessions were permanently deleted.
func (d *DB) RetainedCommitLinkRows(ctx context.Context) ([]CommitLinkRow, error) {
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	owners, err := resolveTrailerOwners(ctx, tx, "1=1")
	if err != nil {
		return nil, err
	}
	rows, err := sessionCommitLinkRows(ctx, tx, owners, "s.id IS NULL")
	if err != nil {
		return nil, err
	}
	return rows, tx.Commit()
}

func sessionCommitLinkRows(ctx context.Context, q messageRowsQuerier, owners, where string, args ...any) ([]CommitLinkRow, error) {
	args = append([]any{owners}, args...)
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
		if err := rows.Scan(&r.SessionID, &r.SHA, &r.Branch, &r.Subject, &r.Repository, &r.MessageOrdinal, &r.CallIndex, &r.ToolUseID, &r.CommittedAt, &r.Source, &r.Missing, &r.Project, &ordinal, &text); err != nil {
			return nil, err
		}
		if ordinal.Valid {
			r.TurnOrdinal = new(int(ordinal.Int64))
			r.TurnText = stringutil.SafeTruncate(text.String, 2000)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
