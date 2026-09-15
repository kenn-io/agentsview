//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPGDuplicateSuppressionRequiresLiveCanonical verifies that PG aggregate
// usage suppression only applies when the canonical session is not deleted:
// a soft-deleted canonical's own usage is invisible to aggregates, so
// suppressing its duplicates would under-count. Mirrors the SQLite
// usage-cache fill rule.
func TestPGDuplicateSuppressionRequiresLiveCanonical(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_dup_suppression_live_canonical_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	ctx := context.Background()
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, EnsureSchema(ctx, store.DB(), schema))

	// Twin sessions with membership pointing at goose:live, plus a second
	// twin pair whose canonical goose:dead is soft-deleted.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (id, machine, project, agent, message_count)
		VALUES
			('goose:live', 'm', 'p', 'goose', 3),
			('augure-desktop:live-copy', 'm', 'p', 'augure-desktop', 3),
			('goose:dead', 'm', 'p', 'goose', 3),
			('augure-desktop:dead-copy', 'm', 'p', 'augure-desktop', 3);
		UPDATE sessions SET deleted_at = now()
			WHERE id = 'goose:dead';
		INSERT INTO duplicate_group_members
			(session_id, group_key, role, canonical_id, member_count)
		VALUES
			('goose:live', 'g-live', 'canonical', '', 2),
			('augure-desktop:live-copy', 'g-live', 'duplicate', 'goose:live', 2),
			('goose:dead', 'g-dead', 'canonical', '', 2),
			('augure-desktop:dead-copy', 'g-dead', 'duplicate', 'goose:dead', 2);
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			occurred_at, dedup_key
		) VALUES
			('goose:live', 'session', 'ossington-5', 1, 100,
				'2026-08-10T09:00:00Z', 'k-live'),
			('augure-desktop:live-copy', 'session', 'ossington-5', 1, 50,
				'2026-08-10T09:00:05Z', 'k-live-copy'),
			('goose:dead', 'session', 'ossington-5', 1, 100,
				'2026-08-10T09:00:00Z', 'k-dead'),
			('augure-desktop:dead-copy', 'session', 'ossington-5', 1, 50,
				'2026-08-10T09:00:05Z', 'k-dead-copy');
	`)
	require.NoError(t, err)

	filter := db.UsageFilter{}

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err, "GetDailyUsage")
	// Live pair: the duplicate's 50 tokens are suppressed. Dead pair: the
	// deleted canonical's 100 are invisible anyway, so suppressing its
	// duplicate would drop 50 counted tokens; the predicate must keep them.
	assert.Equal(t, 150, daily.Totals.OutputTokens,
		"suppression applies for the live canonical only")

	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(t, err, "GetUsageSessionCounts")
	// The live duplicate is suppressed; the dead pair's sessions are both
	// invisible (deleted canonical) or kept (live-canonical check keeps the
	// dead-copy counted) — net: only the live canonical and the dead copy.
	assert.Equal(t, 2, counts.Total,
		"session counts suppress the live duplicate, keep the dead copy")
}
