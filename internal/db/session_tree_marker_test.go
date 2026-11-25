package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionTreeMarkerMatchesSessionVersions(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "project")
	insertSession(t, d, "child", "project", func(s *Session) { s.ParentSessionID = new("parent") })
	check := func() string {
		t.Helper()
		entries := make([]SessionTreeEntry, 0, 2)
		for _, id := range []string{"parent", "child"} {
			count, version, ok := d.GetSessionVersion(t.Context(), id)
			require.True(t, ok)
			entries = append(entries, SessionTreeEntry{ID: id, Count: count, Version: version})
		}
		want, err := SessionTreeMarker(entries)
		require.NoError(t, err)
		got, err := d.GetSessionTreeMarker(t.Context(), "parent")
		require.NoError(t, err)
		assert.Equal(t, want, got.Marker)
		assert.Equal(t, entries[0].Count, got.ParentCount)
		assert.Equal(t, entries[0].Version, got.ParentVersion)
		return got.Marker
	}
	before := check()
	_, err := d.SetSessionLabels(t.Context(), "child", []string{"review"})
	require.NoError(t, err)
	assert.NotEqual(t, before, check())
}
