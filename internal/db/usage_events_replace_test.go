//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageEventsReplaceKeepsPrefixAndSkipsIdenticalWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.SetSyncState(ctx, "artifact_origin_id", "origin-a"))
	insertSession(t, d, "append", "project-a", func(s *Session) { s.Machine = "local" })
	events := []UsageEvent{{Source: "session", Model: "model-x", MessageOrdinal: new(0), Cost: new(money.Money{}), OutputTokens: 3}}
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "append", events))
	original, err := d.GetUsageEvents(ctx, "append")
	require.NoError(t, err)
	before, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	events = append(events, UsageEvent{Source: "session", Model: "model-y", InputTokens: 7, OutputTokens: 7, OccurredAt: "2026-07-02T10:00:00Z", Cost: new(money.Money{Microdollars: 123})})
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "append", events))
	stored, err := d.GetUsageEvents(ctx, "append")
	require.NoError(t, err)
	require.Len(t, stored, 2)
	assert.Equal(t, original[0], stored[0])
	assert.Greater(t, stored[1].ID, stored[0].ID)
	assert.Equal(t, 7, stored[1].OutputTokens)
	after, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+1, after, "only the appended row advances the journal")
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET local_modified_at = '2000-01-01T00:00:00Z' WHERE id = 'append'; DELETE FROM artifact_export_queue`)
	require.NoError(t, err)
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "append", events))
	same, err := d.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, after, same)
	unchanged, err := d.GetUsageEvents(ctx, "append")
	require.NoError(t, err)
	assert.Equal(t, stored, unchanged)
	ids, err := d.LoadUsageChanges(ctx, after, same)
	require.NoError(t, err)
	assert.Empty(t, ids)
	var modified string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT local_modified_at FROM sessions WHERE id = 'append'`).Scan(&modified))
	assert.Equal(t, "2000-01-01T00:00:00Z", modified)
	var queued int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_export_queue`).Scan(&queued))
	assert.Zero(t, queued)
}

func TestUsageEventsReplaceMiddleEditPreservesTimestampOrder(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "ties", "project-a")
	events := []UsageEvent{
		{Source: "session", OutputTokens: 1, OccurredAt: "2026-07-02T10:00:00Z"},
		{Source: "session", OutputTokens: 2, OccurredAt: "2026-07-01T10:00:00Z"},
		{Source: "session", OutputTokens: 3, OccurredAt: "2026-07-02T10:00:00Z"},
	}
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "ties", events))
	events[0].OutputTokens = 9
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "ties", events))
	stored, err := d.GetUsageEvents(ctx, "ties")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, []int{2, 9, 3}, []int{stored[0].OutputTokens, stored[1].OutputTokens, stored[2].OutputTokens})
}

func TestUsageEventsReplaceStoresControlByteCorrection(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "correction", "project-a")
	events := []UsageEvent{{ProviderID: "openai\x00", Source: "session", OutputTokens: 3}}
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "correction", events))
	events[0].ProviderID = "openai"
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "correction", events))
	stored, err := d.GetUsageEvents(ctx, "correction")
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, "openai", stored[0].ProviderID)
}
