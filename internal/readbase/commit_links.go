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

func readCommitLinks(ctx context.Context, q CommitLinkQuerier, retained bool, where string, args ...any) ([]db.CommitLink, error) {
	source := `SELECT session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained, '' AS source_archive_id, CASE WHEN retained THEN 2 ELSE 0 END AS priority FROM commit_links`
	if retained {
		// NULLIF supplies a typed null for both PostgreSQL and ClickHouse.
		source += ` UNION ALL SELECT *, 1 AS priority FROM (SELECT session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,NULLIF(0,0) AS turn_ordinal,'' AS turn_text,'' AS project,TRUE AS retained,source_archive_id FROM retained_commit_links) owned`
	}
	rows, err := q.QueryContext(ctx, `SELECT session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,missing,turn_ordinal,turn_text FROM (
 SELECT l.session_id,l.sha,l.branch,l.subject,l.repository,l.message_ordinal,l.call_index,l.tool_use_id,l.committed_at,
 (COALESCE(s.id,'')='' OR s.deleted_at IS NOT NULL) AS missing,l.turn_ordinal,l.turn_text,
 ROW_NUMBER() OVER (PARTITION BY l.session_id,l.sha ORDER BY l.priority,l.source_archive_id) AS rank
 FROM (`+source+`) l LEFT JOIN sessions s ON s.id=l.session_id
 WHERE (l.retained OR COALESCE(s.id,'')<>'') AND (NOT l.retained OR COALESCE(s.id,'')='') AND (`+where+`)
 ) ranked WHERE rank=1 ORDER BY session_id,message_ordinal,committed_at,call_index,sha`, args...)
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
		// Trailer links have no transcript tool call.
		if l.CallIndex == -1 {
			l.Source = "trailer"
		}
		if !l.Missing && ordinal.Valid {
			l.Turn = &db.CommitTurn{Heuristic: l.Source == "trailer", Ordinal: int(ordinal.Int64), Text: text}
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func SessionCommitLinks(ctx context.Context, q CommitLinkQuerier, dialect db.QueryDialect, retained bool, id string) (*db.SessionCommitList, error) {
	b := db.NewQueryBuilder(dialect, 0)
	links, err := readCommitLinks(ctx, q, retained, "l.session_id="+b.Add(id), b.Args()...)
	if err != nil {
		return nil, err
	}
	return &db.SessionCommitList{Commits: links}, nil
}

func CommitSessionLinks(ctx context.Context, q CommitLinkQuerier, dialect db.QueryDialect, retained bool, shas []string) (*db.CommitSessionList, error) {
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
	links, err := readCommitLinks(ctx, q, retained, strings.Join(predicates, " OR "), b.Args()...)
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

// ScopedRetainedCommitLinks returns the archive's policy-scoped retained snapshot.
func ScopedRetainedCommitLinks(ctx context.Context, local *db.DB, projects, exclude []string) ([]db.CommitLinkRow, error) {
	rows, err := local.RetainedCommitLinkRows(ctx)
	if err != nil {
		return nil, err
	}
	return filterRetainedCommitLinks(rows, projects, exclude), nil
}

func filterRetainedCommitLinks(rows []db.CommitLinkRow, projects, exclude []string) []db.CommitLinkRow {
	out := make([]db.CommitLinkRow, 0, len(rows))
	for _, r := range rows {
		if db.ProjectMatchesPushScope(r.Project, projects, exclude) {
			out = append(out, r)
		}
	}
	return out
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
	for _, r := range filterRetainedCommitLinks(retained, projects, exclude) {
		if !live[r.SessionID] {
			out = append(out, r)
		}
	}
	return ids, out, nil
}
