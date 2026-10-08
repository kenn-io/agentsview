package clickhouse

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) ListArchiveWorktreeCandidates(ctx context.Context, request db.ArchiveWorktreeCandidateRequest) ([]db.WorktreeReclassificationCandidate, error) {
	return s.catalog().ListArchiveWorktreeCandidates(ctx, request)
}

// Snapshots use the latest observation for each source archive and session.
func (s *Store) loadWorktreeCandidateSessions(
	ctx context.Context, ids []string,
) ([]db.WorktreeCandidateSession, error) {
	byID := make(map[string]db.WorktreeCandidateSession, len(ids))
	for batch := range idBatches(ids) {
		placeholders, args := inArgs(batch)
		query := `
			WITH ranked_snapshots AS (
				SELECT source_archive_id, source_session_id, project, machine,
					root_path, worktree_root_path, key_source,
					ROW_NUMBER() OVER (
						PARTITION BY source_archive_id, source_session_id
						ORDER BY observed_at DESC, source_database_generation DESC
					) AS rn
				FROM source_session_project_identity_snapshots
				WHERE source_session_id IN (` + placeholders + `)
			)
			SELECT s.id, s.project, s.machine, s.cwd,
				COALESCE(snap.source_session_id, ''), COALESCE(snap.project, ''),
				COALESCE(snap.machine, ''), COALESCE(snap.root_path, ''),
				COALESCE(snap.worktree_root_path, ''), COALESCE(snap.key_source, '')
			FROM sessions s
			LEFT JOIN ranked_snapshots snap
			  ON snap.source_archive_id = s.source_archive_id
			 AND snap.source_session_id = s.id
			 AND snap.rn = 1
			WHERE s.id IN (` + placeholders + `) AND s.deleted_at IS NULL
			ORDER BY s.id`
		queryArgs := make([]any, 0, len(args)*2)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, args...)
		if err := func() error {
			rows, err := s.queryContext(ctx, query, queryArgs...)
			if err != nil {
				return fmt.Errorf("querying clickhouse worktree candidate sessions: %w", err)
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
					return fmt.Errorf("scanning clickhouse worktree candidate session: %w", err)
				}
				session.HasSnapshot = snapshotSessionID != ""
				byID[session.ID] = session
			}
			err = rows.Err()
			if err != nil {
				return fmt.Errorf("iterating clickhouse worktree candidate sessions: %w", err)
			}
			return nil
		}(); err != nil {
			return nil, err
		}
	}
	result := make([]db.WorktreeCandidateSession, 0, len(byID))
	for _, id := range ids {
		if session, ok := byID[id]; ok {
			result = append(result, session)
		}
	}
	return result, nil
}
