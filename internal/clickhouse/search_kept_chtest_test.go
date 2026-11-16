//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSearchKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	first, syncer, local := newPushedStore(t)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	filter := db.SearchFilter{Query: "alpha"}
	read := func(s *Store) db.SearchPage {
		t.Helper()
		page, err := s.Search(t.Context(), filter)
		require.NoError(t, err)
		return page
	}
	count := func(s *Store) uint64 {
		t.Helper()
		n, _ := usageQueryCountsOf(t, s, "startsWith(trimLeft(query, ' \\n\\t\\r'), 'WITH msg_grouped AS')")
		return n
	}
	want := read(first)
	require.NotEmpty(t, want.Results)
	require.Positive(t, count(first))
	reopen := func() *Store {
		s := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, s.openActivityReportDisk(syncer.target))
		return s
	}
	s := reopen()
	before := count(s)
	require.Equal(t, want, read(s))
	require.Equal(t, before, count(s))
	parts, err := first.tablePartsFingerprint(t.Context(), []string{"sessions", "messages"})
	require.NoError(t, err)
	execPreparedUsage(t, first, "SYSTEM STOP MERGES sessions", "SYSTEM STOP MERGES messages", "INSERT INTO sessions SELECT * FROM sessions", "INSERT INTO messages SELECT * FROM messages", "SYSTEM START MERGES sessions", "SYSTEM START MERGES messages", "OPTIMIZE TABLE sessions FINAL", "OPTIMIZE TABLE messages FINAL")
	after, err := first.tablePartsFingerprint(t.Context(), []string{"sessions", "messages"})
	require.NoError(t, err)
	require.NotEqual(t, parts, after)
	s = reopen()
	before = count(s)
	require.Equal(t, want, read(s))
	require.Equal(t, before, count(s))
	appendMessage(t, local, fixtureAlphaID, "alpha changed", "2026-01-12T05:00:00Z")
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	s = reopen()
	before = count(s)
	read(s)
	require.Greater(t, count(s), before)
}
