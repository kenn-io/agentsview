//go:build chtest

package clickhouse

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Filtering before the session join must retain fallback timestamps and the
// full local day, and must not resurrect a replaced timestamp.
func TestDailyUsageSourceBounds(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	_, err := store.DB().ExecContext(ctx, "SYSTEM STOP MERGES messages")
	require.NoError(t, err)
	for _, agent := range []string{"claude", "codex", "grok", "gemini", "opencode", "copilot", "cursor", "amp"} {
		id := "source-bounds-" + agent
		_, err = store.DB().ExecContext(ctx, `INSERT INTO sessions
		 (id,project,agent,started_at,message_count,user_message_count,push_version)
		 VALUES (?, 'source-bounds', ?, toDateTime64('2026-03-08 12:00:00',6,'UTC'), 6, 3, 1)`, id, agent)
		require.NoError(t, err)
		for ordinal, stamp := range []string{
			"",
			"2026-03-08 04:59:59.999999",
			"2026-03-08 05:00:00",
			"2026-03-09 03:59:59.999999",
			"2026-03-09 04:00:00",
			"2026-03-08 12:01:00",
		} {
			_, err = store.DB().ExecContext(ctx, `INSERT INTO messages
			 (session_id,ordinal,role,timestamp,model,token_usage,push_version)
			 VALUES (?,?,'assistant',parseDateTime64BestEffortOrNull(?,6,'UTC'),'gpt-4o',?,1)`,
				id, ordinal, stamp, fmt.Sprintf(`{"output_tokens":%d}`, ordinal+1))
			require.NoError(t, err)
		}
		_, err = store.DB().ExecContext(ctx, `INSERT INTO messages
		 (session_id,ordinal,role,timestamp,model,token_usage,push_version)
		 VALUES (?,5,'assistant',toDateTime64('2026-04-08 12:01:00',6,'UTC'),'gpt-4o',?,2)`, id, `{"output_tokens":6}`)
		require.NoError(t, err)
		got, err := store.GetDailyUsage(ctx, db.UsageFilter{
			From: "2026-03-08", To: "2026-03-08", Timezone: "America/New_York",
			Project: "source-bounds", Agent: agent,
		})
		require.NoError(t, err)
		require.Equal(t, 8, got.Totals.OutputTokens)
	}
}
