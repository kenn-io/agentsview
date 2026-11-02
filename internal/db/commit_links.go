package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	MaxCommitSHAs          = 200
	commitLinksBackfillKey = "commit_links_backfill_v1"
)

var (
	ErrInvalidCommitSHA       = errors.New("commit SHA must contain 7 to 64 hexadecimal characters")
	ErrCommitLinksUnavailable = errors.New("commit links are unavailable from this backend")
	commitSHARE               = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	commitSummaryRE           = regexp.MustCompile(`(?m)^\[(.+?) (?:\(root-commit\) )?([0-9a-f]{7,64})\] (.*)$`)
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

// commitRepository finds git's commit subcommand after its global options.
// Paths retain literal backslashes, including unquoted backslash-escaped spaces.
func commitRepository(input, cwd string) (string, bool) {
	var payload struct {
		Workdir string `json:"workdir"`
		CWD     string `json:"cwd"`
	}
	_ = json.Unmarshal([]byte(input), &payload)
	if payload.Workdir != "" {
		cwd = payload.Workdir
	} else if payload.CWD != "" {
		cwd = payload.CWD
	}
	resolve := func(base, dir string) string {
		if filepath.IsAbs(dir) {
			return dir
		}
		return filepath.Join(base, dir)
	}
	for _, words := range parser.ShellCommandSegments(signals.CommandText(input)) {
		if len(words) == 2 && words[0] == "cd" {
			cwd = resolve(cwd, words[1])
			continue
		}
		if len(words) < 2 || words[0] != "git" {
			continue
		}
		repo := cwd
		for i := 1; i < len(words); i++ {
			arg := words[i]
			if arg == "commit" {
				return repo, true
			}
			if !strings.HasPrefix(arg, "-") {
				break
			}
			if (arg == "-C" || arg == "-c") && i+1 < len(words) {
				i++
				if arg == "-C" {
					repo = resolve(repo, words[i])
				}
			}
		}
	}
	return "", false
}

func refreshSessionCommitLinksTx(tx transactionQueries, sessionID string) error {
	rows, err := tx.Query(`SELECT m.ordinal, COALESCE(t.call_index,0), COALESCE(t.tool_use_id,''),
 COALESCE(m.timestamp,''), t.input_json, COALESCE(t.result_content,''), s.cwd
 FROM tool_calls t JOIN messages m ON m.id=t.message_id JOIN sessions s ON s.id=t.session_id
 WHERE t.session_id=? AND t.category='Bash' AND t.input_json LIKE '%commit%'`, sessionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var links []CommitLink
	for rows.Next() {
		var l CommitLink
		var input, output, cwd string
		if err := rows.Scan(&l.MessageOrdinal, &l.CallIndex, &l.ToolUseID, &l.CommittedAt, &input, &output, &cwd); err != nil {
			return err
		}
		repo, ok := commitRepository(input, cwd)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, match := range commitSummaryRE.FindAllStringSubmatch(output, -1) {
			if seen[match[2]] {
				continue
			}
			seen[match[2]] = true
			l.SHA, l.Branch, l.Subject, l.Repository = match[2], match[1], match[3], repo
			links = append(links, l)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM commit_links WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	for _, l := range links {
		if _, err := tx.Exec(`INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at) VALUES (?,?,?,?,?,?,?,?,?)`, sessionID, l.SHA, l.Branch, l.Subject, l.Repository, l.MessageOrdinal, l.CallIndex, l.ToolUseID, l.CommittedAt); err != nil {
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

const commitLinkSelect = `SELECT l.session_id,l.sha,l.branch,l.subject,l.repository,l.message_ordinal,l.call_index,l.tool_use_id,l.committed_at,
 (s.id IS NULL OR s.deleted_at IS NOT NULL),
 (SELECT m.ordinal FROM messages m WHERE m.session_id=s.id AND m.ordinal<=l.message_ordinal AND m.role='user' AND m.is_system=0 ORDER BY m.ordinal DESC LIMIT 1),
 (SELECT m.content FROM messages m WHERE m.session_id=s.id AND m.ordinal<=l.message_ordinal AND m.role='user' AND m.is_system=0 ORDER BY m.ordinal DESC LIMIT 1)
 FROM commit_links l LEFT JOIN sessions s ON s.id=l.session_id
 WHERE (s.id IS NOT NULL OR EXISTS(SELECT 1 FROM excluded_sessions e WHERE e.id=l.session_id)) AND `

func readCommitLinks(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]CommitLink, error) {
	rows, err := tx.QueryContext(ctx, commitLinkSelect+where+` ORDER BY l.session_id,l.message_ordinal,l.call_index,l.sha`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []CommitLink{}
	for rows.Next() {
		var l CommitLink
		var ordinal sql.NullInt64
		var text sql.NullString
		if err := rows.Scan(&l.SessionID, &l.SHA, &l.Branch, &l.Subject, &l.Repository, &l.MessageOrdinal, &l.CallIndex, &l.ToolUseID, &l.CommittedAt, &l.Missing, &ordinal, &text); err != nil {
			return nil, err
		}
		l.Source = "transcript"
		if !l.Missing && ordinal.Valid {
			l.Turn = &CommitTurn{Ordinal: int(ordinal.Int64), Text: stringutil.SafeTruncate(text.String, 2000)}
		}
		links = append(links, l)
	}
	return links, rows.Err()
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
		out.Commits = append(out.Commits, CommitSessionMatch{SHA: q, Links: links})
	}
	return out, tx.Commit()
}
