//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// seedDuckWindowMessages seeds a session with 12 messages (ordinals 0..11)
// with the same user/assistant/system role layout used by the SQLite
// GetMessagesWindow parity tests (internal/db/messages_window_test.go):
//
//	0 user, 1 assistant, 2 user, 3 assistant, 4 system, 5 user,
//	6 assistant, 7 user, 8 assistant, 9 system, 10 user, 11 assistant
func seedDuckWindowMessages(t *testing.T, local *db.DB, sessionID string) {
	t.Helper()
	s := db.Session{
		ID:           sessionID,
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, local.UpsertSession(s), "seedDuckWindowMessages upsertSession %s", sessionID)
	roles := []string{
		"user", "assistant", "user", "assistant", "system", "user",
		"assistant", "user", "assistant", "system", "user", "assistant",
	}
	msgs := make([]db.Message, 0, len(roles))
	for ordinal, role := range roles {
		content := "msg"
		msgs = append(msgs, db.Message{
			SessionID:     sessionID,
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
			ContentLength: len(content),
			IsSystem:      role == "system",
		})
	}
	require.NoError(t, local.InsertMessages(msgs),
		"seedDuckWindowMessages insertMessages %s", sessionID)
}

// newDuckWindowStore seeds the local SQLite DB via setup, pushes to a fresh
// DuckDB mirror, and returns the read-only Store.
func newDuckWindowStore(t *testing.T, setup func(local *db.DB)) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	setup(local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "Push to DuckDB mirror")
	return NewStoreFromDB(syncer.DB())
}

func duckOrdinalsOf(msgs []db.Message) []int {
	out := make([]int, len(msgs))
	for i, m := range msgs {
		out[i] = m.Ordinal
	}
	return out
}

func TestDuckGetMessagesWindow_AroundMidSession(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sMid")
	})

	anchor := 6
	msgs, err := store.GetMessagesWindow(ctx, "sMid", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{4, 5, 6, 7, 8}, duckOrdinalsOf(msgs),
		"unfiltered window should return anchor +/- 2 ordinals ascending")
}

func TestDuckGetMessagesWindow_RoleFilterCountsFilteredMessages(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sRoleCount")
	})

	anchor := 6
	msgs, err := store.GetMessagesWindow(ctx, "sRoleCount", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
		Roles: []string{"user", "assistant"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{3, 5, 6, 7, 8}, duckOrdinalsOf(msgs),
		"before/after counts should count role-filtered messages, not raw ordinals")
}

func TestDuckGetMessagesWindow_AnchorIncludedEvenWhenRoleFiltered(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sAnchorFiltered")
	})

	anchor := 4 // role "system", excluded by the role filter
	msgs, err := store.GetMessagesWindow(ctx, "sAnchorFiltered", db.MessageWindow{
		Around: &anchor, Before: 1, After: 1,
		Roles: []string{"user", "assistant"},
	})
	require.NoError(t, err)
	require.Equal(t, []int{3, 4, 5}, duckOrdinalsOf(msgs),
		"anchor must be included even though its own role is filtered out")
	assert.Equal(t, "system", msgs[1].Role)
}

func TestDuckGetMessagesWindow_AroundOrdinalZeroHasNoBefore(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sFirst")
	})

	anchor := 0
	msgs, err := store.GetMessagesWindow(ctx, "sFirst", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, duckOrdinalsOf(msgs),
		"no before rows exist above the first ordinal")
}

func TestDuckGetMessagesWindow_AroundLastOrdinalHasNoAfter(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sLast")
	})

	anchor := 11
	msgs, err := store.GetMessagesWindow(ctx, "sLast", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{9, 10, 11}, duckOrdinalsOf(msgs),
		"no after rows exist below the last ordinal")
}

func TestDuckGetMessagesWindow_LinearModeWithRoles(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sLinearRoles")
	})

	msgs, err := store.GetMessagesWindow(ctx, "sLinearRoles", db.MessageWindow{
		Limit: 100, Asc: true, Roles: []string{"user"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 2, 5, 7, 10}, duckOrdinalsOf(msgs),
		"linear mode should apply the role filter like the around mode")
}

func TestDuckGetMessagesWindow_EmptyRolesEquivalentToGetMessages(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sEquiv")
	})

	direct, err := store.GetMessages(ctx, "sEquiv", 3, 5, true)
	require.NoError(t, err)

	from := 3
	windowed, err := store.GetMessagesWindow(ctx, "sEquiv", db.MessageWindow{
		From: &from, Limit: 5, Asc: true,
	})
	require.NoError(t, err)
	assert.Equal(t, direct, windowed,
		"empty Roles should behave identically to GetMessages")
}
