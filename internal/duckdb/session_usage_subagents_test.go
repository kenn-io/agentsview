//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

// TestSessionUsageWithSubagentsMatchesSQLite pins DuckDB store-contract
// parity for the presentation-time subagent rollup: the mirror must produce
// the same combined totals, the same deduplication of a row shared between a
// parent and a subagent transcript, and the same tagged breakdown as the
// SQLite archive it mirrors.
func TestSessionUsageWithSubagentsMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	const (
		parentID      = "claude:duck-parent"
		childAID      = "agent-duck-a"
		childBID      = "agent-duck-b"
		latestChildID = "agent-duck-latest"
	)
	usageMsg := func(
		sessionID string, ordinal int, clock, messageID string, in, out int,
	) db.Message {
		return db.Message{
			SessionID: sessionID,
			Ordinal:   ordinal,
			Role:      "assistant",
			Content:   "work",
			Timestamp: "2026-05-20T" + clock + "Z",
			Model:     "test-opus",
			// A parent turn echoed inside a sidechain transcript shares
			// these identifiers, which is what dedup keys off.
			ClaudeMessageID: messageID,
			ClaudeRequestID: "req-" + messageID,
			TokenUsage: jsontext.Value(fmt.Sprintf(
				`{"input_tokens":%d,"output_tokens":%d}`, in, out)),
		}
	}
	// outputTokens is the aggregate the parser derives from that transcript
	// alone. Across the four sessions the stored totals sum to 2800; removing
	// the two 500-token echoes while preserving the parent's output-only 200
	// yields 1800.
	session := func(
		id string, parent *string, relationship string, outputTokens int,
	) db.Session {
		return db.Session{
			ID: id, Project: "duck-subagents", Machine: "local",
			Agent:                "claude",
			StartedAt:            new("2026-05-20T09:00:00Z"),
			EndedAt:              new("2026-05-20T11:00:00Z"),
			MessageCount:         1,
			ParentSessionID:      parent,
			RelationshipType:     relationship,
			TotalOutputTokens:    outputTokens,
			HasTotalOutputTokens: true,
		}
	}

	parentRef := parentID
	parentSession := session(parentID, nil, "", 700)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: parentSession,
			Messages: []db.Message{
				usageMsg(parentID, 0, "10:00:00", "m-1", 1000, 500),
				{
					SessionID:       parentID,
					Ordinal:         1,
					Role:            "assistant",
					Content:         "output-only",
					Timestamp:       "2026-05-20T10:00:30Z",
					Model:           "test-opus",
					OutputTokens:    200,
					HasOutputTokens: true,
				},
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: session(childAID, &parentRef, "subagent", 1500),
			Messages: []db.Message{
				usageMsg(childAID, 0, "10:01:00", "m-2", 2000, 1000),
				usageMsg(childAID, 1, "10:02:00", "m-1", 1000, 500),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: session(childBID, &parentRef, "subagent", 100),
			Messages: []db.Message{
				usageMsg(childBID, 0, "10:03:00", "m-3", 500, 100),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: session(
				latestChildID, &parentRef, "subagent", 500),
			Messages: []db.Message{
				usageMsg(
					latestChildID, 0, "10:04:00", "m-1", 1000, 500),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
	})
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "push to DuckDB")
	duck := NewStoreFromDB(syncer.DB())
	ids := []string{
		parentID, childAID, childBID, latestChildID,
	}
	sqliteRows, err := local.GetSessionUsageRows(ctx, ids)
	require.NoError(t, err)
	duckRows, err := duck.GetSessionUsageRows(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, sqliteRows.RawOutputTokensBySession,
		duckRows.RawOutputTokensBySession,
		"raw per-transcript output metadata matches SQLite")
	assert.Equal(t, sqliteRows.DiscardedContributingSessions,
		duckRows.DiscardedContributingSessions,
		"discarded contributing-row metadata matches SQLite")
	require.Len(t, sqliteRows.Rows, 3)
	require.Len(t, duckRows.Rows, 3)
	assert.Equal(t, []string{
		childAID, childBID, latestChildID,
	}, []string{
		duckRows.Rows[0].SourceSessionID,
		duckRows.Rows[1].SourceSessionID,
		duckRows.Rows[2].SourceSessionID,
	})

	sqliteGot, err := service.SessionUsageWithSubagents(
		ctx, local, parentID, true)
	require.NoError(t, err, "SQLite combined usage")
	require.NotNil(t, sqliteGot)

	duckGot, err := service.SessionUsageWithSubagents(ctx, duck, parentID, true)
	require.NoError(t, err, "DuckDB combined usage")
	require.NotNil(t, duckGot)

	assert.False(t, sqliteGot.HasCost,
		"the output-only parent tokens make the combined cost incomplete")
	assert.Zero(t, sqliteGot.Cost)
	assert.Equal(t, sqliteGot.Cost, duckGot.Cost)
	assert.Equal(t, sqliteGot.HasCost, duckGot.HasCost)
	assert.Equal(t, sqliteGot.CostSource, duckGot.CostSource)
	assert.Equal(t, sqliteGot.SubagentCount, duckGot.SubagentCount)
	assert.Equal(t, sqliteGot.BreakdownCount, duckGot.BreakdownCount)
	assert.Equal(t, sqliteGot.Models, duckGot.Models)
	assert.Equal(t, sqliteGot.UnpricedModels, duckGot.UnpricedModels)
	assert.Equal(t, 1800, sqliteGot.TotalOutputTokens,
		"output tokens are deduplicated without dropping the parent's "+
			"output-only message")
	assert.Equal(t, sqliteGot.TotalOutputTokens, duckGot.TotalOutputTokens)
	assert.Equal(t, sqliteGot.HasTokenData, duckGot.HasTokenData)
	assert.Equal(t, sqliteGot.PeakContextTokens, duckGot.PeakContextTokens)

	require.Len(t, duckGot.Breakdown, 3)
	assert.Equal(t, sqliteGot.Breakdown, duckGot.Breakdown,
		"breakdown rows, ordering, and subagent tagging match SQLite")
	assert.Equal(t, []string{childAID, childBID, latestChildID}, []string{
		duckGot.Breakdown[0].SubagentSessionID,
		duckGot.Breakdown[1].SubagentSessionID,
		duckGot.Breakdown[2].SubagentSessionID,
	})

	// The own-session path stays own-session on both backends.
	duckOwn, err := duck.GetSessionUsage(ctx, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, duckOwn)
	assert.Equal(t, money.MustParseDollars("0.007"), duckOwn.Cost)
	assert.Zero(t, duckOwn.SubagentCount)
}
