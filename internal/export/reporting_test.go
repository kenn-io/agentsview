package export

import (
	"encoding/json/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReportingHourKeyAcceptsOnlyClosedCanonicalUTCHours(t *testing.T) {
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
	got, err := ParseReportingHourKey("2026-07-29-13", now)
	require.NoError(t, err)

	assert.Equal(t, time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC), got)

	invalid := []string{
		"",
		"2026-7-29-13",
		"2026-07-29-3",
		"2026-02-30-13",
		"2026-07-29T13",
		"2026-07-29-13Z",
		"2026-07-29-14",
		"2026-07-30-00",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			_, parseErr := ParseReportingHourKey(value, now)
			assert.Error(t, parseErr)
		})
	}
}

func TestParseReportingDateAcceptsOnlyCanonicalUTCDates(t *testing.T) {
	got, err := ParseReportingDate("2026-07-29")
	require.NoError(t, err)

	assert.Equal(t, time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), got)

	invalid := []string{
		"",
		"2026-7-29",
		"2026-07-3",
		"2026-02-30",
		"2026-07-29Z",
		"2026-07-29T00:00:00Z",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			_, parseErr := ParseReportingDate(value)
			assert.Error(t, parseErr)
		})
	}
}

func TestFinalizeReportingHourCanonicalizesOrderingAndIgnoresInputDigest(t *testing.T) {
	hour := reportingHourFixture("2026-07-29-13")
	reversed := hour
	reversed.Digest = "sha256:stale-derived-field"
	slices.Reverse(reversed.Activity.Buckets)
	slices.Reverse(reversed.Activity.ByModel)
	slices.Reverse(reversed.Activity.ByAgent)
	slices.Reverse(reversed.Activity.ByProject)
	slices.Reverse(reversed.Usage.ByModel)
	slices.Reverse(reversed.Usage.ByAgent)
	slices.Reverse(reversed.Usage.ByProject)

	finalized, canonical, err := FinalizeReportingHour(hour)
	require.NoError(t, err)
	reversedFinalized, reversedCanonical, err := FinalizeReportingHour(reversed)
	require.NoError(t, err)

	assert.Equal(t, finalized, reversedFinalized)
	assert.Equal(t, canonical, reversedCanonical)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, finalized.Digest)
	assert.Equal(t, []string{"agent-a", "agent-z"}, []string{
		finalized.Activity.ByAgent[0].Key,
		finalized.Activity.ByAgent[1].Key,
	})
	assert.Equal(t, []string{"model-a", "model-z"}, []string{
		finalized.Usage.ByModel[0].Key,
		finalized.Usage.ByModel[1].Key,
	})

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(canonical, &decoded))
	assert.Equal(t, finalized.Digest, decoded["digest"])
}

func TestFinalizeReportingHourDigestVector(t *testing.T) {
	tests := []struct {
		name    string
		version int
		digest  string
	}{
		{
			name: "v1", version: ReportingLegacySchemaVersion,
			digest: "sha256:3b1d5c9c228f26da81e4acc08ccf50d11b4855fa11fdbaf67bfbc6a34b72e172",
		},
		{
			name: "v2", version: ReportingSchemaVersion,
			digest: "sha256:4c3dbbedc3bd6bcebfe194093ce77c4e85e85dd1b3ac6bd39e21b841b9ce993b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hour := quietReportingHourFixtureVersion("2026-07-28-00", tt.version)
			hour.Digest = "sha256:stale-derived-field"

			finalized, _, err := FinalizeReportingHour(hour)
			require.NoError(t, err)
			assert.Equal(t, tt.digest, finalized.Digest)

			hour.Digest = "sha256:different-stale-derived-field"
			repeated, _, err := FinalizeReportingHour(hour)
			require.NoError(t, err)
			assert.Equal(t, finalized.Digest, repeated.Digest)
		})
	}
}

func TestFinalizeReportingDayGivesCompletedEmptyDateCanonicalDigest(t *testing.T) {
	tests := []struct {
		name       string
		version    int
		dayDigest  string
		hourDigest string
	}{
		{
			name: "v1", version: ReportingLegacySchemaVersion,
			dayDigest:  "sha256:3e92051eeb2fa36ad03a30bbbf1a7769244ecd33c5dca3eddd3698ddc0cd71d3",
			hourDigest: "sha256:3b1d5c9c228f26da81e4acc08ccf50d11b4855fa11fdbaf67bfbc6a34b72e172",
		},
		{
			name: "v2", version: ReportingSchemaVersion,
			dayDigest:  "sha256:24fb5a2f40effe393c3c384087b4103811eabc85e029c1afd56b3f61afa8fe3e",
			hourDigest: "sha256:4c3dbbedc3bd6bcebfe194093ce77c4e85e85dd1b3ac6bd39e21b841b9ce993b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hours := make([]ReportingHour, 24)
			for i := range hours {
				hours[i] = quietReportingHourFixtureVersion(
					"2026-07-28-"+time.Date(
						0, 1, 1, i, 0, 0, 0, time.UTC,
					).Format("15"),
					tt.version,
				)
			}

			finalized, canonical, err := FinalizeReportingDay(ReportingDay{
				SchemaVersion: tt.version,
				Date:          "2026-07-28",
				Complete:      true,
				HasData:       true,
				Digest:        "sha256:stale-derived-field",
				Hours:         hours,
			})
			require.NoError(t, err)

			assert.False(t, finalized.HasData)
			assert.Equal(t, tt.dayDigest, finalized.Digest)
			require.Len(t, finalized.Hours, 24)
			assert.Equal(t, tt.hourDigest, finalized.Hours[0].Digest)
			assert.Contains(t, string(canonical), `"has_data":false`)
		})
	}
}

func reportingHourFixture(period string) ReportingHour {
	hourStart, err := time.Parse("2006-01-02-15", period)
	if err != nil {
		panic(err)
	}
	buckets := make([]ReportingActivityBucket, 12)
	for i := range buckets {
		buckets[i].Start = hourStart.Add(time.Duration(i) * 5 * time.Minute).
			Format(time.RFC3339)
	}
	return ReportingHour{
		SchemaVersion: ReportingSchemaVersion,
		Period:        period,
		HasData:       true,
		Activity: ReportingActivity{
			Buckets: buckets,
			ByModel: []ReportingActivityBreakdown{
				{Key: "model-z"},
				{Key: "model-a"},
			},
			ByAgent: []ReportingActivityBreakdown{
				{Key: "agent-z"},
				{Key: "agent-a"},
			},
			ByProject: []ReportingActivityProjectBreakdown{
				{Project: "project-z", ProjectKey: "/project-z"},
				{Project: "project-a", ProjectKey: "/project-a"},
			},
		},
		Usage: ReportingUsage{
			ByModel: []ReportingUsageBreakdown{
				{Key: "model-z"},
				{Key: "model-a"},
			},
			ByAgent: []ReportingUsageBreakdown{
				{Key: "agent-z"},
				{Key: "agent-a"},
			},
			ByProject: []ReportingUsageProjectBreakdown{
				{Project: "project-z", ProjectKey: "/project-z"},
				{Project: "project-a", ProjectKey: "/project-a"},
			},
		},
	}
}

func quietReportingHourFixtureVersion(period string, schemaVersion int) ReportingHour {
	hourStart, err := time.Parse("2006-01-02-15", period)
	if err != nil {
		panic(err)
	}
	buckets := make([]ReportingActivityBucket, 12)
	for i := range buckets {
		buckets[i].Start = hourStart.Add(time.Duration(i) * 5 * time.Minute).
			Format(time.RFC3339)
	}
	return ReportingHour{
		SchemaVersion: schemaVersion,
		Period:        period,
		Activity: ReportingActivity{
			Buckets: buckets,
		},
	}
}
