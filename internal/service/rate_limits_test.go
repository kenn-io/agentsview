package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// TestRateLimitCurrent_ResetsAtNullability covers the service layer's
// happy path (a snapshot reaches the API shape) and the nullable
// resets_at invariant: a window with no known reset time must reach the
// wire as an omitted field, not the number 0, which the Usage page would
// otherwise render as a bogus "resets in 0m".
func TestRateLimitCurrent_ResetsAtNullability(t *testing.T) {
	known := int64(1789435448)
	for _, tc := range []struct {
		name        string
		resetsAt    *int64
		wantPresent bool
	}{
		{"unknown reset time is omitted, not serialized as 0", nil, false},
		{"a known reset time is serialized", &known, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			require.NoError(t, d.UpsertSession(db.Session{ID: "codex:sess-1", Agent: "codex"}))
			require.NoError(t, d.InsertRateLimitSnapshots([]db.RateLimitSnapshot{{
				SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex",
				PlanType: "pro", WindowKind: "primary",
				ObservedAt: "2026-09-09T10:00:00Z", ResetsAt: tc.resetsAt,
			}}))

			rows, err := service.RateLimitCurrent(
				context.Background(), d, service.RateLimitFilterRequest{},
			)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, tc.resetsAt == nil, rows[0].ResetsAt == nil)

			raw, err := json.Marshal(rows[0])
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &decoded))
			value, present := decoded["resetsAt"]
			assert.Equal(t, tc.wantPresent, present)
			if tc.wantPresent {
				assert.EqualValues(t, *tc.resetsAt, value)
			}
		})
	}
}
