// Package rawtest holds synthetic capture fixtures and stored-result assertions
// shared by the PostgreSQL pipeline and executable runtime integration tests.
package rawtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func Oracle(t *testing.T, agent parser.AgentType, root string, policy config.ArchiveContent, images config.ToolResultImages) (*db.DB, *agentsync.Engine) {
	t.Helper()
	archive, err := db.OpenIsolated(filepath.Join(t.TempDir(), "oracle.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	archive.SetArchiveContent(policy)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "capture-device", Ephemeral: true, DisableFilesystemProjectDiscovery: true, ToolResultImages: images})
	t.Cleanup(engine.Close)
	return archive, engine
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
		assert.Equal(t, normalizeMessages(t, am), normalizeMessages(t, bm), "messages %s", id)
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
	s.CreatedAt = ""
	s.LocalModifiedAt = nil
	s.TranscriptRevision = nil
	return s
}

func normalizeMessages(t *testing.T, messages []db.Message) []db.Message {
	// Empty SQL result sets differ only in nil versus allocated slice transport.
	if len(messages) == 0 {
		return nil
	}
	for i := range messages {
		m := &messages[i]
		m.ID = 0
		// PostgreSQL JSONB changes key order/whitespace, never the usage values.
		if len(m.TokenUsage) > 0 {
			var v any
			require.NoError(t, json.Unmarshal(m.TokenUsage, &v))
			b, err := json.Marshal(v)
			require.NoError(t, err)
			m.TokenUsage = b
		}
		m.Timestamp = normalizeTime(m.Timestamp)
		for j := range m.ToolCalls {
			c := &m.ToolCalls[j]
			c.MessageID = 0
			for k := range c.ResultEvents {
				e := &c.ResultEvents[k]
				e.Timestamp = normalizeTime(e.Timestamp)
				// SQLite-only late-result reconciliation state is deliberately not mirrored.
				e.RawContentDigest = nil
				e.SummaryParticipates = nil
			}
		}
	}
	return messages
}
func normalizeTime(s string) string {
	if v, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return v.UTC().Format(time.RFC3339Nano)
	}
	return s
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
			e.OccurredAt = normalizeTime(*occurred)
		}
		e.SessionID = publicID
		got = append(got, e)
	}
	require.NoError(t, rows.Err())
	for i := range want {
		want[i].ID = 0
		want[i].OccurredAt = normalizeTime(want[i].OccurredAt)
	}
	assert.Equal(t, want, got, "complete usage events for %s", publicID)
}
