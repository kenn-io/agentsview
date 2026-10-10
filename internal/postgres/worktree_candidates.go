package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) ListArchiveWorktreeCandidates(ctx context.Context, request db.ArchiveWorktreeCandidateRequest) ([]db.WorktreeReclassificationCandidate, error) {
	return s.catalog().ListArchiveWorktreeCandidates(ctx, request)
}

// Snapshots match the session source database generation.
func (s *Store) loadWorktreeCandidateSessions(
	ctx context.Context, ids []string, filter db.SessionFilter,
) ([]db.WorktreeCandidateSession, error) {
	byID := make(map[string]db.WorktreeCandidateSession, len(ids))
	err := pgQueryChunked(ids, func(chunk []string) error {
		filter.IDs = chunk
		filter.IDsExact = true
		where, args := db.BuildSessionBaseFilterSQL(filter, db.PostgresQueryDialect())
		query := `
			SELECT s.id, s.project, s.machine, s.cwd,
				COALESCE(snap.source_session_id, ''), COALESCE(snap.project, ''),
				COALESCE(snap.machine, ''), COALESCE(snap.root_path, ''),
				COALESCE(snap.worktree_root_path, ''), COALESCE(snap.key_source, '')
			FROM sessions s
			LEFT JOIN source_session_project_identity_snapshots snap
			  ON snap.source_archive_id = s.source_archive_id
			 AND snap.source_database_generation =
			     s.source_database_generation
			 AND snap.source_session_id = s.id
			WHERE s.id IN (SELECT id FROM sessions WHERE ` + where + `)
			ORDER BY s.id`
		rows, err := s.pg.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying pg worktree candidate sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var session db.WorktreeCandidateSession
			var snapshotSessionID string
			if err := rows.Scan(
				&session.ID, &session.Project, &session.Machine, &session.Cwd,
				&snapshotSessionID, &session.Snapshot.Project,
				&session.Snapshot.Machine, &session.Snapshot.RootPath,
				&session.Snapshot.WorktreeRootPath, &session.Snapshot.KeySource,
			); err != nil {
				return fmt.Errorf("scanning pg worktree candidate session: %w", err)
			}
			session.HasSnapshot = snapshotSessionID != ""
			byID[session.ID] = session
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	result := make([]db.WorktreeCandidateSession, 0, len(byID))
	for _, id := range ids {
		if session, ok := byID[id]; ok {
			result = append(result, session)
		}
	}
	return result, nil
}
