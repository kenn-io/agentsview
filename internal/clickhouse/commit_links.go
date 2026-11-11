package clickhouse

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

func (s *Store) SessionCommitLinks(ctx context.Context, id string) (*db.SessionCommitList, error) {
	out, err := readbase.SessionCommitLinks(ctx, sessionsSQL{Store: s}, db.ClickHouseQueryDialect(), true, id)
	if isMissingTableError(err) {
		return nil, db.ErrCommitLinksUnavailable
	}
	return out, err
}

func (s *Store) CommitSessionLinks(ctx context.Context, shas []string) (*db.CommitSessionList, error) {
	out, err := readbase.CommitSessionLinks(ctx, sessionsSQL{Store: s}, db.ClickHouseQueryDialect(), true, shas)
	if isMissingTableError(err) {
		return nil, db.ErrCommitLinksUnavailable
	}
	return out, err
}

func commitLinkBaseRow(r db.CommitLinkRow) []any {
	return []any{r.SessionID, r.SHA, r.Branch, r.Subject, r.Repository, r.MessageOrdinal, r.CallIndex, r.ToolUseID, r.CommittedAt}
}

func commitLinkRow(r db.CommitLinkRow, retained bool, version uint64) []any {
	return append(commitLinkBaseRow(r), nullIntPtr(r.TurnOrdinal), r.TurnText, r.Project, retained, version)
}

func (s *Sync) replaceRetainedCommitLinks(ctx context.Context, version uint64) error {
	rows, err := readbase.ScopedRetainedCommitLinks(ctx, s.local, s.projects, s.excludeProjects)
	if err != nil {
		return err
	}
	var block [][]any
	for _, r := range rows {
		block = append(block, append(append([]any{s.archiveID}, commitLinkBaseRow(r)...), version))
	}
	if err := insertRows(ctx, s.conn, "retained_commit_links", block); err != nil {
		return err
	}
	predicate := `source_archive_id = ? AND push_version < ? AND (source_archive_id,sha,session_id) NOT IN (SELECT source_archive_id,sha,session_id FROM retained_commit_links WHERE source_archive_id = ? AND push_version >= ? SETTINGS final = 0)`
	args := []any{s.archiveID, version, s.archiveID, version}
	var vanished uint64
	if err := s.conn.QueryRowContext(ctx, `SELECT count() FROM retained_commit_links WHERE `+predicate, args...).Scan(&vanished); err != nil {
		return err
	}
	if vanished > 0 {
		if _, err := s.conn.ExecContext(ctx, `DELETE FROM retained_commit_links WHERE `+predicate, args...); err != nil {
			return err
		}
	}
	return nil
}
