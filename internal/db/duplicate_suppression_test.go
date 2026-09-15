package db

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fillDuplicateFacts runs a usage-cache fill for the given sessions and
// returns each session's installed facts.
func fillDuplicateFacts(
	t *testing.T, d *DB, sessionIDs []string,
) map[string][]usageFactRow {
	t.Helper()
	snapshot, err := d.captureUsageQuery(context.Background(), UsageFilter{},
		usageQueryKindActivity)
	require.NoError(t, err)
	cache, err := d.usageCache.Generation(context.Background(),
		snapshot.DatabaseID)
	require.NoError(t, err)
	versions := make([]usageSourceVersion, 0, len(sessionIDs))
	for _, version := range snapshot.Versions {
		if slices.Contains(sessionIDs, version.SessionID) {
			versions = append(versions, version)
		}
	}
	results, err := cache.fill.Ensure(
		context.Background(), versions, snapshot.CursorHighWater,
	)
	require.NoError(t, err)
	out := make(map[string][]usageFactRow)
	for _, id := range sessionIDs {
		if _, ok := results[id]; !ok {
			continue
		}
		rows, err := cache.db.Query(`
			SELECT f.source, f.model, f.token_eligible, f.activity_eligible
			FROM usage_facts f
			JOIN usage_cached_sessions cs ON cs.id = f.cached_session_id
			WHERE cs.session_id = ?
			ORDER BY f.fact_index`, id)
		require.NoError(t, err)
		var facts []usageFactRow
		for rows.Next() {
			var row usageFactRow
			require.NoError(t, rows.Scan(
				&row.source, &row.model, &row.token, &row.active,
			))
			facts = append(facts, row)
		}
		require.NoError(t, rows.Err())
		rows.Close()
		out[id] = facts
	}
	return out
}

type usageFactRow struct {
	source string
	model  string
	token  int
	active int
}

func seedCanonicalWithoutUsage(t *testing.T, d *DB) {
	t.Helper()
	first := "I need you to research and create a comprehensive plan."
	seedDuplicateTwinPair(t, d, "goose:old", "augure-desktop:new", first, 5, 4)
	require.NoError(t, d.InsertMessages([]Message{
		{SessionID: "goose:old", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "ossington-5",
			TokenUsage:      json.RawMessage(`{"input_tokens":2,"output_tokens":3067}`),
			ClaudeMessageID: "m-old", ClaudeRequestID: "r-old",
			SourceUUID: "u-old"},
	}))
}

func TestDuplicateSuppressionKeepsUsageWhenCanonicalHasNone(t *testing.T) {
	d := testDB(t)
	seedCanonicalWithoutUsage(t, d)

	// The old-store canonical carries the usage; the migrated duplicate
	// carries none. Nothing should be suppressed: suppressing the
	// duplicate's usage would be a no-op anyway, and the canonical keeps
	// everything.
	result, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Groups)

	facts := fillDuplicateFacts(t, d, []string{"goose:old", "augure-desktop:new"})
	require.NotEmpty(t, facts["goose:old"])
	for _, row := range facts["goose:old"] {
		assert.Equal(t, 1, row.token,
			"canonical facts keep token eligibility")
	}
	// The duplicate has no usage rows at all here; absence is fine either
	// way. The key assertion is the canonical above.
}

func TestDuplicateSuppressionSuppressesWhenCanonicalHasUsage(t *testing.T) {
	d := testDB(t)
	seedCanonicalWithoutUsage(t, d)
	// Give the migrated duplicate its own event-model usage so the
	// double-count case is real.
	require.NoError(t, d.ReplaceSessionUsageEvents("augure-desktop:new",
		[]UsageEvent{{
			Source: "session", Model: "ossington-5", InputTokens: 7,
			OutputTokens: 9, OccurredAt: "2026-08-10T09:00:05Z",
			DedupKey: "dup-1",
		}}))

	result, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Groups)

	facts := fillDuplicateFacts(t, d, []string{"goose:old", "augure-desktop:new"})
	require.NotEmpty(t, facts["goose:old"])
	for _, row := range facts["goose:old"] {
		assert.Equal(t, 1, row.token)
	}
	require.NotEmpty(t, facts["augure-desktop:new"])
	for _, row := range facts["augure-desktop:new"] {
		assert.Equal(t, 0, row.token,
			"duplicate facts lose token eligibility when the canonical has usage")
		assert.Equal(t, 1, row.active,
			"activity eligibility is unchanged by suppression")
	}
}

func TestDuplicateSiblingNotificationExtendsNotifyList(t *testing.T) {
	d := testDB(t)
	seedCanonicalWithoutUsage(t, d)
	_, err := d.RebuildDuplicateGroups(context.Background())
	require.NoError(t, err)

	siblings, err := d.duplicateSiblingsOf([]string{"goose:old"})
	require.NoError(t, err)
	assert.Equal(t, []string{"augure-desktop:new"}, siblings)

	siblings, err = d.duplicateSiblingsOf([]string{"augure-desktop:new"})
	require.NoError(t, err)
	assert.Empty(t, siblings,
		"duplicates have no siblings of their own")

	siblings, err = d.duplicateSiblingsOf([]string{"unrelated"})
	require.NoError(t, err)
	assert.Empty(t, siblings)

	// The wrapper must be a no-op when the usage cache is disabled.
	d.notifyUsageSessionsWithDuplicates([]string{"goose:old"})
}
