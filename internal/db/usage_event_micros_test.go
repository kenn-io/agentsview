package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A bounded report finds a usage event by its time even when the stored text
// does not sort by time: an offset timestamp lands on its UTC day, and text
// that does not parse lands on the day its date prefix names.
func TestBoundedUsageFindsOffsetAndUnparsedEventTimes(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{
		ID: "usage-times", Project: "sample", Machine: "local", Agent: "hermes",
		StartedAt: new("2026-07-01T00:00:00Z"),
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "usage-times", []UsageEvent{
		{
			Source: "session", Model: "gpt-5.4", InputTokens: 100,
			OccurredAt: "2026-08-10T23:30:00-05:00", DedupKey: "offset",
		},
		{
			Source: "session", Model: "gpt-5.4", InputTokens: 7,
			OccurredAt: "2026-08-12 09:00:00", DedupKey: "unparsed",
		},
	}))

	for day, want := range map[string]int{
		"2026-08-10": 0, "2026-08-11": 100, "2026-08-12": 7,
	} {
		filter := UsageFilter{From: day, To: day, Timezone: "UTC"}
		got, err := d.GetDailyUsage(ctx, filter)
		require.NoError(t, err, day)
		assert.Equal(t, want, got.Totals.InputTokens, "daily usage on %s", day)
		legacy, err := d.getDailyUsageLegacy(ctx, filter)
		require.NoError(t, err, day)
		assert.Equal(t, want, legacy.Totals.InputTokens, "legacy daily usage on %s", day)
	}
}

// Opening an archive written before usage_events.occurred_at_us fills the
// column from the stored text. The fill changes no recorded usage, so a change
// stream reader sees nothing new.
func TestUsageEventMicrosUpgradeFillsWithoutPublishingUsage(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(ctx, path)
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "hermes"}))
	require.NoError(t, d.InsertMessages(ctx, []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run it", SourceUUID: "user-one"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Done", SourceUUID: "reply-one", Model: "sample-model"},
	}))
	_, err = d.EnsureConversationExportInitialized(ctx)
	require.NoError(t, err)
	ordinal := 1
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "chat", []UsageEvent{
		{
			MessageOrdinal: &ordinal, Source: "session", Model: "gpt-5.4", InputTokens: 1,
			OccurredAt: "2026-08-10T23:30:00-05:00", DedupKey: "offset",
		},
		{
			MessageOrdinal: &ordinal, Source: "session", Model: "gpt-5.4", InputTokens: 2,
			OccurredAt: "2026-08-12 09:00:00", DedupKey: "unparsed",
		},
		{
			MessageOrdinal: &ordinal, Source: "session", Model: "gpt-5.4", InputTokens: 3,
			DedupKey: "blank",
		},
	}))
	head, err := d.ReadChangeStreamHead(ctx)
	require.NoError(t, err)
	for _, statement := range []string{
		"DROP INDEX idx_usage_events_occurred_us",
		"DROP INDEX idx_usage_events_occurred_unparsed",
		"ALTER TABLE usage_events DROP COLUMN occurred_at_us",
		"CREATE INDEX idx_usage_events_occurred ON usage_events(occurred_at)",
	} {
		_, err := d.getWriter().Exec(ctx, statement)
		require.NoError(t, err, statement)
	}
	require.NoError(t, d.Close())

	upgraded, err := OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })

	rows, err := upgraded.getReader().QueryContext(ctx,
		"SELECT dedup_key, occurred_at_us FROM usage_events ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]sql.NullInt64{}
	for rows.Next() {
		var key string
		var micros sql.NullInt64
		require.NoError(t, rows.Scan(&key, &micros))
		got[key] = micros
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]sql.NullInt64{
		"offset":   {Int64: 1786422600000000, Valid: true},
		"unparsed": {},
		"blank":    {},
	}, got)

	var oldIndex bool
	require.NoError(t, upgraded.getReader().QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE name = 'idx_usage_events_occurred')`).Scan(&oldIndex))
	assert.False(t, oldIndex, "the text index is replaced")

	batch, err := upgraded.ReadChangeStream(ctx, head.Head, nil)
	require.NoError(t, err)
	assert.False(t, batch.Reset)
	assert.Empty(t, batch.Messages, "filling occurred_at_us republished a message")
	assert.Empty(t, batch.Sessions, "filling occurred_at_us republished a session")
}

// Copying an orphaned session derives occurred_at_us from the old archive's
// text, so the copy is reachable by the same range seeks as a fresh write.
func TestCopyOrphanedUsageEventsDerivesMicros(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")
	old := testDBAtPath(t, oldPath, "old")
	require.NoError(t, old.UpsertSession(ctx, Session{ID: "gone", Project: "sample", Machine: "local", Agent: "hermes"}))
	require.NoError(t, old.ReplaceSessionUsageEvents(ctx, "gone", []UsageEvent{
		{
			Source: "session", Model: "gpt-5.4", InputTokens: 1,
			OccurredAt: "2026-08-10T23:30:00-05:00", DedupKey: "offset",
		},
		{Source: "session", Model: "gpt-5.4", InputTokens: 2, DedupKey: "blank"},
	}))
	_, err := old.getWriter().Exec(ctx, "UPDATE usage_events SET occurred_at_us = NULL")
	require.NoError(t, err)
	require.NoError(t, old.Close())

	fresh := testDBAtPath(t, filepath.Join(dir, "fresh.db"), "fresh")
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	copied, err := fresh.CopyOrphanedDataFrom(ctx, oldPath)
	require.NoError(t, err)
	require.Equal(t, 1, copied)

	rows, err := fresh.getReader().QueryContext(ctx,
		"SELECT dedup_key, occurred_at_us FROM usage_events WHERE session_id = 'gone'")
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]sql.NullInt64{}
	for rows.Next() {
		var key string
		var micros sql.NullInt64
		require.NoError(t, rows.Scan(&key, &micros))
		got[key] = micros
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]sql.NullInt64{
		"offset": {Int64: 1786422600000000, Valid: true},
		"blank":  {},
	}, got)
}
