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
		if _, err := exec.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, sanitizePG(r.SessionID), sanitizePG(r.SHA), sanitizePG(r.Branch), sanitizePG(r.Subject), sanitizePG(r.Repository), r.MessageOrdinal, r.CallIndex, sanitizePG(r.ToolUseID), sanitizePG(r.CommittedAt), r.TurnOrdinal, sanitizePG(r.TurnText), sanitizePG(r.Project), retained); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) pushCommitLinks(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := s.local.SessionCommitLinkRows(ctx, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM commit_links WHERE session_id=$1", id); err != nil {
		return err
	}
	return insertCommitLinks(ctx, tx, rows, false)
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
