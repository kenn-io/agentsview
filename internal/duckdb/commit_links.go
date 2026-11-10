package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

func (s *Store) SessionCommitLinks(ctx context.Context, id string) (*db.SessionCommitList, error) {
	return readbase.SessionCommitLinks(ctx, sessionsSQL{Store: s}, db.DuckDBQueryDialect(), false, id)
}

func (s *Store) CommitSessionLinks(ctx context.Context, shas []string) (*db.CommitSessionList, error) {
	return readbase.CommitSessionLinks(ctx, sessionsSQL{Store: s}, db.DuckDBQueryDialect(), false, shas)
}

func insertCommitLinks(ctx context.Context, exec duckMutationExecutor, rows []db.CommitLinkRow, retained bool) error {
	for _, r := range rows {
		if _, err := exec.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.SessionID, r.SHA, r.Branch, r.Subject, r.Repository, r.MessageOrdinal, r.CallIndex, r.ToolUseID, r.CommittedAt, r.TurnOrdinal, r.TurnText, r.Project, retained); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) replaceCommitLinks(ctx context.Context, exec duckMutationExecutor, id string) error {
	rows, err := s.local.SessionCommitLinkRows(ctx, id)
	if err != nil {
		return err
	}
	return insertCommitLinks(ctx, exec, rows, false)
}

func (s *Sync) replaceRetainedCommitLinks(ctx context.Context) error {
	ids, rows, err := readbase.RetainedCommitLinks(ctx, s.local, s.duck, db.DuckDBQueryDialect(), s.projects, s.excludeProjects)
	if err != nil {
		return err
	}
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "DELETE FROM commit_links WHERE session_id=?", id); err != nil {
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
