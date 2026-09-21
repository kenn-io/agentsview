//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// TestStoreGetAnalyticsSessionShape_AutonomyExcludesSystemUsers
// seeds a session with one real user message and several promoted
// Claude system messages (role='user' + is_system=true), plus four
// assistant tool-use messages. Autonomy = assistant-tool / real-user
// = 4/1 = 4.0, which must land in the "2-5" bucket. If the PG query
// counted promoted system rows as user turns, the ratio would be
// 4/(1+3) = 1.0 and fall in "1-2".
func TestStoreGetAnalyticsSessionShape_AutonomyExcludesSystemUsers(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"autonomy-test-machine", true,
		storage.PusherOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Add(-1 * time.Hour).
		Format(time.RFC3339)
	first := "real user turn"
	sess := db.Session{
		ID:           "autonomy-1",
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &first,
		StartedAt:    &started,
		MessageCount: 8,
	}
	require.NoError(t, local.UpsertSession(t.Context(), sess), "upsert session")

	msgs := []db.Message{
		{
			SessionID: "autonomy-1", Ordinal: 0,
			Role: "user", Content: first,
		},
	}
	// 3 promoted system messages: role=user + is_system=true.
	for i, sub := range []string{
		"continuation", "interrupted", "task_notification",
	} {
		msgs = append(msgs, db.Message{
			SessionID:     "autonomy-1",
			Ordinal:       i + 1,
			Role:          "user",
			IsSystem:      true,
			SourceType:    "system",
			SourceSubtype: sub,
			Content:       "sys",
		})
	}
	// 4 assistant tool-use messages.
	for i := 0; i < 4; i++ {
		msgs = append(msgs, db.Message{
			SessionID:  "autonomy-1",
			Ordinal:    4 + i,
			Role:       "assistant",
			HasToolUse: true,
			Content:    "tool call",
		})
	}
	require.NoError(t, local.InsertMessages(t.Context(), msgs), "insert messages")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	resp, err := store.GetAnalyticsSessionShape(
		ctx, db.AnalyticsFilter{},
	)
	require.NoError(t, err, "GetAnalyticsSessionShape")

	// Promoted system rows must be excluded: ratio = 4/1 = 4.0 → "2-5".
	// Regression case (counts all role='user'): 4/4 = 1.0 → "1-2".
	want := map[string]int{"2-5": 1}
	for label, got := range mapAutonomy(resp.AutonomyDistribution) {
		assert.Equal(t, want[label], got,
			"AutonomyDistribution[%q] (full: %+v)",
			label, resp.AutonomyDistribution)
	}
	require.NotNil(t, resp.AutonomyDistribution)
	assert.Equal(t, 0, bucketCount(resp.AutonomyDistribution, "1-2"),
		"expected zero sessions in '1-2' bucket; got %+v",
		resp.AutonomyDistribution)
	assert.Equal(t, 1, bucketCount(resp.AutonomyDistribution, "2-5"),
		"expected 1 session in '2-5' bucket; got %+v",
		resp.AutonomyDistribution)
}

// mapAutonomy and bucketCount flatten the bucket slice so the
// assertions above read naturally regardless of bucket order.
func mapAutonomy(buckets []db.DistributionBucket) map[string]int {
	m := make(map[string]int, len(buckets))
	for _, b := range buckets {
		m[b.Label] = b.Count
	}
	return m
}

func bucketCount(buckets []db.DistributionBucket, label string) int {
	for _, b := range buckets {
		if b.Label == label {
			return b.Count
		}
	}
	return 0
}
