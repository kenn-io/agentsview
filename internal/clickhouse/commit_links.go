package clickhouse

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

func (s *Store) SessionCommitLinks(ctx context.Context, id string) (*db.SessionCommitList, error) {
	out, err := readbase.SessionCommitLinks(ctx, sessionsSQL{Store: s}, db.ClickHouseQueryDialect(), id)
	if isMissingTableError(err) {
		return nil, db.ErrCommitLinksUnavailable
	}
	return out, err
}

func (s *Store) CommitSessionLinks(ctx context.Context, shas []string) (*db.CommitSessionList, error) {
	out, err := readbase.CommitSessionLinks(ctx, sessionsSQL{Store: s}, db.ClickHouseQueryDialect(), shas)
	if isMissingTableError(err) {
		return nil, db.ErrCommitLinksUnavailable
	}
	return out, err
}

func commitLinkRow(r db.CommitLinkRow, retained bool, version uint64) []any {
	return []any{r.SessionID, r.SHA, r.Branch, r.Subject, r.Repository, r.MessageOrdinal, r.CallIndex, r.ToolUseID, r.CommittedAt, nullIntPtr(r.TurnOrdinal), r.TurnText, r.Project, retained, version}
}

func (s *Sync) replaceRetainedCommitLinks(ctx context.Context, version uint64) error {
	ids, rows, err := readbase.RetainedCommitLinks(ctx, s.local, s.conn, db.ClickHouseQueryDialect(), s.projects, s.excludeProjects)
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
	for batch := range idBatches(ids) {
		ph, args := inArgs(batch)
		if _, err := s.conn.ExecContext(ctx, "DELETE FROM commit_links WHERE session_id IN ("+ph+") AND push_version < ?", append(args, version)...); err != nil {
			return err
		}
	}
	return nil
}
