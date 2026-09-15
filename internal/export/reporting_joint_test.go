package export

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJointReportingCanonicalIdentity(t *testing.T) {
	hour := reportingHourFixture("2026-07-29-13")
	hour.SchemaVersion = 4
	hour.BucketSeconds = 300
	hour.Joint = &ReportingJoint{ProjectKeys: []string{"b", "a", "a"}, Cells: []ReportingCell{
		{BucketStart: "2026-07-29T13:00:00Z", ProjectKey: "b", Model: "model-b", Agent: "agent-a", Automation: "interactive", AgentMinutes: 1, MaxAgents: 1},
		{BucketStart: "2026-07-29T13:05:00Z", ProjectKey: "a", Model: "model-a", Agent: "agent-b", Automation: "automated", AgentMinutes: 2, MaxAgents: 1},
	}}
	first, canonical, err := FinalizeReportingHour(hour)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, first.Joint.ProjectKeys)
	slices.Reverse(hour.Joint.Cells)
	slices.Reverse(hour.Joint.ProjectKeys)
	_, reordered, err := FinalizeReportingHour(hour)
	require.NoError(t, err)
	assert.Equal(t, canonical, reordered)

	hour.Joint.Cells[0].Pricing.UnpricedRows = 1
	corrected, _, err := FinalizeReportingHour(hour)
	require.NoError(t, err)
	assert.NotEqual(t, first.Digest, corrected.Digest, "cell metadata is part of replacement identity")
	hour.Joint.Cells = append(hour.Joint.Cells, hour.Joint.Cells[0])
	_, _, err = FinalizeReportingHour(hour)
	assert.ErrorContains(t, err, "duplicate joint cell")
}

func TestJointReportingRejectsInconsistentResolution(t *testing.T) {
	hour := reportingHourFixture("2026-07-29-13")
	hour.SchemaVersion, hour.BucketSeconds = 4, 300
	hour.Joint = &ReportingJoint{Cells: []ReportingCell{{BucketStart: "2026-07-29T13:00:00Z"}}}
	for _, tc := range []struct {
		name    string
		seconds int
		message string
	}{
		{"missing", 0, "bucket_seconds"},
		{"non-divisor", 420, "whole-minute divisor"},
		{"wrong bucket count", 60, "requires 60 activity buckets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := hour
			invalid.BucketSeconds = tc.seconds
			_, _, err := FinalizeReportingHour(invalid)
			assert.ErrorContains(t, err, tc.message)
		})
	}
	hour.Joint.Cells[0].BucketStart = "2026-07-29T13:01:00Z"
	_, _, err := FinalizeReportingHour(hour)
	assert.ErrorContains(t, err, "invalid bucket")
	// The same timestamp is valid at one-minute precision.
	hour.BucketSeconds = 60
	hour.Activity.Buckets = make([]ReportingActivityBucket, 60)
	start := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	for i := range hour.Activity.Buckets {
		hour.Activity.Buckets[i].Start = start.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
	}
	_, _, err = FinalizeReportingHour(hour)
	require.NoError(t, err)
	hour.Period = "2026-07-29-00"
	_, _, err = FinalizeReportingDay(ReportingDay{SchemaVersion: 4, BucketSeconds: 300, Date: "2026-07-29", Hours: []ReportingHour{hour}})
	assert.ErrorContains(t, err, "share its schema and bucket resolution")
}
