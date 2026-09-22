//go:build chtest

package clickhouse

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// Out-of-range duplicates must not suppress in-range usage at timezone or DST boundaries.
func TestClickHouseActivityUsageBounds(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	for _, zone := range []string{"UTC", "America/New_York", "Pacific/Kiritimati", "Pacific/Pago_Pago"} {
		for _, date := range []string{"2026-03-08", "2026-11-01"} {
			q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: date, Timezone: zone}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			// The fixture and survivor selection branch only on claude versus
			// any other agent.
			for _, agent := range []string{"claude", "codex"} {
				t.Run(zone+"/"+date+"/"+agent, func(t *testing.T) {
					id := t.Name()
					for ordinal, timestamp := range []string{
						q.RangeStart.Add(-time.Microsecond).UTC().Format(time.RFC3339Nano),
						q.RangeStart.UTC().Format(time.RFC3339Nano),
						q.RangeEnd.Add(-time.Microsecond).UTC().Format(time.RFC3339Nano),
						q.RangeEnd.UTC().Format(time.RFC3339Nano), "",
					} {
						tokens := []int{7, 2, 3, 11, 5}[ordinal]
						pair := ""
						sourceUUID := ""
						if ordinal < 2 {
							if agent == "claude" {
								pair = id + "-pair"
							} else {
								sourceUUID = id + "-duplicate"
							}
						}
						_, err := store.DB().ExecContext(ctx, `INSERT INTO messages
       (session_id,ordinal,timestamp,model,token_usage,claude_message_id,claude_request_id,source_uuid,push_version)
       VALUES (?,?,parseDateTime64BestEffortOrNull(?),'gpt-4o',?,?,?,?,1)`,
							id, ordinal, timestamp, fmt.Sprintf(`{"output_tokens":%d}`, tokens), pair, pair, sourceUUID)
						require.NoError(t, err)
					}
					_, err := store.DB().ExecContext(ctx, `INSERT INTO sessions (id,agent,started_at,ended_at,push_version)
      VALUES (?,?,parseDateTime64BestEffort(?),parseDateTime64BestEffort(?),1)`, id, agent, q.RangeStart.UTC().Format(time.RFC3339Nano), q.RangeEnd.UTC().Format(time.RFC3339Nano))
					require.NoError(t, err)
					ids := []string{id}
					start, end := q.RangeStart.UTC().Format(time.RFC3339Nano), q.RangeEnd.UTC().Format(time.RFC3339Nano)
					got, _, err := store.activityReportUsage(ctx, chSessionSetFromIDs(ids), ids, start, end, q)
					require.NoError(t, err)
					var total int
					for _, row := range got {
						total += row.OutputTokens
					}
					require.Equal(t, 10, total)
				})
			}
		}
	}
}

func TestClickHouseActivityUsageFractionalBound(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-01-10T12:00:00.100Z", To: "2026-01-10T12:01:00.500Z",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	for _, query := range []string{
		`INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,push_version)
		 VALUES ('fractional',0,'2026-01-10 12:01:00.250','gpt-4o','{"output_tokens":7}',1)`,
		`INSERT INTO sessions (id,agent,started_at,ended_at,push_version)
		 VALUES ('fractional','codex','2026-01-10 12:00:00.100','2026-01-10 12:01:00.500',1)`,
	} {
		_, err := store.DB().ExecContext(ctx, query)
		require.NoError(t, err)
	}
	start, end := activityReportRangeBoundsUTC(q)
	ids := []string{"fractional"}
	rows, _, err := store.activityReportUsage(ctx, chSessionSetFromIDs(ids), ids, start, end, q)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 7, rows[0].OutputTokens)
}
