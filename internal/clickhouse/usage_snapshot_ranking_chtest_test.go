//go:build chtest

package clickhouse

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestUsageSnapshotRanking(t *testing.T) {
	local, target := seedUsagePriceFixture(t)
	const a, b = "ranking-z", "ranking-a"
	const early, mid, late = "2026-01-12T03:00:00.000Z", "2026-01-12T03:00:30.000Z", "2026-01-12T03:01:00.000Z"
	snapshot := func(id string, ordinal int, ts, usage string) db.Message {
		m := usagePriceMessage(id, ordinal, ts, "claude-test", usage)
		m.ClaudeMessageID, m.ClaudeRequestID = "ranking-message", "ranking-request"
		return m
	}
	missingBoth := usagePriceMessage(a, 1, early, "claude-test", `{"input_tokens":2,"output_tokens":3}`)
	missingMessage := usagePriceMessage(a, 2, early, "claude-test", `{"input_tokens":4,"output_tokens":5}`)
	missingMessage.ClaudeRequestID = "request-only"
	missingRequest := usagePriceMessage(a, 3, early, "claude-test", `{"input_tokens":6,"output_tokens":7}`)
	missingRequest.ClaudeMessageID = "message-only"
	sharedA := usagePriceMessage(a, 4, early, "claude-test", `{"input_tokens":8,"output_tokens":9}`)
	sharedB := usagePriceMessage(b, 2, late, "claude-test", `{"input_tokens":8,"output_tokens":9}`)
	sharedA.SourceUUID, sharedB.SourceUUID = "ranking-shared", "ranking-shared"
	writes := []db.SessionBatchWrite{
		{Session: fixtureSession(a, "rank-alpha", "a", early, 8), Messages: []db.Message{
			snapshot(a, 0, early, `{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":7}}`),
			missingBoth, missingMessage, missingRequest, sharedA,
			snapshot(a, 5, late, `{"input_tokens":10,"output_tokens":20}`),
			snapshot(a, 6, late, `{"input_tokens":10,"output_tokens":20}`),
			snapshot(a, 7, mid, `{"input_tokens":10,"output_tokens":20}`),
		}, UsageEvents: []db.UsageEvent{{Source: "hermes", Model: "claude-test", InputTokens: 11, OutputTokens: 12, OccurredAt: early, DedupKey: "ranking-event"}}, ReplaceMessages: true, DataVersion: 1},
		{Session: fixtureSession(b, "rank-beta", "b", late, 4), Messages: []db.Message{
			snapshot(b, 0, late, `{"input_tokens":10,"output_tokens":20}`),
			snapshot(b, 1, late, `{"input_tokens":10,"output_tokens":20}`), sharedB,
			snapshot(b, 3, mid, `{"input_tokens":10,"output_tokens":20}`),
		}, ReplaceMessages: true, DataVersion: 1},
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := newTestStore(t, syncer.conn, syncer.target)
	execPreparedUsage(t, store, "SYSTEM STOP VIEW prepare_usage")
	pushPreparedUsage(t, syncer)

	for _, phase := range []string{"raw", "prepared", "delta"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "prepared" {
				runPreparedUsageRefresh(t, store)
			}
			if phase == "delta" {
				require.NoError(t, local.ReplaceSessionUsageEvents(t.Context(), b, []db.UsageEvent{{Source: "hermes", Model: "claude-test", InputTokens: 1, OutputTokens: 2, OccurredAt: late, DedupKey: "ranking-delta"}}))
				pushPreparedUsage(t, syncer)
			}
			state, err := store.preparedUsageState(t.Context())
			require.NoError(t, err)
			switch phase {
			case "raw":
				require.False(t, state.ready)
			case "prepared":
				require.True(t, state.ready)
				require.Empty(t, state.changed)
			case "delta":
				require.True(t, state.ready)
				require.Contains(t, state.changed, b)
			}
			query := usageCTEFor(state, db.UsageFilter{Timezone: "UTC"}, "")
			readCtx, err := withUsageDeltaTables(t.Context(), state)
			require.NoError(t, err)
			rows, err := store.DB().QueryContext(readCtx, query.cte+` SELECT session_id, message_ordinal, output_tokens_norm, snapshot_deduplicated_output_tokens, web_search_requests_norm FROM usage_localized WHERE claude_message_id = 'ranking-message' AND claude_request_id = 'ranking-request'`, query.args...)
			require.NoError(t, err)
			defer rows.Close()
			require.True(t, rows.Next())
			var session string
			var ordinal sql.NullInt64
			var output, suppressed, searches int64
			require.NoError(t, rows.Scan(&session, &ordinal, &output, &suppressed, &searches))
			assert.Equal(t, a, session)
			assert.Equal(t, sql.NullInt64{Int64: 6, Valid: true}, ordinal)
			assert.EqualValues(t, 20, output)
			assert.EqualValues(t, 105, suppressed)
			assert.EqualValues(t, 7, searches)
			assert.False(t, rows.Next())
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			var inputTotal, outputTotal int64
			err = store.DB().QueryRowContext(readCtx, query.cte+` SELECT sum(input_tokens_norm), sum(output_tokens_norm) FROM usage_localized WHERE session_id = 'ranking-z'`, query.args...).Scan(&inputTotal, &outputTotal)
			require.NoError(t, err)
			assert.EqualValues(t, 41, inputTotal)
			assert.EqualValues(t, 56, outputTotal)
			// Parity also checks that the shared source UUID contributes once across sessions.
			filter := db.UsageFilter{Timezone: "UTC"}
			want, err := local.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			got, err := store.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, dailyUsageWire(t, want, true), dailyUsageWire(t, got, false))
			wantTop, err := local.GetTopSessionsByCost(t.Context(), filter, 20)
			require.NoError(t, err)
			normalizeSQLiteTopSessions(t, wantTop)
			gotTop, err := store.GetTopSessionsByCost(t.Context(), filter, 20)
			require.NoError(t, err)
			assert.Equal(t, wantTop, gotTop)
		})
	}
}
