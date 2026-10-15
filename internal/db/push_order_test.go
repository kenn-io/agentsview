package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortSessionsNewestFirst(t *testing.T) {
	ptr := func(s string) *string { return &s }
	sessions := []Session{
		{ID: "created-only", CreatedAt: "2026-03-01T00:00:00Z"},
		{ID: "ended", StartedAt: ptr("2026-01-01T00:00:00Z"), EndedAt: ptr("2026-05-01T00:00:00Z")},
		{ID: "unparseable", CreatedAt: "not a time"},
		{ID: "started-b", StartedAt: ptr("2026-04-01T00:00:00Z"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "started-a", StartedAt: ptr("2026-04-01T00:00:00Z")},
	}
	SortSessionsNewestFirst(sessions)
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	assert.Equal(t, []string{"ended", "started-a", "started-b", "created-only", "unparseable"}, ids)
}

// A Codex revert page goes out right after its thread's row, even when the
// page is newer than the thread, because mirrors read the thread's row to
// decide whether the page is trashed.
func TestSortSessionsNewestFirstPutsCodexThreadBeforeItsPages(t *testing.T) {
	ptr := func(s string) *string { return &s }
	const (
		thread = "codex:11111111-1111-1111-1111-111111111111"
		page   = thread + "_22222222-2222-2222-2222-222222222222"
	)
	sessions := []Session{
		{ID: "middle", Agent: "claude", EndedAt: ptr("2026-02-01T00:00:00Z")},
		{ID: page, Agent: "codex", EndedAt: ptr("2026-03-01T00:00:00Z")},
		{ID: "oldest", Agent: "claude", EndedAt: ptr("2025-12-01T00:00:00Z")},
		{ID: thread, Agent: "codex", EndedAt: ptr("2026-01-01T00:00:00Z")},
	}
	SortSessionsNewestFirst(sessions)
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	assert.Equal(t, []string{thread, page, "middle", "oldest"}, ids)
}
