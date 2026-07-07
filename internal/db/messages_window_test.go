package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowMsgSpec describes one seeded message's role, keyed by ordinal.
type windowMsgSpec struct {
	ordinal int
	role    string
}

// seedWindowMessages seeds a session with 12 messages (ordinals 0..11) with a
// mix of user/assistant/system roles, used across the GetMessagesWindow
// tests. Layout:
//
//	0 user, 1 assistant, 2 user, 3 assistant, 4 system, 5 user,
//	6 assistant, 7 user, 8 assistant, 9 system, 10 user, 11 assistant
func seedWindowMessages(t *testing.T, d *DB, sessionID string) {
	t.Helper()
	insertSession(t, d, sessionID, "proj")
	specs := []windowMsgSpec{
		{0, "user"}, {1, "assistant"}, {2, "user"}, {3, "assistant"},
		{4, "system"}, {5, "user"}, {6, "assistant"}, {7, "user"},
		{8, "assistant"}, {9, "system"}, {10, "user"}, {11, "assistant"},
	}
	msgs := make([]Message, 0, len(specs))
	for _, sp := range specs {
		content := "msg"
		msgs = append(msgs, Message{
			SessionID:     sessionID,
			Ordinal:       sp.ordinal,
			Role:          sp.role,
			Content:       content,
			ContentLength: len(content),
			IsSystem:      sp.role == "system",
		})
	}
	insertMessages(t, d, msgs...)
}

func ordinalsOf(msgs []Message) []int {
	out := make([]int, len(msgs))
	for i, m := range msgs {
		out[i] = m.Ordinal
	}
	return out
}

func TestGetMessagesWindow_AroundMidSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sMid")

	anchor := 6
	msgs, err := d.GetMessagesWindow(ctx, "sMid", MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{4, 5, 6, 7, 8}, ordinalsOf(msgs),
		"unfiltered window should return anchor +/- 2 ordinals ascending")
}

func TestGetMessagesWindow_RoleFilterCountsFilteredMessages(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sRoleCount")

	anchor := 6
	msgs, err := d.GetMessagesWindow(ctx, "sRoleCount", MessageWindow{
		Around: &anchor, Before: 2, After: 2,
		Roles: []string{"user", "assistant"},
	})
	require.NoError(t, err)
	// Ordinal 4 (system) sits between 3 and 5, so the 2 role-filtered
	// messages before the anchor are ordinals 3 and 5, not 4 and 5.
	assert.Equal(t, []int{3, 5, 6, 7, 8}, ordinalsOf(msgs),
		"before/after counts should count role-filtered messages, not raw ordinals")
}

func TestGetMessagesWindow_AnchorIncludedEvenWhenRoleFiltered(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sAnchorFiltered")

	anchor := 4 // role "system", excluded by the role filter
	msgs, err := d.GetMessagesWindow(ctx, "sAnchorFiltered", MessageWindow{
		Around: &anchor, Before: 1, After: 1,
		Roles: []string{"user", "assistant"},
	})
	require.NoError(t, err)
	require.Equal(t, []int{3, 4, 5}, ordinalsOf(msgs),
		"anchor must be included even though its own role is filtered out")
	assert.Equal(t, "system", msgs[1].Role)
}

func TestGetMessagesWindow_AroundOrdinalZeroHasNoBefore(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sFirst")

	anchor := 0
	msgs, err := d.GetMessagesWindow(ctx, "sFirst", MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, ordinalsOf(msgs),
		"no before rows exist above the first ordinal")
}

func TestGetMessagesWindow_AroundLastOrdinalHasNoAfter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sLast")

	anchor := 11
	msgs, err := d.GetMessagesWindow(ctx, "sLast", MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{9, 10, 11}, ordinalsOf(msgs),
		"no after rows exist below the last ordinal")
}

func TestGetMessagesWindow_LinearModeWithRoles(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sLinearRoles")

	msgs, err := d.GetMessagesWindow(ctx, "sLinearRoles", MessageWindow{
		Limit: 100, Asc: true, Roles: []string{"user"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 2, 5, 7, 10}, ordinalsOf(msgs),
		"linear mode should apply the role filter like the around mode")
}

func TestGetMessagesWindow_EmptyRolesEquivalentToGetMessages(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedWindowMessages(t, d, "sEquiv")

	direct, err := d.GetMessages(ctx, "sEquiv", 3, 5, true)
	require.NoError(t, err)

	from := 3
	windowed, err := d.GetMessagesWindow(ctx, "sEquiv", MessageWindow{
		From: &from, Limit: 5, Asc: true,
	})
	require.NoError(t, err)
	assert.Equal(t, direct, windowed,
		"empty Roles should behave identically to GetMessages")
}
