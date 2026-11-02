package clickhouse

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

func (s *Store) SessionCommitLinks(ctx context.Context, id string) (*db.SessionCommitList, error) {
	return readbase.SessionCommitLinks(ctx, sessionsSQL{Store: s}, db.ClickHouseQueryDialect(), id)
}

func (s *Store) CommitSessionLinks(ctx context.Context, shas []string) (*db.CommitSessionList, error) {
	return readbase.CommitSessionLinks(ctx, sessionsSQL{Store: s}, db.ClickHouseQueryDialect(), shas)
}

func commitLinkRow(r db.CommitLinkRow, retained bool, version uint64) []any {
	return []any{r.SessionID, r.SHA, r.Branch, r.Subject, r.Repository, r.MessageOrdinal, r.CallIndex, r.ToolUseID, r.CommittedAt, nullIntPtr(r.TurnOrdinal), r.TurnText, r.Project, retained, version}
}

func (s *Sync) replaceRetainedCommitLinks(ctx context.Context, version uint64) error {
	ids, rows, err := readbase.RetainedCommitLinks(ctx, s.local, s.conn, db.ClickHouseQueryDialect(), s.projects, s.excludeProjects, nil)
	if err != nil {
		return err
	}
	var block [][]any
	for _, r := range rows {
		block = append(block, commitLinkRow(r, true, version))
	}
	if err := insertRows(ctx, s.conn, "commit_links", block); err != nil {
		return err
	}
	for batch := range idBatches(append(ids, s.deletedCommitSessions...)) {
		ph, args := inArgs(batch)
		if _, err := s.conn.ExecContext(ctx, "DELETE FROM commit_links WHERE session_id IN ("+ph+") AND push_version < ?", append(args, version)...); err != nil {
			return err
		}
	}
	s.deletedCommitSessions = nil

	return nil
}
