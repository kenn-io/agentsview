package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode"
)

// Duplicate roles stored in duplicate_group_members.role.
const (
	DuplicateRoleCanonical = "canonical"
	DuplicateRoleDuplicate = "duplicate"
)

// DuplicateGroupsResult summarizes one detection run.
type DuplicateGroupsResult struct {
	Groups      int   `json:"groups"`
	Members     int   `json:"members"`
	NotifiedIDs int   `json:"notified_ids"`
	GroupSizes  []int `json:"group_sizes,omitempty"`
}

// DuplicateGroupMember is one session's membership in a duplicate group.
type DuplicateGroupMember struct {
	SessionID   string `json:"session_id"`
	GroupKey    string `json:"group_key"`
	Role        string `json:"role"`
	CanonicalID string `json:"canonical_id"`
	MemberCount int    `json:"member_count"`
}

// duplicateCandidate is one session row considered for grouping.
type duplicateCandidate struct {
	id           string
	startedAt    string
	firstMessage string
	messageCount int
}

// duplicateGroupKey builds the group identity for one candidate:
// sha256(machine + NUL + started_at second + NUL + normalized first
// message). Migration tools preserve the start time and the opening prompt,
// so this key groups copies of the same interaction across stores while
// unrelated sessions (different machines, minutes-apart starts, different
// prompts) stay apart.
func duplicateGroupKey(
	machine string, startedAt string, firstMessage string,
) string {
	normalized := normalizeDuplicateFirstMessage(firstMessage)
	sum := sha256.Sum256([]byte(
		machine + "\x00" + truncateRunes(startedAt, 19) + "\x00" + normalized,
	))
	return hex.EncodeToString(sum[:])
}

// normalizeDuplicateFirstMessage collapses whitespace runs to single spaces,
// trims, and truncates to 300 runes. Rune-safe, not byte-safe.
func normalizeDuplicateFirstMessage(message string) string {
	var b strings.Builder
	b.Grow(len(message))
	prevSpace := false
	for _, r := range message {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return truncateRunes(strings.TrimSpace(b.String()), 300)
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// RebuildDuplicateGroups recomputes the duplicate_group_members table from
// the visible sessions and notifies the usage cache for every session whose
// membership changed. The table is derived state: it is rebuilt in full, and
// nothing here deletes sessions or usage rows.
func (db *DB) RebuildDuplicateGroups(
	ctx context.Context,
) (DuplicateGroupsResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return DuplicateGroupsResult{}, fmt.Errorf(
			"beginning duplicate group rebuild: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the previous membership first so changed memberships can be
	// diffed after the rewrite.
	prevMembers, err := loadDuplicateMembersTx(ctx, tx)
	if err != nil {
		return DuplicateGroupsResult{}, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, machine, COALESCE(started_at, ''),
			COALESCE(first_message, ''), COALESCE(message_count, 0)
		FROM sessions
		WHERE deleted_at IS NULL
			AND first_message IS NOT NULL AND first_message <> ''
			AND started_at IS NOT NULL AND started_at <> ''`)
	if err != nil {
		return DuplicateGroupsResult{}, fmt.Errorf(
			"querying duplicate group candidates: %w", err,
		)
	}
	groups := make(map[string][]duplicateCandidate)
	for rows.Next() {
		var (
			c       duplicateCandidate
			machine string
		)
		if err := rows.Scan(
			&c.id, &machine, &c.startedAt, &c.firstMessage, &c.messageCount,
		); err != nil {
			rows.Close()
			return DuplicateGroupsResult{}, fmt.Errorf(
				"scanning duplicate group candidate: %w", err,
			)
		}
		key := duplicateGroupKey(machine, c.startedAt, c.firstMessage)
		groups[key] = append(groups[key], c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DuplicateGroupsResult{}, fmt.Errorf(
			"iterating duplicate group candidates: %w", err,
		)
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM duplicate_group_members`); err != nil {
		return DuplicateGroupsResult{}, fmt.Errorf(
			"clearing duplicate group members: %w", err,
		)
	}

	result := DuplicateGroupsResult{GroupSizes: []int{}}
	changed := map[string]bool{}
	newMemberIDs := map[string]bool{}
	for key, members := range groups {
		_ = key
		if len(members) < 2 {
			continue
		}
		// Canonical: most messages; tie -> earliest started_at;
		// tie -> lexicographically smallest ID.
		canonical := members[0]
		for _, m := range members[1:] {
			if betterCanonical(m, canonical) {
				canonical = m
			}
		}
		count := len(members)
		result.Groups++
		result.Members += count
		result.GroupSizes = append(result.GroupSizes, count)
		for _, m := range members {
			role := DuplicateRoleDuplicate
			canonicalID := canonical.id
			if m.id == canonical.id {
				role = DuplicateRoleCanonical
				canonicalID = ""
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO duplicate_group_members
					(session_id, group_key, role, canonical_id, member_count)
				VALUES (?, ?, ?, ?, ?)`,
				m.id, key, role, canonicalID, count,
			); err != nil {
				return DuplicateGroupsResult{}, fmt.Errorf(
					"inserting duplicate group member %s: %w", m.id, err,
				)
			}
			newMemberIDs[m.id] = true
			prev, existed := prevMembers[m.id]
			if !existed || prev.Role != role ||
				prev.CanonicalID != canonicalID || prev.MemberCount != count {
				changed[m.id] = true
			}
		}
	}
	// Sessions that left membership entirely also changed.
	for id := range prevMembers {
		if !newMemberIDs[id] {
			changed[id] = true
		}
	}
	// Bump local_modified_at for every session whose membership changed so
	// mirror push windows re-select the row (the worktree reclassification
	// and agent-remap rewrites use the same signal), letting the push carry
	// the new membership into the PG/duckdb mirror tables.
	for id := range changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions
				SET local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
				WHERE id = ?`, id,
		); err != nil {
			return DuplicateGroupsResult{}, fmt.Errorf(
				"bumping local_modified_at for %s: %w", id, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return DuplicateGroupsResult{}, fmt.Errorf(
			"committing duplicate group rebuild: %w", err,
		)
	}

	if len(changed) > 0 {
		ids := make([]string, 0, len(changed))
		for id := range changed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		db.notifyUsageSessions(ids)
		result.NotifiedIDs = len(ids)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(result.GroupSizes)))
	return result, nil
}

// betterCanonical reports whether candidate a beats b for the canonical
// role: more messages, then earlier start, then smaller ID.
func betterCanonical(a duplicateCandidate, b duplicateCandidate) bool {
	if a.messageCount != b.messageCount {
		return a.messageCount > b.messageCount
	}
	if a.startedAt != b.startedAt {
		return a.startedAt < b.startedAt
	}
	return a.id < b.id
}

func loadDuplicateMembersTx(
	ctx context.Context, tx *sql.Tx,
) (map[string]DuplicateGroupMember, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, group_key, role, canonical_id, member_count
		FROM duplicate_group_members`)
	if err != nil {
		return nil, fmt.Errorf("loading duplicate members: %w", err)
	}
	defer rows.Close()
	out := make(map[string]DuplicateGroupMember)
	for rows.Next() {
		var m DuplicateGroupMember
		if err := rows.Scan(
			&m.SessionID, &m.GroupKey, &m.Role, &m.CanonicalID,
			&m.MemberCount,
		); err != nil {
			return nil, fmt.Errorf("scanning duplicate member: %w", err)
		}
		out[m.SessionID] = m
	}
	return out, rows.Err()
}

// decorateSessionsWithDuplicateRoles fills the duplicate-group indicator
// fields on the given sessions in one lookup. Sessions without membership
// are left at zero values, which the JSON transport omits.
func (db *DB) decorateSessionsWithDuplicateRoles(sessions []Session) {
	if len(sessions) == 0 {
		return
	}
	ctx := context.Background()
	placeholders := strings.Repeat("?,", len(sessions))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(sessions))
	byID := make(map[string][]*Session, len(sessions))
	for i := range sessions {
		args[i] = sessions[i].ID
		byID[sessions[i].ID] = append(byID[sessions[i].ID], &sessions[i])
	}
	query := `
		SELECT session_id, group_key, role, canonical_id, member_count
		FROM duplicate_group_members
		WHERE session_id IN (` + placeholders + `)`
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("warning: duplicate indicator lookup failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			sessionID string
			m         DuplicateGroupMember
		)
		if err := rows.Scan(
			&sessionID, &m.GroupKey, &m.Role, &m.CanonicalID, &m.MemberCount,
		); err != nil {
			log.Printf("warning: duplicate indicator scan failed: %v", err)
			return
		}
		for _, s := range byID[sessionID] {
			s.DuplicateRole = m.Role
			s.DuplicateCanonicalID = m.CanonicalID
			s.DuplicateMemberCount = m.MemberCount
			s.DuplicateGroupKey = m.GroupKey
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("warning: duplicate indicator iteration failed: %v", err)
	}
}

// decorateSidebarIndexWithDuplicateRoles fills the duplicate indicator
// fields on sidebar index rows in one lookup. Rows without membership are
// left at zero values, which the JSON transport omits.
func (db *DB) decorateSidebarIndexWithDuplicateRoles(
	rows []SidebarSessionIndexRow,
) {
	if len(rows) == 0 {
		return
	}
	ctx := context.Background()
	placeholders := strings.Repeat("?,", len(rows))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(rows))
	byID := make(map[string][]*SidebarSessionIndexRow, len(rows))
	for i := range rows {
		args[i] = rows[i].ID
		byID[rows[i].ID] = append(byID[rows[i].ID], &rows[i])
	}
	query := `
		SELECT session_id, role, member_count
		FROM duplicate_group_members
		WHERE session_id IN (` + placeholders + `)`
	dbRows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("warning: sidebar duplicate lookup failed: %v", err)
		return
	}
	defer dbRows.Close()
	for dbRows.Next() {
		var (
			sessionID string
			role      string
			count     int
		)
		if err := dbRows.Scan(&sessionID, &role, &count); err != nil {
			log.Printf("warning: sidebar duplicate scan failed: %v", err)
			return
		}
		for _, row := range byID[sessionID] {
			row.DuplicateRole = role
			row.DuplicateMemberCount = count
		}
	}
	if err := dbRows.Err(); err != nil {
		log.Printf("warning: sidebar duplicate iteration failed: %v", err)
	}
}

// DuplicateGroupInfo is one duplicate group with its member rows.
type DuplicateGroupInfo struct {
	GroupKey string                  `json:"group_key"`
	Members  []DuplicateGroupMemberX `json:"members"`
}

// DuplicateGroupMemberX is one member row joined with its session summary.
type DuplicateGroupMemberX struct {
	DuplicateGroupMember
	Agent        string `json:"agent"`
	StartedAt    string `json:"started_at"`
	MessageCount int    `json:"message_count"`
}

// ListDuplicateGroups reads the membership table joined with session rows
// and returns every stored duplicate group with its members, largest first.
func (db *DB) ListDuplicateGroups(
	ctx context.Context,
) ([]DuplicateGroupInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT m.session_id, m.group_key, m.role, m.canonical_id,
			m.member_count, COALESCE(s.agent, ''),
			COALESCE(s.started_at, ''), COALESCE(s.message_count, 0)
		FROM duplicate_group_members m
		JOIN sessions s ON s.id = m.session_id
		ORDER BY m.member_count DESC, s.started_at DESC, m.session_id`)
	if err != nil {
		return nil, fmt.Errorf("listing duplicate groups: %w", err)
	}
	defer rows.Close()
	byGroup := make(map[string]*DuplicateGroupInfo)
	var order []string
	for rows.Next() {
		var (
			m     DuplicateGroupMemberX
			group string
		)
		if err := rows.Scan(
			&m.SessionID, &group, &m.Role, &m.CanonicalID, &m.MemberCount,
			&m.Agent, &m.StartedAt, &m.MessageCount,
		); err != nil {
			return nil, fmt.Errorf("scanning duplicate group: %w", err)
		}
		m.GroupKey = group
		info, ok := byGroup[group]
		if !ok {
			info = &DuplicateGroupInfo{GroupKey: group}
			byGroup[group] = info
			order = append(order, group)
		}
		info.Members = append(info.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duplicate groups: %w", err)
	}
	out := make([]DuplicateGroupInfo, 0, len(order))
	for _, group := range order {
		out = append(out, *byGroup[group])
	}
	return out, nil
}

// duplicateSuppressionForSession reports whether the session's usage facts
// should lose token eligibility: only when the session is a duplicate member
// AND its canonical carries at least one token-bearing usage event. When the
// canonical has no usage (the migration-shape case), the duplicate's usage
// stays counted so nothing is lost.
func (db *DB) duplicateSuppressionForSession(
	ctx context.Context, sessionID string,
) (bool, error) {
	var (
		role        string
		canonicalID string
	)
	err := db.getReader().QueryRowContext(ctx, `
		SELECT role, canonical_id FROM duplicate_group_members
		WHERE session_id = ?`, sessionID,
	).Scan(&role, &canonicalID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(
			"reading duplicate membership for %s: %w", sessionID, err,
		)
	}
	if role != DuplicateRoleDuplicate || canonicalID == "" {
		return false, nil
	}
	var hasUsage bool
	err = db.getReader().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM usage_events
			WHERE session_id = ? AND model <> '')`, canonicalID,
	).Scan(&hasUsage)
	if err != nil {
		return false, fmt.Errorf(
			"reading canonical usage presence for %s: %w", canonicalID, err,
		)
	}
	return hasUsage, nil
}

// notifyUsageSessionsWithDuplicates extends a mutation notification with the
// duplicate siblings of every notified canonical member. A canonical gaining
// usage flips its duplicates' suppression verdict, so the cache must refill
// them too. Over-notifying is safe: refills are versioned.
func (db *DB) notifyUsageSessionsWithDuplicates(sessionIDs []string) {
	if db.usageCache == nil || len(sessionIDs) == 0 {
		return
	}
	extra, err := db.duplicateSiblingsOf(sessionIDs)
	if err != nil {
		db.notifyUsageSessions(sessionIDs)
		return
	}
	if len(extra) > 0 {
		combined := make([]string, 0, len(sessionIDs)+len(extra))
		combined = append(combined, sessionIDs...)
		combined = append(combined, extra...)
		sessionIDs = combined
	}
	db.notifyUsageSessions(sessionIDs)
}

// duplicateSiblingsOf returns the duplicate-role members whose canonical is
// in sessionIDs.
func (db *DB) duplicateSiblingsOf(sessionIDs []string) ([]string, error) {
	placeholders := strings.Repeat("?,", len(sessionIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	query := `
		SELECT session_id FROM duplicate_group_members
		WHERE role = 'duplicate'
			AND canonical_id IN (` + placeholders + `)`
	rows, err := db.getReader().QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duplicate siblings: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning duplicate sibling: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
