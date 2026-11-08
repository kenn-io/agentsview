package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostScopedSessionIDs(t *testing.T) {
	const raw = "aaaa_1111"
	d := testDB(t)
	for _, id := range []string{
		raw,                       // bare local copy
		"hostA~" + raw,            // one host prefix
		"hostB~" + raw,            // another host
		"hostC~" + raw,            // soft-deleted below
		"hostA~hostB~" + raw,      // nested host prefix
		"codex:" + raw,            // agent prefix only
		"hostA~xaaaa_1111",        // longer raw ID with the same suffix
		"hostA~aaaaX1111",         // '_' must not act as a wildcard
		"hostA~" + raw + "-child", // raw ID is only a prefix
	} {
		insertSession(t, d, id, "proj")
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "hostC~"+raw))

	ids, err := d.HostScopedSessionIDs(t.Context(), raw)
	require.NoError(t, err)
	assert.Equal(t, []string{raw, "hostA~" + raw, "hostB~" + raw}, ids)

	for _, input := range []string{"", "hostA~" + raw} {
		ids, err := d.HostScopedSessionIDs(t.Context(), input)
		require.NoError(t, err)
		assert.Empty(t, ids, "input %q", input)
	}
}

func TestSessionParentLookups(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "hostB~parent", "proj")
	insertSession(t, d, "hostA~child", "proj", func(s *Session) {
		s.ParentSessionID = new("hostB~parent")
	})

	parent, parserParent, err := d.SessionParentIDs(t.Context(), "hostA~child")
	require.NoError(t, err)
	assert.Equal(t, "hostB~parent", parent)
	assert.Equal(t, "hostB~parent", parserParent)

	parent, parserParent, err = d.SessionParentIDs(t.Context(), "missing")
	require.NoError(t, err)
	assert.Empty(t, parent)
	assert.Empty(t, parserParent)

	found, err := d.LiveSessionExists(t.Context(), "hostB~parent")
	require.NoError(t, err)
	assert.True(t, found)
	require.NoError(t, d.SoftDeleteSession(t.Context(), "hostB~parent"))
	found, err = d.LiveSessionExists(t.Context(), "hostB~parent")
	require.NoError(t, err)
	assert.False(t, found, "soft-deleted sessions are not live")
}
