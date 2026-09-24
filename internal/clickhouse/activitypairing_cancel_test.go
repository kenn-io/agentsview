package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// A report whose request is cancelled stops pairing instead of finishing
// work no one will read.
func TestPairActivitySessionsStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _, err := (&Store{}).pairActivitySessions(ctx, map[string]activitySessionInputs{}, []string{"a"},
		start, start.Add(time.Hour), activity.Query{RangeStart: start, EffectiveEnd: start.Add(time.Hour)})
	require.ErrorIs(t, err, context.Canceled)
}
