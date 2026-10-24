package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalEmbeddableSourceFiltersMachinesAndPrefixes(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.SetSyncState(t.Context(), "artifact_local_installation_id", "installation"))
	for _, tc := range []struct{ id, machine string }{{"a", "local"}, {"b", "installation"}, {"other~c", "local"}, {"d", "remote"}} {
		insertSession(t, d, tc.id, "project")
		_, err := d.getWriter().ExecContext(t.Context(), "UPDATE sessions SET machine=? WHERE id=?", tc.machine, tc.id)
		require.NoError(t, err)
		insertMessages(t, d, Message{SessionID: tc.id, Ordinal: 0, Role: "user", Content: "content", Timestamp: tsZero})
	}
	collect := func(s EmbeddableSnapshot) {
		var ids []string
		_, err := s.ScanEmbeddableUnits(t.Context(), "", false, 4000, func(u EmbeddableUnit) error { ids = append(ids, u.SessionID); return nil })
		require.NoError(t, err)
		require.Equal(t, []string{"a", "b"}, ids)
		var n int
		require.NoError(t, s.ScanEmbeddableUnitsForSession(t.Context(), "other~c", false, 4000, func(EmbeddableUnit) error { n++; return nil }))
		require.Zero(t, n)
		require.ErrorContains(t, s.ScanEmbeddableUnitsForSession(t.Context(), "", false, 4000, func(EmbeddableUnit) error { n++; return nil }), "session ID is required")
		require.Zero(t, n)
	}
	source := d.LocalEmbeddableSource()
	collect(source)
	require.NoError(t, source.WithEmbeddableSnapshot(t.Context(), func(s EmbeddableSnapshot) error { collect(s); return nil }))
}
