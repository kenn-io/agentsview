//go:build chtest

package clickhouse

import (
	"sync/atomic"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// The plan usage counts run on every usage summary, so they must read only
// the rows of sessions that have plan usage, not every message and usage
// event in the mirror.
func TestPlanUsageCountsReadOnlyPlanSessions(t *testing.T) {
	const bigMessages = 50_000
	local, target := seedFixture(t)
	const bigID = "zz-big-session"
	stamp := "2026-01-12T00:00:00.000Z"
	msgs := make([]db.Message, bigMessages)
	for i := range msgs {
		msgs[i] = fixtureMessage(bigID, i, "assistant", "x", stamp)
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:     fixtureSession(bigID, "big", "big first", stamp, bigMessages),
		Messages:    msgs,
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	require.NoError(t, local.ReplaceSessionPlanUsage(t.Context(), fixtureBetaID, chPlanUsage(3)))
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = s.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	var read atomic.Uint64
	ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(p *chdriver.Progress) {
		read.Add(p.Rows)
	}))
	counts, err := store.GetPlanUsageSessionCounts(ctx, db.UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.Equal(t, 1, counts.PlanOnly, "beta has plan usage and no usage events")
	require.Positive(t, read.Load(), "the server must report progress, or the bound below proves nothing")
	assert.Less(t, read.Load(), uint64(bigMessages/2),
		"the query must not scan the messages of sessions without plan usage")
}
