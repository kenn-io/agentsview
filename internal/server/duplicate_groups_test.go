package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestDuplicateGroupsEndpoints(t *testing.T) {
	te := setup(t)

	// Empty list before any detection has run.
	w := te.get(t, "/api/v1/settings/duplicate-groups")
	assertStatus(t, w, http.StatusOK)
	var groups []db.DuplicateGroupInfo
	decodeInto(t, w, &groups)
	assert.Empty(t, groups)

	// Seed a migration-twin pair: same machine, same started second,
	// same first message. The old-store copy has more messages, so it
	// becomes the canonical.
	first := "I need you to research and create a comprehensive plan."
	started := "2026-08-10T08:00:00Z"
	te.seedSession(t, "goose:old", "project", 5, func(s *db.Session) {
		s.Agent = "goose"
		s.Machine = "local"
		s.StartedAt = &started
		s.FirstMessage = &first
	})
	te.seedSession(t, "augure-desktop:new", "project", 4, func(s *db.Session) {
		s.Agent = "augure-desktop"
		s.Machine = "local"
		s.StartedAt = &started
		s.FirstMessage = &first
	})

	// Rebuild route.
	w = te.post(t, "/api/v1/settings/duplicate-groups/rebuild", `{}`)
	assertStatus(t, w, http.StatusOK)
	var result db.DuplicateGroupsResult
	decodeInto(t, w, &result)
	require.Equal(t, 1, result.Groups)
	require.Equal(t, 2, result.Members)
	require.Equal(t, 2, result.NotifiedIDs)

	// List route shows the group with both members.
	w = te.get(t, "/api/v1/settings/duplicate-groups")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &groups)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Members, 2)
	roles := map[string]string{}
	for _, m := range groups[0].Members {
		roles[m.Role] = m.SessionID
	}
	assert.Equal(t, "goose:old", roles[db.DuplicateRoleCanonical])
	assert.Equal(t, "augure-desktop:new", roles[db.DuplicateRoleDuplicate])

	// A stable second rebuild reports no notifications.
	w = te.post(t, "/api/v1/settings/duplicate-groups/rebuild", `{}`)
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &result)
	assert.Equal(t, 0, result.NotifiedIDs)
}

func TestSessionListCarriesDuplicateIndicators(t *testing.T) {
	te := setup(t)

	first := "I need you to research and create a comprehensive plan."
	started := "2026-08-10T08:00:00Z"
	te.seedSession(t, "goose:old", "project", 5, func(s *db.Session) {
		s.Agent = "goose"
		s.Machine = "local"
		s.StartedAt = &started
		s.FirstMessage = &first
	})
	te.seedSession(t, "augure-desktop:new", "project", 4, func(s *db.Session) {
		s.Agent = "augure-desktop"
		s.Machine = "local"
		s.StartedAt = &started
		s.FirstMessage = &first
	})

	_, err := te.db.RebuildDuplicateGroups(t.Context())
	require.NoError(t, err)

	w := te.get(t, "/api/v1/sessions?limit=50")
	assertStatus(t, w, http.StatusOK)
	var page struct {
		Sessions []db.Session `json:"sessions"`
	}
	decodeInto(t, w, &page)
	require.NotEmpty(t, page.Sessions)

	byID := map[string]db.Session{}
	for _, s := range page.Sessions {
		byID[s.ID] = s
	}
	old, ok := byID["goose:old"]
	require.True(t, ok)
	assert.Equal(t, db.DuplicateRoleCanonical, old.DuplicateRole)
	assert.Equal(t, "", old.DuplicateCanonicalID)
	assert.Equal(t, 2, old.DuplicateMemberCount)

	twin, ok := byID["augure-desktop:new"]
	require.True(t, ok)
	assert.Equal(t, db.DuplicateRoleDuplicate, twin.DuplicateRole)
	assert.Equal(t, "goose:old", twin.DuplicateCanonicalID)
	assert.Equal(t, 2, twin.DuplicateMemberCount)
}

func TestSessionDetailCarriesDuplicateIndicators(t *testing.T) {
	te := setup(t)

	first := "I need you to research and create a comprehensive plan."
	started := "2026-08-10T08:00:00Z"
	te.seedSession(t, "goose:old", "project", 5, func(s *db.Session) {
		s.Agent = "goose"
		s.Machine = "local"
		s.StartedAt = &started
		s.FirstMessage = &first
	})
	te.seedSession(t, "augure-desktop:new", "project", 4, func(s *db.Session) {
		s.Agent = "augure-desktop"
		s.Machine = "local"
		s.StartedAt = &started
		s.FirstMessage = &first
	})

	_, err := te.db.RebuildDuplicateGroups(t.Context())
	require.NoError(t, err)

	w := te.get(t, "/api/v1/sessions/augure-desktop%3Anew")
	assertStatus(t, w, http.StatusOK)
	var session db.Session
	decodeInto(t, w, &session)
	assert.Equal(t, db.DuplicateRoleDuplicate, session.DuplicateRole)
	assert.Equal(t, "goose:old", session.DuplicateCanonicalID)
	assert.Equal(t, 2, session.DuplicateMemberCount)
}
