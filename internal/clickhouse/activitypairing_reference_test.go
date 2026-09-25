package clickhouse

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
	"unique"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// The mirror pairs each session's messages from its kept rows instead of
// rendering a transcript for activity.PairActivityEvents. Both must agree,
// including unstamped messages, clock reversals, and assistant messages
// without a model.
func TestActivityMessagePairsMatchReference(t *testing.T) {
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	gapCap := time.Duration(q.GapCapSeconds) * time.Second
	base := q.RangeStart.Add(-2 * time.Hour)
	roles := []string{"user", "assistant", "assistant", "tool"}
	models := []string{"", "model-a", "model-b"}
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 200 {
		inputs := map[string]activitySessionInputs{}
		var ids []string
		var transcript []activity.ActivityEvent
		for session := range 1 + rng.IntN(4) {
			id := fmt.Sprintf("s%d-%d", trial, session)
			ids = append(ids, id)
			var entry activitySessionInputs
			for ordinal := range rng.IntN(12) {
				role, model := roles[rng.IntN(len(roles))], models[rng.IntN(len(models))]
				m := clickActivityMessage{ordinal: ordinal, role: unique.Make(role), model: unique.Make(model)}
				if rng.IntN(5) != 0 {
					// Spread across the day and the gap before it, with reversals.
					m.ts = base.Add(time.Duration(rng.IntN(28*60)) * time.Minute).
						Add(time.Duration(rng.IntN(1000)) * time.Microsecond)
					transcript = append(transcript, activity.ActivityEvent{
						SessionID: id, Ordinal: ordinal, Timestamp: m.ts.UTC().Format(time.RFC3339Nano),
						Role: role, Model: model,
					})
				}
				entry.messages = append(entry.messages, m)
			}
			inputs[id] = entry
		}
		want := activity.PairActivityEvents(transcript, q.RangeStart, q.EffectiveEnd, gapCap)
		var store Store
		got, _, err := store.pairActivitySessions(inputs, ids, q.RangeStart.Add(-gapCap), q.EffectiveEnd, q)
		require.NoError(t, err)
		require.Equal(t, want, got, "trial %d", trial)
	}
}
