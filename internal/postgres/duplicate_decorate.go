package postgres

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// decorateSessionsWithDuplicateRoles fills the duplicate-group indicator
// fields on session reads from the mirrored duplicate_group_members table,
// matching the SQLite read path (decorateSessionsWithDuplicateRoles in
// internal/db/duplicate_groups.go). One batched lookup covers the whole
// page; sessions without membership are left at zero values, which the
// JSON transport omits. A failing lookup degrades to missing badges: the
// decoration is presentation metadata on the read path, never a billing
// input, so it must not fail the underlying read.
func (s *Store) decorateSessionsWithDuplicateRoles(
	ctx context.Context, sessions []db.Session,
) {
	if len(sessions) == 0 {
		return
	}
	ids := make([]string, 0, len(sessions))
	seen := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		if !seen[sess.ID] {
			seen[sess.ID] = true
			ids = append(ids, sess.ID)
		}
	}
	members := s.pgDuplicateMembersByID(ctx, ids)
	for i := range sessions {
		if m, ok := members[sessions[i].ID]; ok {
			sessions[i].DuplicateRole = m.Role
			sessions[i].DuplicateCanonicalID = m.CanonicalID
			sessions[i].DuplicateMemberCount = m.MemberCount
			sessions[i].DuplicateGroupKey = m.GroupKey
		}
	}
}

// decorateSidebarIndexWithDuplicateRoles fills the duplicate indicator
// fields on sidebar index rows in one lookup. Rows without membership are
// left at zero values, which the JSON transport omits.
func (s *Store) decorateSidebarIndexWithDuplicateRoles(
	ctx context.Context, rows []db.SidebarSessionIndexRow,
) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !seen[row.ID] {
			seen[row.ID] = true
			ids = append(ids, row.ID)
		}
	}
	members := s.pgDuplicateMembersByID(ctx, ids)
	for i := range rows {
		if m, ok := members[rows[i].ID]; ok {
			rows[i].DuplicateRole = m.Role
			rows[i].DuplicateMemberCount = m.MemberCount
		}
	}
}

// pgDuplicateMembersByID loads membership rows for the given session IDs
// in one query. Unknown IDs are simply absent from the result; a failed
// query returns an empty map so decoration can never fail the read.
func (s *Store) pgDuplicateMembersByID(
	ctx context.Context, ids []string,
) map[string]db.DuplicateGroupMember {
	out := make(map[string]db.DuplicateGroupMember, len(ids))
	if len(ids) == 0 {
		return out
	}
	pb := &paramBuilder{}
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, pb.add(id))
	}
	rows, err := s.pg.QueryContext(ctx,
		`SELECT session_id, group_key, role, canonical_id, member_count
		 FROM duplicate_group_members
		 WHERE session_id IN (`+strings.Join(placeholders, ",")+`)`,
		pb.args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m db.DuplicateGroupMember
		if err := rows.Scan(
			&m.SessionID, &m.GroupKey, &m.Role,
			&m.CanonicalID, &m.MemberCount,
		); err != nil {
			return out
		}
		out[m.SessionID] = m
	}
	return out
}
