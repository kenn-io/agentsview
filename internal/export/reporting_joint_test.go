package export

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJointReportingCanonicalIdentity(t *testing.T) {
	hour := reportingHourFixture("2026-07-29-13")
	hour.SchemaVersion = 4
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
