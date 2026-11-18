package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/stringutil"
)

func TestPGActivityInputBytesIncludesPrices(t *testing.T) {
	raw := pgActivityInputs{}
	withPrices := pgActivityInputs{priced: &pgActivityPriced{rows: make([]pgActivityPricedRow, 2), block: &export.PricingBlock{}}}
	assert.Greater(t, pgActivityInputBytes(withPrices), pgActivityInputBytes(raw))
}

func TestPGActivityPricedCovers(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	effective := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	next := effective.Add(time.Hour)
	priced := pgActivityPriced{rangeStart: start, rangeEnd: end, effectiveEnd: effective, next: next, pricingDigest: "prices"}
	for _, tc := range []struct {
		name   string
		query  activity.Query
		digest string
		want   bool
	}{
		{"same", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective}, "prices", true},
		{"rewind within a microsecond", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective.Add(-time.Nanosecond)}, "prices", false},
		{"start changes", activity.Query{RangeStart: start.Add(time.Microsecond), RangeEnd: end, EffectiveEnd: effective}, "prices", false},
		{"end changes", activity.Query{RangeStart: start, RangeEnd: end.Add(time.Microsecond), EffectiveEnd: effective}, "prices", false},
		{"prices change", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective}, "new prices", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, priced.covers(tc.query, tc.digest))
		})
	}
	priced.next = time.Time{}
	assert.True(t, priced.covers(activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: end}, "prices"))
}

func TestPGActivityDiskRowRoundTrip(t *testing.T) {
	intern := stringutil.Interner{}
	row := pgActivityUsageRow{
		sessionID: intern.Intern("session"), usageSource: intern.Intern("source"), model: intern.Intern("model"), providerID: intern.Intern("provider"), agent: intern.Intern("agent"),
		claudeMessageID: "message", claudeRequestID: "request", sourceUUID: "uuid", usageDedupKey: "dedup",
		us: 1, cost: 2, webSearches: 3, input: 4, output: 5, cacheCreate: 6, cacheRead: 7, reasoning: 8,
		ordinal: 9, cacheCreate1h: 10, ordinalValid: true, costValid: true, copilotReported: true, pricingTimeAbsent: true, costRecorded: true,
	}
	assert.Equal(t, row, pgActivityRowToDisk(row).activityRow(stringutil.Interner{}))
}

func TestPGActivityDiskDropsPricesWhenOverLimit(t *testing.T) {
	rows := []kept.Part[pgActivityDiskPart]{
		{Value: pgActivityDiskPart{Identity: "source", Usage: []pgActivityDiskRow{{SessionID: "session", Model: "model"}}, Priced: &pgActivityDiskPriced{Rows: []pgActivityDiskPricedRow{{Attribution: "model", Index: 0}}}}},
		{ID: "session", Value: pgActivityDiskPart{Bounds: []int64{1, 2}, Runs: []activity.SessionRunPiece{{Model: "model", Last: 1}}, Candidate: true}},
	}
	data, err := encodePGActivityDiskRows(rows, 60)
	require.NoError(t, err)
	decoded, ok := kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
	require.True(t, ok)
	require.Len(t, decoded, 2)
	assert.Nil(t, decoded[0].Value.Priced)
	assert.Equal(t, rows[0].Value.Usage, decoded[0].Value.Usage)
	assert.Equal(t, rows[1], decoded[1])
	data, err = encodePGActivityDiskRows(rows, 1000)
	require.NoError(t, err)
	decoded, ok = kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
	require.True(t, ok)
	assert.Equal(t, rows[0].Value.Priced, decoded[0].Value.Priced)
	_, err = encodePGActivityDiskRows(rows, 1)
	require.Error(t, err)
}
