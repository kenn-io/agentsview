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

func TestSearchKeptRejectsChangedParts(t *testing.T) {
	for _, change := range []string{"merge", "write"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			s, syncer, local := newPushedStore(t)
			require.NoError(t, s.openActivityReportDisk(syncer.target))
			if change == "merge" {
				execPreparedUsage(t, s, "SYSTEM STOP MERGES messages", "INSERT INTO messages SELECT * FROM messages")
			}
			filter := db.SearchFilter{Query: "alpha"}
			if change == "write" {
				appendMessage(t, local, fixtureAlphaID, "alpha changed", "2026-01-12T05:00:00Z")
			}
			snapshotCtx, err := s.withPartsSnapshot(t.Context())
			require.NoError(t, err)
			version, ok, err := s.usageRowsVersion(snapshotCtx, []string{"sessions", "messages", "image_text"}, "")
			require.NoError(t, err)
			require.True(t, ok)
			var changeErr error
			s.afterSearch = func() {
				if change == "merge" {
					_, changeErr = s.conn.ExecContext(t.Context(), "SYSTEM START MERGES messages")
					if changeErr == nil {
						_, changeErr = s.conn.ExecContext(t.Context(), "OPTIMIZE TABLE messages FINAL")
					}
				} else {
					_, changeErr = syncer.Push(t.Context(), false, nil)
				}
			}
			page, err := s.Search(t.Context(), filter)
			require.NoError(t, err)
			require.NoError(t, changeErr)
			require.NotEmpty(t, page.Results)
			normalized := filter
			normalized.Limit = db.DefaultSearchLimit
			_, ok = s.searchReads.Get(normalized.Slot(), version)
			require.False(t, ok)
			_, ok = s.usageDisk.Get("search|"+normalized.Slot(), version)
			require.False(t, ok)
			s.afterSearch = nil
			reopened := newTestStore(t, syncer.conn, syncer.target)
			require.NoError(t, reopened.openActivityReportDisk(syncer.target))
			before, _ := usageQueryCountsOf(t, reopened, "startsWith(trimLeft(query, ' \\n\\t\\r'), 'WITH msg_grouped AS')")
			repeat, err := reopened.Search(t.Context(), filter)
			require.NoError(t, err)
			require.NotEmpty(t, repeat.Results)
			after, _ := usageQueryCountsOf(t, reopened, "startsWith(trimLeft(query, ' \\n\\t\\r'), 'WITH msg_grouped AS')")
			require.Greater(t, after, before)
			if change == "merge" {
				execPreparedUsage(t, s, "SYSTEM START MERGES messages")
			}
		})
	}
}
