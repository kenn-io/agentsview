package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

func TestActivityReportTokenSelectionRevalidatesResolvedQuery(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	selection, err := resolveActivitySelection(activitySelectionInput{
		Preset: "month", Date: "2026-07-01", Timezone: "UTC",
	}, now)
	require.NoError(t, err)
	valid := newActivityReportTokenPayload(selection, "digest")

	tests := []struct {
		name   string
		mutate func(*activityReportTokenPayload)
	}{
		{
			name: "range longer than one year",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Query.RangeEnd = payload.Query.RangeStart.Add(366 * 24 * time.Hour)
				payload.Query.EffectiveEnd = payload.Query.RangeEnd
			},
		},
		{
			name: "unapproved bucket",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Query.BucketUnit = activity.BucketMinute
				payload.Query.BucketSeconds = 1
			},
		},
		{
			name: "too many buckets",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Query.RangeEnd = payload.Query.RangeStart.Add(365 * 24 * time.Hour)
				payload.Query.EffectiveEnd = payload.Query.RangeEnd
				payload.Query.BucketUnit = activity.BucketMinute
				payload.Query.BucketSeconds = 300
			},
		},
		{
			name: "unbounded gap cap",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Query.GapCapSeconds = 3600
			},
		},
		{
			name: "effective end outside range",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Query.EffectiveEnd = payload.Query.RangeEnd.Add(time.Second)
			},
		},
		{
			name: "mismatched filter timezone",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Filter.Timezone = "America/Chicago"
			},
		},
		{
			name: "mutually exclusive automation filters",
			mutate: func(payload *activityReportTokenPayload) {
				payload.Filter.ExcludeAutomated = true
				payload.Filter.ExcludeInteractive = true
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := valid
			tc.mutate(&payload)
			_, err := payload.selection()
			assert.Error(t, err)
		})
	}

	_, err = valid.selection()
	require.NoError(t, err)
}
