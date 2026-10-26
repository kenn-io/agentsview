package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplicaPendingPush(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		cursor     string
		pending    int
		maxPending time.Duration
		wantValue  string
		wantErr    string
	}{
		{
			name:       "nothing pending ignores an old cursor",
			cursor:     "2026-10-26T16:15:20.000Z",
			maxPending: 15 * time.Minute,
			wantValue:  "none",
		},
		{
			name:       "fresh push within the limit",
			cursor:     "2026-10-26T21:29:30.000Z",
			pending:    2,
			maxPending: 15 * time.Minute,
			wantValue:  "2 session(s) changed since the push that started 30s ago",
		},
		{
			name:       "wait equal to the limit passes",
			cursor:     "2026-10-26T21:15:00.000Z",
			pending:    1,
			maxPending: 15 * time.Minute,
			wantValue:  "1 session(s) changed since the push that started 15m0s ago",
		},
		{
			name:       "stalled push exceeds the limit",
			cursor:     "2026-10-26T16:15:20.000Z",
			pending:    126,
			maxPending: 15 * time.Minute,
			wantValue:  "126 session(s) changed since the push that started 5h14m40s ago",
			wantErr:    "126 archive session(s) have waited 5h14m40s to push, more than --max-pending 15m0s; run `agentsview clickhouse service logs` to see why pushes fail",
		},
		{
			name:      "stalled push only reports without a limit",
			cursor:    "2026-10-26T16:15:20.000Z",
			pending:   126,
			wantValue: "126 session(s) changed since the push that started 5h14m40s ago",
		},
		{
			name:       "cursor ahead of the clock reads as no wait",
			cursor:     "2026-10-26T21:31:00.000Z",
			pending:    1,
			maxPending: time.Minute,
			wantValue:  "1 session(s) changed since the push that started 0s ago",
		},
		{
			name:       "never pushed fails the limit",
			pending:    4,
			maxPending: 15 * time.Minute,
			wantValue:  "4 session(s), never pushed",
			wantErr:    "4 archive session(s) have never been pushed; run `agentsview clickhouse service logs` to see why pushes fail",
		},
		{
			name:      "never pushed only reports without a limit",
			pending:   4,
			wantValue: "4 session(s), never pushed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, err := replicaPendingPush("clickhouse", tt.cursor, tt.pending, now, tt.maxPending)
			assert.Equal(t, "Pending push:", row.Label)
			assert.Equal(t, tt.wantValue, row.Value)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestReplicaPendingPushRejectsMalformedCursor(t *testing.T) {
	_, err := replicaPendingPush("clickhouse", "not-a-time", 1, time.Now(), 0)
	require.ErrorContains(t, err, `parsing push cursor "not-a-time"`)
}
