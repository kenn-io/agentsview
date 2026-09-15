package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedDuplicateTwinPair(
	t *testing.T, d *DB, oldID, newID, firstMessage string, oldMsgs, newMsgs int,
) {
	t.Helper()
	// Migration tools preserve the start time, so twins share the exact
	// started_at; the detection key uses its second-precision prefix.
	started := "2026-08-10T08:00:00Z"
	insertSession(t, d, oldID, "project", func(s *Session) {
		s.StartedAt = &started
		s.FirstMessage = &firstMessage
		s.MessageCount = oldMsgs
		s.Agent = "goose"
	})
	insertSession(t, d, newID, "project", func(s *Session) {
		s.StartedAt = &started
		s.FirstMessage = &firstMessage
		s.MessageCount = newMsgs
		s.Agent = "augure-desktop"
	})
}

func duplicateMembership(
	t *testing.T, d *DB, sessionID string,
) (DuplicateGroupMember, bool) {
	t.Helper()
	rows, err := d.getReader().Query(
		`SELECT session_id, group_key, role, canonical_id, member_count
		 FROM duplicate_group_members WHERE session_id = ?`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var m DuplicateGroupMember
		require.NoError(t, rows.Scan(
			&m.SessionID, &m.GroupKey, &m.Role, &m.CanonicalID,
			&m.MemberCount,
		))
		return m, true
	}
	require.NoError(t, rows.Err())
	return DuplicateGroupMember{}, false
}

func TestDuplicateGroupsGroupsTwinsAndElectsCanonical(t *testing.T) {
	d := testDB(t)
	// The measured migration shape: the old-store copy has more messages;
	// it becomes the canonical.
	seedDuplicateTwinPair(t, d, "goose:old", "augure-desktop:new",
		"I need you to research and create a comprehensive plan.", 5, 4)

	// Same first message on a different machine stays ungrouped.
	otherStarted := "2026-08-10T08:00:01Z"
	otherFirst := "I need you to research and create a comprehensive plan."
	insertSession(t, d, "goose:other-machine", "project", func(s *Session) {
		s.StartedAt = &otherStarted
		s.FirstMessage = &otherFirst
		s.Machine = "other-machine"
		s.MessageCount = 3
	})

	// Whitespace normalization groups these two.
	wsStarted := "2026-08-11T10:00:00Z"
	insertSession(t, d, "codex:ws-a", "project", func(s *Session) {
		s.StartedAt = &wsStarted
		s.FirstMessage = &otherFirst
		s.MessageCount = 2
	})
	wsBFirst := "I   need\t you to research and create a comprehensive plan."
	wsBStarted := "2026-08-11T10:00:00Z"
	insertSession(t, d, "codex:ws-b", "project", func(s *Session) {
		s.StartedAt = &wsBStarted
		s.FirstMessage = &wsBFirst
		s.MessageCount = 4
	})

	result, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, result.Groups)
	assert.Equal(t, 4, result.Members)
	assert.Equal(t, 4, result.NotifiedIDs)

	oldMember, ok := duplicateMembership(t, d, "goose:old")
	require.True(t, ok)
	assert.Equal(t, DuplicateRoleCanonical, oldMember.Role)
	assert.Equal(t, 2, oldMember.MemberCount)

	newMember, ok := duplicateMembership(t, d, "augure-desktop:new")
	require.True(t, ok)
	assert.Equal(t, DuplicateRoleDuplicate, newMember.Role)
	assert.Equal(t, "goose:old", newMember.CanonicalID)

	// Whitespace variant: more messages wins the canonical role.
	wsA, ok := duplicateMembership(t, d, "codex:ws-a")
	require.True(t, ok)
	assert.Equal(t, DuplicateRoleDuplicate, wsA.Role)
	wsB, ok := duplicateMembership(t, d, "codex:ws-b")
	require.True(t, ok)
	assert.Equal(t, DuplicateRoleCanonical, wsB.Role)

	groups, err := d.ListDuplicateGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 2)
	// Ordered by member_count desc; both are size 2 here.
	for _, g := range groups {
		assert.Len(t, g.Members, 2)
		for _, m := range g.Members {
			assert.NotEmpty(t, m.Agent)
			assert.NotEmpty(t, m.StartedAt)
		}
	}
}

func TestDuplicateGroupsNegatives(t *testing.T) {
	d := testDB(t)
	first := "Build the thing."
	minutesLater := "2026-08-10T08:01:00Z"
	insertSession(t, d, "a:minutes-apart", "project", func(s *Session) {
		s.StartedAt = &minutesLater
		s.FirstMessage = &first
	})
	otherSecond := "2026-08-10T08:00:30Z"
	insertSession(t, d, "b:minutes-apart", "project", func(s *Session) {
		s.StartedAt = &otherSecond
		s.FirstMessage = &first
	})

	// Sessions with no first message are excluded entirely.
	empty := ""
	insertSession(t, d, "a:no-message", "project", func(s *Session) {
		s.StartedAt = &otherSecond
		s.FirstMessage = &empty
	})

	// Identical sessions on different machines do not group.
	otherMachine := "2026-08-10T08:00:00Z"
	insertSession(t, d, "a:solo", "project", func(s *Session) {
		s.StartedAt = &otherMachine
		s.FirstMessage = &first
		s.Machine = "machine-b"
	})

	result, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, result.Groups)
	assert.Equal(t, 0, result.Members)

	_, ok := duplicateMembership(t, d, "a:minutes-apart")
	assert.False(t, ok)
	_, ok = duplicateMembership(t, d, "a:no-message")
	assert.False(t, ok)
}

func TestDuplicateGroupsSecondPassIsStable(t *testing.T) {
	d := testDB(t)
	seedDuplicateTwinPair(t, d, "goose:old", "augure-desktop:new",
		"I need you to research and create a comprehensive plan.", 5, 4)

	first, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, first.Groups)
	assert.Equal(t, 2, first.NotifiedIDs)

	second, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, second.Groups)
	assert.Equal(t, 0, second.NotifiedIDs,
		"unchanged membership must not re-notify the usage cache")
}

func TestDuplicateGroupsMachineRelabelChangesGroupKeys(t *testing.T) {
	d := testDB(t)
	seedDuplicateTwinPair(t, d, "goose:old", "augure-desktop:new",
		"I need you to research and create a comprehensive plan.", 5, 4)

	first, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, first.NotifiedIDs)
	before, ok := duplicateMembership(t, d, "goose:old")
	require.True(t, ok)

	// Relabeling the machine (installation identity changes rewrite
	// sessions.machine wholesale) rewrites every group key while leaving
	// roles, canonical IDs, and member counts untouched. The membership rows
	// still changed, so mirror push windows must re-select them.
	_, err = d.getWriter().Exec(
		`UPDATE sessions SET machine = 'machine-b'
		 WHERE id IN ('goose:old', 'augure-desktop:new')`)
	require.NoError(t, err)

	second, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, second.NotifiedIDs,
		"a group-key-only change must count as changed membership")

	after, ok := duplicateMembership(t, d, "goose:old")
	require.True(t, ok)
	assert.NotEqual(t, before.GroupKey, after.GroupKey,
		"the relabeled machine must produce a new group key")
	assert.Equal(t, before.Role, after.Role)
	assert.Equal(t, before.CanonicalID, after.CanonicalID)
	assert.Equal(t, before.MemberCount, after.MemberCount)
}

func TestDuplicateGroupsBootstrapBackfillsOnce(t *testing.T) {
	d := testDB(t)
	seedDuplicateTwinPair(t, d, "goose:old", "augure-desktop:new",
		"I need you to research and create a comprehensive plan.", 5, 4)

	// A migrated archive has never had a rebuild; the bootstrap runs once
	// and populates membership.
	ran, err := d.EnsureDuplicateGroupsBootstrapped(context.Background())
	require.NoError(t, err)
	assert.True(t, ran)
	member, ok := duplicateMembership(t, d, "goose:old")
	require.True(t, ok, "bootstrap must populate membership")
	assert.Equal(t, DuplicateRoleCanonical, member.Role)

	// The marker makes every later call a no-op, including after a second
	// rebuild keeps membership fresh through the normal paths.
	ran, err = d.EnsureDuplicateGroupsBootstrapped(context.Background())
	require.NoError(t, err)
	assert.False(t, ran)
	_, ok = duplicateMembership(t, d, "goose:old")
	assert.True(t, ok, "no-op bootstrap must not clear membership")

	result, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Groups)

	ran, err = d.EnsureDuplicateGroupsBootstrapped(context.Background())
	require.NoError(t, err)
	assert.False(t, ran, "a rebuilt archive never bootstraps again")
}
