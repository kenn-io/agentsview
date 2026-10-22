//go:build pgtest

package postgres

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPushSuffixPublishesUsageAfterSnapshotRace(t *testing.T) {
	syncer, local, messages := suffixFixture(t, 5)
	ctx := t.Context()
	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "suffix", []db.UsageEvent{{SessionID: "suffix", Source: "provider", Model: "sample", InputTokens: 22, OccurredAt: "2026-10-01T10:00:00Z", DedupKey: "usage"}}))
	grown := append(append([]db.Message(nil), messages...), db.Message{SessionID: "suffix", Ordinal: 5, Role: "user", Content: "append"})
	require.NoError(t, local.ReplaceSessionMessages(ctx, "suffix", grown))
	session, err := local.GetSessionFull(ctx, "suffix")
	require.NoError(t, err)
	require.NotNil(t, session)
	session.FileHash = new("incremental-race")
	require.NoError(t, local.UpsertSession(ctx, *session))
	_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET updated_at='2000-01-01T00:00:00Z' WHERE id='suffix'`)
	require.NoError(t, err)
	store := &Store{pg: syncer.pg}
	_, before, found := store.GetSessionVersion(ctx, "suffix")
	require.True(t, found)
	hookRan := false
	syncer.beforeMessageComparison = sync.OnceFunc(func() {
		require.NoError(t, local.ReplaceSessionMessages(ctx, "suffix", messages))
		hookRan = true
	})
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	require.False(t, result.Full)
	require.True(t, hookRan)
	assert.Zero(t, result.MessagesPushed)
	var tokens, count int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens),0) FROM usage_events WHERE session_id='suffix'`).Scan(&tokens))
	assert.Equal(t, 22, tokens, "changed usage must publish even when the snapshot transcript matches")
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id='suffix'`).Scan(&count))
	assert.Equal(t, 5, count)
	_, after, found := store.GetSessionVersion(ctx, "suffix")
	require.True(t, found)
	assert.NotEqual(t, before, after, "changed usage must wake remote session viewers")
	t.Logf("remote input tokens=%d, messages=%d, session version before=%d after=%d", tokens, count, before, after)
}
