package postgres

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

func (s *Store) SessionCommitLinks(ctx context.Context, id string) (*db.SessionCommitList, error) {
	out, err := readbase.SessionCommitLinks(ctx, s.pg, db.PostgresQueryDialect(), id)
	if isUndefinedTable(err) {
		return nil, db.ErrCommitLinksUnavailable
	}
	return out, err
}

func (s *Store) CommitSessionLinks(ctx context.Context, shas []string) (*db.CommitSessionList, error) {
	out, err := readbase.CommitSessionLinks(ctx, s.pg, db.PostgresQueryDialect(), shas)
	if isUndefinedTable(err) {
		return nil, db.ErrCommitLinksUnavailable
	}
	return out, err
}

func insertCommitLinks(ctx context.Context, exec *sql.Tx, rows []db.CommitLinkRow, retained bool) error {
	for _, r := range rows {
		l := newPublishedCommitLink(r, retained)
		if _, err := exec.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, l.sessionID, l.sha, l.branch, l.subject, l.repository, l.messageOrdinal, l.callIndex, l.toolUseID, l.committedAt, l.turnOrdinal, l.turnText, l.project, l.retained); err != nil {
			return err
		}
	}
	return nil
}

// pushCommitLinks reports whether the published links changed. A trailer can
// arrive without any other session change, and session watchers see it only
// when the caller advances updated_at.
func (s *Sync) pushCommitLinks(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	rows, err := s.local.SessionCommitLinkRows(ctx, id)
	if err != nil {
		return false, err
	}
	published, err := publishedCommitLinks(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if len(published) == len(rows) && containsCommitLinks(published, rows) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM commit_links WHERE session_id=$1", id); err != nil {
		return false, err
	}
	return true, insertCommitLinks(ctx, tx, rows, false)
}

// publishedCommitLink holds every column insertCommitLinks writes.
type publishedCommitLink struct {
	sessionID, sha, branch, subject, repository string
	messageOrdinal, callIndex                   int
	toolUseID, committedAt                      string
	turnOrdinal                                 sql.NullInt64
	turnText, project                           string
	retained                                    bool
}

func newPublishedCommitLink(r db.CommitLinkRow, retained bool) publishedCommitLink {
	link := publishedCommitLink{
		sessionID: sanitizePG(r.SessionID), sha: sanitizePG(r.SHA),
		branch: sanitizePG(r.Branch), subject: sanitizePG(r.Subject),
		repository: sanitizePG(r.Repository), messageOrdinal: r.MessageOrdinal,
		callIndex: r.CallIndex, toolUseID: sanitizePG(r.ToolUseID),
		committedAt: sanitizePG(r.CommittedAt), turnText: sanitizePG(r.TurnText),
		project: sanitizePG(r.Project), retained: retained,
	}
	if r.TurnOrdinal != nil {
		link.turnOrdinal = sql.NullInt64{Int64: int64(*r.TurnOrdinal), Valid: true}
	}
	return link
}

func publishedCommitLinks(ctx context.Context, tx *sql.Tx, id string) (map[publishedCommitLink]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained FROM commit_links WHERE session_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	published := map[publishedCommitLink]struct{}{}
	for rows.Next() {
		var l publishedCommitLink
		if err := rows.Scan(&l.sessionID, &l.sha, &l.branch, &l.subject, &l.repository, &l.messageOrdinal, &l.callIndex, &l.toolUseID, &l.committedAt, &l.turnOrdinal, &l.turnText, &l.project, &l.retained); err != nil {
			return nil, err
		}
		published[l] = struct{}{}
	}
	return published, rows.Err()
}

// containsCommitLinks checks that every live row is already published.
// The primary key makes each row distinct, so equal counts mean equal sets.
func containsCommitLinks(published map[publishedCommitLink]struct{}, rows []db.CommitLinkRow) bool {
	for _, r := range rows {
		if _, ok := published[newPublishedCommitLink(r, false)]; !ok {
			return false
		}
	}
	return true
}

func (s *Sync) replaceRetainedCommitLinks(ctx context.Context) error {
	ids, rows, err := readbase.RetainedCommitLinks(ctx, s.local, s.pg, db.PostgresQueryDialect(), s.projects, s.excludeProjects)
	if err != nil {
		return err
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "DELETE FROM commit_links WHERE session_id=$1", id); err != nil {
			return err
		}
	}
	if err := insertCommitLinks(ctx, tx, rows, true); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_links WHERE NOT retained AND session_id NOT IN (SELECT id FROM sessions)`); err != nil {
		return err
	}
	return tx.Commit()
}
