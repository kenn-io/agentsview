package readbase

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type CommitLinkQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readCommitLinks(ctx context.Context, q CommitLinkQuerier, where string, args ...any) ([]db.CommitLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.session_id,l.sha,l.branch,l.subject,l.repository,l.message_ordinal,l.call_index,l.tool_use_id,l.committed_at,
 (COALESCE(s.id,'')='' OR s.deleted_at IS NOT NULL),l.turn_ordinal,l.turn_text
 FROM commit_links l LEFT JOIN sessions s ON s.id=l.session_id
 WHERE (l.retained OR COALESCE(s.id,'')<>'') AND (`+where+`) ORDER BY l.session_id,l.message_ordinal,l.call_index,l.sha`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []db.CommitLink{}
	for rows.Next() {
		var l db.CommitLink
		var ordinal sql.NullInt64
		var text string
		if err := rows.Scan(&l.SessionID, &l.SHA, &l.Branch, &l.Subject, &l.Repository, &l.MessageOrdinal, &l.CallIndex, &l.ToolUseID, &l.CommittedAt, &l.Missing, &ordinal, &text); err != nil {
			return nil, err
		}
		l.Source = "transcript"
		if !l.Missing && ordinal.Valid {
			l.Turn = &db.CommitTurn{Ordinal: int(ordinal.Int64), Text: text}
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func SessionCommitLinks(ctx context.Context, q CommitLinkQuerier, dialect db.QueryDialect, id string) (*db.SessionCommitList, error) {
	b := db.NewQueryBuilder(dialect, 0)
	links, err := readCommitLinks(ctx, q, "l.session_id="+b.Add(id), b.Args()...)
	if err != nil {
		return nil, err
	}
	return &db.SessionCommitList{Commits: links}, nil
}

func CommitSessionLinks(ctx context.Context, q CommitLinkQuerier, dialect db.QueryDialect, shas []string) (*db.CommitSessionList, error) {
	queries, err := db.NormalizeCommitSHAs(shas)
	if err != nil {
		return nil, err
	}
	out := &db.CommitSessionList{Commits: make([]db.CommitSessionMatch, 0, len(queries))}
	b := db.NewQueryBuilder(dialect, 0)
	var predicates []string
	for _, sha := range queries {
		predicates = append(predicates, "l.sha LIKE "+b.Add(sha+"%")+" OR "+b.Add(sha)+" LIKE l.sha || '%'")
	}
	links, err := readCommitLinks(ctx, q, strings.Join(predicates, " OR "), b.Args()...)
	if err != nil {
		return nil, err
	}
	for _, sha := range queries {
		matches := []db.CommitLink{}
		for _, l := range links {
			if strings.HasPrefix(l.SHA, sha) || strings.HasPrefix(sha, l.SHA) {
				matches = append(matches, l)
			}
		}
		db.MarkCommitAmbiguity(matches)
		out.Commits = append(out.Commits, db.CommitSessionMatch{SHA: sha, Links: matches})
	}
	return out, nil
}

// RetainedCommitLinks refreshes only this archive's absent sessions. Stored
// projects survive session deletion, so filtered pushes can retain their links.
func RetainedCommitLinks(ctx context.Context, local *db.DB, q CommitLinkQuerier, dialect db.QueryDialect, projects, exclude []string) ([]string, []db.CommitLinkRow, error) {
	retained, err := local.RetainedCommitLinkRows(ctx)
	if err != nil || len(retained) == 0 {
		return nil, nil, err
	}
	var ids []string
	for _, r := range retained {
		if len(ids) == 0 || ids[len(ids)-1] != r.SessionID {
			ids = append(ids, r.SessionID)
		}
	}
	live := make(map[string]bool)
	err = db.QueryChunkedSize(ids, 200, func(batch []string) error {
		b := db.NewQueryBuilder(dialect, 0)
		ph := make([]string, 0, len(batch))
		for _, id := range batch {
			ph = append(ph, b.Add(id))
		}
		// Protect another archive's session, including one without any links.
		rows, err := q.QueryContext(ctx, `SELECT id FROM sessions WHERE id IN (`+strings.Join(ph, ",")+`)`, b.Args()...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			live[id] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	ids = slices.DeleteFunc(ids, func(id string) bool { return live[id] })
	out := []db.CommitLinkRow{}
	for _, r := range retained {
		if live[r.SessionID] || !db.ProjectMatchesPushScope(r.Project, projects, exclude) {
			continue
		}
		out = append(out, r)
	}
	return ids, out, nil
}
