package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	require.NoError(t, err)
	return l
}

func mustRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts.UTC()
}

func TestResolveQuery_PresetDayUTC(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, now)
	require.NoError(t, err)
	assert.Equal(t, mustRFC3339(t, "2026-06-16T00:00:00Z"), q.RangeStart)
	assert.Equal(t, mustRFC3339(t, "2026-06-17T00:00:00Z"), q.RangeEnd)
	assert.Equal(t, BucketMinute, q.Bucket.Unit)
	assert.Equal(t, 300, q.Bucket.NominalSeconds)
	assert.False(t, q.Partial, "past day is complete")
	assert.Equal(t, q.RangeEnd, q.EffectiveEnd, "complete day clamps to range_end")
	assert.InDelta(t, 300.0, q.GapCapSeconds, 0)
}

func TestResolveQuery_EmptyInputDefaultsToToday(t *testing.T) {
	now := mustRFC3339(t, "2026-06-17T09:30:00Z")
	q, err := ResolveQuery(QueryInput{}, now)
	require.NoError(t, err)
	assert.Equal(t, mustRFC3339(t, "2026-06-17T00:00:00Z"), q.RangeStart)
	assert.Equal(t, mustRFC3339(t, "2026-06-18T00:00:00Z"), q.RangeEnd)
	assert.Equal(t, BucketMinute, q.Bucket.Unit, "empty input defaults to the day preset")
}

func TestResolveQuery_PresetWeekISOMonday(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	// 2026-06-17 is a Wednesday; ISO week starts Monday 2026-06-15.
	q, err := ResolveQuery(QueryInput{Preset: "week", Date: "2026-06-17", Timezone: "UTC"}, now)
	require.NoError(t, err)
	assert.Equal(t, mustRFC3339(t, "2026-06-15T00:00:00Z"), q.RangeStart)
	assert.Equal(t, mustRFC3339(t, "2026-06-22T00:00:00Z"), q.RangeEnd)
	assert.Equal(t, BucketHour, q.Bucket.Unit, "7d range auto-buckets hourly")
}

func TestResolveQuery_PresetMonthUTC(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	q, err := ResolveQuery(QueryInput{Preset: "month", Date: "2026-02-14", Timezone: "UTC"}, now)
	require.NoError(t, err)
	assert.Equal(t, mustRFC3339(t, "2026-02-01T00:00:00Z"), q.RangeStart)
	assert.Equal(t, mustRFC3339(t, "2026-03-01T00:00:00Z"), q.RangeEnd)
	assert.Equal(t, BucketDay, q.Bucket.Unit, "28d range auto-buckets daily")
}

func TestResolveQuery_PresetDayDSTSpringForward(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	// America/New_York springs forward 2026-03-08: local day is 23h.
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-03-08", Timezone: "America/New_York"}, now)
	require.NoError(t, err)
	assert.Equal(t, 23*time.Hour, q.RangeEnd.Sub(q.RangeStart), "spring-forward day is 23h")
}

func TestResolveQuery_PresetDayDSTFallBack(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	// America/New_York falls back 2026-11-01: local day is 25h.
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-11-01", Timezone: "America/New_York"}, now)
	require.NoError(t, err)
	assert.Equal(t, 25*time.Hour, q.RangeEnd.Sub(q.RangeStart), "fall-back day is 25h")
}

func TestResolveQuery_MonthSpanningDST(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	// March 2026 in New York spans the spring-forward; month is 31 calendar
	// days but one hour short of 31*24h.
	q, err := ResolveQuery(QueryInput{Preset: "month", Date: "2026-03-10", Timezone: "America/New_York"}, now)
	require.NoError(t, err)
	assert.Equal(t, 31*24*time.Hour-time.Hour, q.RangeEnd.Sub(q.RangeStart),
		"March in NY is 31 days minus the lost DST hour")
}

func TestResolveBucket_Thresholds(t *testing.T) {
	l := time.UTC
	start := mustRFC3339(t, "2026-06-01T00:00:00Z")
	cases := []struct {
		name string
		dur  time.Duration
		unit BucketUnit
		secs int
	}{
		{"exactly 36h is minute", 36 * time.Hour, BucketMinute, 300},
		{"just over 36h is hour", 36*time.Hour + time.Second, BucketHour, 3600},
		{"exactly 14d is hour", 14 * 24 * time.Hour, BucketHour, 3600},
		{"just over 14d is day", 14*24*time.Hour + time.Second, BucketDay, 86400},
		{"exactly 90d is day", 90 * 24 * time.Hour, BucketDay, 86400},
		{"just over 90d is week", 90*24*time.Hour + time.Second, BucketWeek, 604800},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ResolveBucket(start, start.Add(tc.dur), "", l)
			require.NoError(t, err)
			assert.Equal(t, tc.unit, spec.Unit)
			assert.Equal(t, tc.secs, spec.NominalSeconds)
		})
	}
}

func TestResolveBucket_OverrideAllowList(t *testing.T) {
	l := time.UTC
	start := mustRFC3339(t, "2026-06-01T00:00:00Z")
	end := start.Add(48 * time.Hour)
	ok := map[string]BucketSpec{
		"5m":  {BucketMinute, 300},
		"15m": {BucketMinute, 900},
		"1h":  {BucketHour, 3600},
		"1d":  {BucketDay, 86400},
		"1w":  {BucketWeek, 604800},
	}
	for k, want := range ok {
		spec, err := ResolveBucket(start, end, k, l)
		require.NoErrorf(t, err, "override %q", k)
		assert.Equalf(t, want, spec, "override %q", k)
	}
	_, err := ResolveBucket(start, end, "2h", l)
	require.Error(t, err, "off-allow-list override is rejected")
}

func TestResolveQuery_BucketCountCap(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	// 5m buckets over a full year is ~105k buckets > 2000.
	_, err := ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-01-01T00:00:00Z", To: "2026-12-31T00:00:00Z",
		BucketOverride: "5m",
	}, now)
	require.Error(t, err, "5m over a year exceeds the 2000-bucket cap")
}

func TestResolveQuery_MaxRangeOneYear(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	_, err := ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-01-01T00:00:00Z", To: "2027-01-02T00:00:00Z",
	}, now)
	require.Error(t, err, "range over one year is rejected")
}

func TestResolveQuery_FromAfterToRejected(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	_, err := ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-06-02T00:00:00Z", To: "2026-06-01T00:00:00Z",
	}, now)
	require.Error(t, err, "from >= to is rejected")
	_, err = ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-06-01T00:00:00Z", To: "2026-06-01T00:00:00Z",
	}, now)
	require.Error(t, err, "zero-length range is rejected")
}

func TestResolveQuery_CustomRequiresBothBounds(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	_, err := ResolveQuery(QueryInput{Preset: "custom", Timezone: "UTC", From: "2026-06-01T00:00:00Z"}, now)
	require.Error(t, err, "custom needs both from and to")
}

func TestResolveQuery_InvalidTimezone(t *testing.T) {
	now := mustRFC3339(t, "2030-01-01T00:00:00Z")
	_, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "Fake/Zone"}, now)
	require.Error(t, err)
}

func TestResolveQuery_FutureRangeClamps(t *testing.T) {
	now := mustRFC3339(t, "2026-06-16T00:00:00Z")
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-06-20", Timezone: "UTC"}, now)
	require.NoError(t, err)
	assert.Equal(t, q.RangeStart, q.EffectiveEnd, "fully future range clamps effective_end to range_start")
	assert.True(t, q.Partial, "future range is partial")
}

func TestBuildBuckets_FixedDurationWithShortFinal(t *testing.T) {
	l := time.UTC
	start := mustRFC3339(t, "2026-06-16T10:00:00Z")
	end := mustRFC3339(t, "2026-06-16T10:12:00Z") // 12 minutes
	ws, err := BuildBuckets(start, end, BucketSpec{BucketMinute, 300}, l)
	require.NoError(t, err)
	require.Len(t, ws, 3, "12 minutes / 5 = 2 full + 1 short bucket")
	assert.Equal(t, start, ws[0].Start)
	assert.Equal(t, mustRFC3339(t, "2026-06-16T10:05:00Z"), ws[0].End)
	assert.Equal(t, mustRFC3339(t, "2026-06-16T10:10:00Z"), ws[2].Start)
	assert.Equal(t, end, ws[2].End, "final bucket clipped to range end")
}

func TestBuildBuckets_CalendarDayLocalBoundaries(t *testing.T) {
	l := loc(t, "America/New_York")
	// Two local days; the boundary is local midnight (04:00Z in EDT).
	start := mustRFC3339(t, "2026-06-16T04:00:00Z")
	end := mustRFC3339(t, "2026-06-18T04:00:00Z")
	ws, err := BuildBuckets(start, end, BucketSpec{BucketDay, 86400}, l)
	require.NoError(t, err)
	require.Len(t, ws, 2)
	assert.Equal(t, start, ws[0].Start)
	assert.Equal(t, mustRFC3339(t, "2026-06-17T04:00:00Z"), ws[0].End,
		"calendar day boundary is local midnight")
	assert.Equal(t, end, ws[1].End)
}

func TestBuildBuckets_CalendarDaySpansDST(t *testing.T) {
	l := loc(t, "America/New_York")
	// Spring forward 2026-03-08 (23h local day) then a normal 24h day.
	start := mustRFC3339(t, "2026-03-08T05:00:00Z") // 2026-03-08 00:00 EST
	end := mustRFC3339(t, "2026-03-10T04:00:00Z")   // 2026-03-10 00:00 EDT
	ws, err := BuildBuckets(start, end, BucketSpec{BucketDay, 86400}, l)
	require.NoError(t, err)
	require.Len(t, ws, 2)
	assert.Equal(t, 23*time.Hour, ws[0].End.Sub(ws[0].Start), "spring-forward calendar day is 23h")
	assert.Equal(t, 24*time.Hour, ws[1].End.Sub(ws[1].Start), "following day is 24h")
}

func TestBuildBuckets_CalendarDayFallBack25h(t *testing.T) {
	l := loc(t, "America/New_York")
	start := mustRFC3339(t, "2026-11-01T04:00:00Z") // 2026-11-01 00:00 EDT
	end := mustRFC3339(t, "2026-11-02T05:00:00Z")   // 2026-11-02 00:00 EST
	ws, err := BuildBuckets(start, end, BucketSpec{BucketDay, 86400}, l)
	require.NoError(t, err)
	require.Len(t, ws, 1)
	assert.Equal(t, 25*time.Hour, ws[0].End.Sub(ws[0].Start), "fall-back calendar day is 25h")
}

func TestBuildBuckets_CalendarWeekISOMonday(t *testing.T) {
	l := time.UTC
	start := mustRFC3339(t, "2026-06-15T00:00:00Z") // Monday
	end := mustRFC3339(t, "2026-06-29T00:00:00Z")   // two ISO weeks later
	ws, err := BuildBuckets(start, end, BucketSpec{BucketWeek, 604800}, l)
	require.NoError(t, err)
	require.Len(t, ws, 2)
	assert.Equal(t, mustRFC3339(t, "2026-06-22T00:00:00Z"), ws[0].End)
}
