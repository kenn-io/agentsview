// Package rawtest holds synthetic capture fixtures and stored-result assertions
// shared by the PostgreSQL pipeline and executable runtime integration tests.
package rawtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawparity"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func Oracle(t *testing.T, agent parser.AgentType, root string, policy config.ArchiveContent, images config.ToolResultImages) (*db.DB, *agentsync.Engine) {
	t.Helper()
	archive, err := db.OpenIsolated(t.Context(), filepath.Join(t.TempDir(), "oracle.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	archive.SetArchiveContent(policy)
	engine := agentsync.NewEngine(t.Context(), archive, agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "capture-device", Ephemeral: true, DisableFilesystemProjectDiscovery: true, ToolResultImages: images, FrictionEnabled: true})
	t.Cleanup(engine.Close)
	return archive, engine
}

// SyncOracle syncs the oracle archive and then reviews its friction, because
// hosted projection always reviews while local sync leaves it to a later pass.
func SyncOracle(t *testing.T, engine *agentsync.Engine) int {
	t.Helper()
	stats := engine.SyncAll(t.Context(), nil)
	_, err := engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	return stats.Synced
}

// EqualStored catches lost content, tool pairing, usage presence, signals,
// findings and public relationship mappings. Expected rows come from the local
// engine, never the hosted preparation/projection API.
func EqualStored(t *testing.T, ctx context.Context, want *db.DB, got db.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		a, err := want.GetSessionFull(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, a, id)
		b, err := got.GetSessionFull(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, b, id)
		assert.Equal(t, normalizeSession(*a), normalizeSession(*b), "session %s", id)
		am, err := want.GetAllMessages(ctx, id)
		require.NoError(t, err)
		bm, err := got.GetAllMessages(ctx, id)
		require.NoError(t, err)
		equalContent(t, rawparity.Snapshot{Session: *a, Messages: am}, rawparity.Snapshot{Session: *b, Messages: bm})
		// Public ownership, reader order and derived tool signals remain part
		// of this stricter check beyond the normalized parity content matrix.
		require.Len(t, bm, len(am))
		for i, message := range am {
			assert.Equal(t, id, bm[i].SessionID, "message owner %s/%d", id, i)
			assert.Equal(t, message.Ordinal, bm[i].Ordinal, "message order %s/%d", id, i)
			require.Len(t, bm[i].ToolCalls, len(message.ToolCalls))
			for j, call := range message.ToolCalls {
				other := bm[i].ToolCalls[j]
				assert.Equal(t, id, other.SessionID, "tool owner %s/%d/%d", id, message.Ordinal, j)
				assert.Equal(t, call.CallIndex, other.CallIndex, "tool order %s/%d/%d", id, message.Ordinal, j)
				require.Len(t, other.ResultEvents, len(call.ResultEvents))
				for k, event := range call.ResultEvents {
					assert.Equal(t, event.EventIndex, other.ResultEvents[k].EventIndex, "result order %s/%d/%d/%d", id, message.Ordinal, call.CallIndex, k)
				}
				assert.Equal(t, []*string{call.ObservedOutcome, call.ObservedRepeat, call.SequenceEnding}, []*string{other.ObservedOutcome, other.ObservedRepeat, other.SequenceEnding}, "tool signals %s/%d/%d", id, message.Ordinal, call.CallIndex)
			}
		}
		au, err := want.GetSessionUsage(ctx, id, true)
		require.NoError(t, err)
		bu, err := got.GetSessionUsage(ctx, id, true)
		require.NoError(t, err)
		assert.Equal(t, au, bu, "usage %s", id)
	}
	af, err := want.ListSecretFindings(ctx, db.SecretFindingFilter{Limit: 1000})
	require.NoError(t, err)
	bf, err := got.ListSecretFindings(ctx, db.SecretFindingFilter{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, af, bf, "complete findings and coordinates")
}

func normalizeSession(s db.Session) db.Session {
	// Machine and file coordinates identify the custody transport, not content.
	s.Machine = ""
	s.FilePath = nil
	s.FileSize = nil
	s.FileMtime = nil
	s.FileInode = nil
	s.FileDevice = nil
	s.FileHash = nil
	// These fields are archive-local import bookkeeping or publication clocks.
	s.NextOrdinal = 0
	s.LastEntryUUID = nil
	s.ClaudeLinearParse = nil
	s.LastWriteIncremental = false
	// PostgreSQL returns the effective DisplayName but does not expose the
	// SQLite-only raw session_name read field. DisplayName remains compared.
	s.SessionName = nil
	// The hosted public adapter deliberately clears the internal parser parent
	// cache; the resolved public ParentSessionID(s) remain fully compared.
	s.ParserParentSessionID = nil
	// PostgreSQL session reads leave out friction summaries until a reader
	// needs them; EqualFriction compares the stored columns directly.
	s.FrictionCount = 0
	s.FrictionRulesVersion = ""
	s.FrictionHash = ""
	s.CreatedAt = ""
	s.LocalModifiedAt = nil
	s.TranscriptRevision = nil
	return s
}

func equalContent(t *testing.T, a, b rawparity.Snapshot) {
	t.Helper()
	comparison, err := rawparity.Compare(a, b)
	require.NoError(t, err)
	assert.Empty(t, comparison.Differing, "stored content parts differ")
}

// EqualFriction compares the friction summary, whose hash covers every
// finding, between the local oracle and the hosted session row.
func EqualFriction(t *testing.T, ctx context.Context, oracle *db.DB, pg *sql.DB, publicID, physicalID string) {
	t.Helper()
	want, err := oracle.GetSessionFull(ctx, publicID)
	require.NoError(t, err)
	require.NotNil(t, want, publicID)
	var count int
	var version, hash string
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT friction_count,friction_rules_version,friction_hash FROM sessions WHERE id=$1`, physicalID).Scan(&count, &version, &hash))
	assert.Equal(t, []any{want.FrictionCount, want.FrictionRulesVersion, want.FrictionHash}, []any{count, version, hash}, "friction %s", publicID)
}

// EqualUsageEvents preserves the complete normalized accounting rows, including
// nullable money/message links, provider/source keys and deduplication identity.
func EqualUsageEvents(t *testing.T, ctx context.Context, oracle *db.DB, pg *sql.DB, publicID, physicalID string) {
	t.Helper()
	want, err := oracle.GetUsageEvents(ctx, publicID)
	require.NoError(t, err)
	rows, err := pg.QueryContext(ctx, `SELECT message_ordinal,source,model,provider_id,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,reasoning_tokens,cost_microdollars,cost_status,cost_source,occurred_at,dedup_key FROM usage_events WHERE session_id=$1 ORDER BY occurred_at NULLS FIRST,id`, physicalID)
	require.NoError(t, err)
	defer rows.Close()
	var got []db.UsageEvent
	for rows.Next() {
		var e db.UsageEvent
		var cost *int64
		var occurred *string
		require.NoError(t, rows.Scan(&e.MessageOrdinal, &e.Source, &e.Model, &e.ProviderID, &e.InputTokens, &e.OutputTokens, &e.CacheCreationInputTokens, &e.CacheReadInputTokens, &e.ReasoningTokens, &cost, &e.CostStatus, &e.CostSource, &occurred, &e.DedupKey))
		if cost != nil {
			e.Cost = &money.Money{Microdollars: *cost}
		}
		if occurred != nil {
			e.OccurredAt = *occurred
		}
		got = append(got, e)
	}
	require.NoError(t, rows.Err())
	equalContent(t, rawparity.Snapshot{Usage: want}, rawparity.Snapshot{Usage: got})
}
