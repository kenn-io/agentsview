package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// ListArchiveWorktreeCandidates returns the machine/path groups for a
// project selected by (display label, project key) across every visible
// session mirrored into this ClickHouse store, with no Activity date range
// or filter scoping. It mirrors internal/db.ListArchiveWorktreeCandidates
// (SQLite): the same session-selection predicate, the same
// BuildProjectIdentityMap-based label/key match, and the same shared
// db.BuildWorktreeCandidates grouping pipeline.
func (s *Store) ListArchiveWorktreeCandidates(
	ctx context.Context,
	request db.ArchiveWorktreeCandidateRequest,
) ([]db.WorktreeReclassificationCandidate, error) {
	if strings.TrimSpace(request.ProjectKey) == "" {
		return nil, errors.New("project_key is required")
	}
	sessions, err := s.archiveWorktreeCandidateSessions(ctx, request.ProjectDateFilter)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]struct{})
	for _, session := range sessions {
		labels[session.project] = struct{}{}
	}
	projects, err := s.BuildProjectIdentityMap(ctx, sortedProjectLabels(labels))
	if err != nil {
		return nil, err
	}
	selectedProjects := db.SelectWorktreeCandidateProjects(
		request, labels, projects,
	)
	if len(selectedProjects) == 0 {
		return []db.WorktreeReclassificationCandidate{}, nil
	}

	selectedIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := selectedProjects[session.project]; !ok {
			continue
		}
		selectedIDs = append(selectedIDs, session.id)
	}
	return s.worktreeCandidatesFromSelection(ctx, selectedIDs, selectedProjects)
}

// archiveCandidateSessionRef is the minimal (id, project) pair the
// archive-wide selection query needs, mirroring internal/db's SQLite type
// of the same shape.
type archiveCandidateSessionRef struct {
	id, project string
}

// archiveWorktreeCandidateSessions returns every visible session (deleted_at
// IS NULL) across every source archive mirrored into this store, matching
// both the project inventory and SQLite candidate selection.
func (s *Store) archiveWorktreeCandidateSessions(
	ctx context.Context,
	filter db.ProjectDateFilter,
) ([]archiveCandidateSessionRef, error) {
	where, args := db.BuildSessionBaseFilterSQL(filter.SessionFilter(), db.ClickHouseQueryDialect())
	rows, err := s.queryContext(ctx, `
		SELECT id, project
		FROM sessions
		WHERE `+where+`
		ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse archive worktree candidate sessions: %w", err)
	}
	defer rows.Close()
	var sessions []archiveCandidateSessionRef
	for rows.Next() {
		var session archiveCandidateSessionRef
		if err := rows.Scan(&session.id, &session.project); err != nil {
			return nil, fmt.Errorf("scanning clickhouse archive worktree candidate session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse archive worktree candidate sessions: %w", err)
	}
	return sessions, nil
}

// worktreeCandidatesFromSelection loads the session and observation rows
// for an already-selected set of session IDs and runs them through the
// shared db.BuildWorktreeCandidates grouping pipeline.
func (s *Store) worktreeCandidatesFromSelection(
	ctx context.Context,
	selectedIDs []string,
	selectedProjects map[string]struct{},
) ([]db.WorktreeReclassificationCandidate, error) {
	sessions, err := s.loadWorktreeCandidateSessions(ctx, selectedIDs)
	if err != nil {
		return nil, err
	}
	observations, err := s.ListProjectIdentityObservations(ctx, sortedProjectLabels(selectedProjects))
	if err != nil {
		return nil, err
	}
	return db.BuildWorktreeCandidates(sessions, observations), nil
}

// loadWorktreeCandidateSessions returns the session id/project/machine/cwd
// plus its most recent source_session_project_identity_snapshots row (the
// mirror's snapshot table is keyed by (source_archive_id,
// source_database_generation, source_session_id), so a ROW_NUMBER dedup
// picks one snapshot per (source_archive_id, session id) the same way the
// DuckDB loader does).
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

func sortedProjectLabels(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
