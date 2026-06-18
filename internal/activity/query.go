package activity

import (
	"fmt"
	"time"
)

// BucketUnit names the calendar/clock unit a timeline bucket spans.
type BucketUnit string

const (
	BucketMinute BucketUnit = "minute"
	BucketHour   BucketUnit = "hour"
	BucketDay    BucketUnit = "day"
	BucketWeek   BucketUnit = "week"
)

// BucketSpec is a resolved bucket size: its unit plus the nominal number of
// seconds one bucket spans (calendar buckets may deviate per DST).
type BucketSpec struct {
	Unit           BucketUnit
	NominalSeconds int
}

// QueryInput is the raw, request-level description of a range to report on.
type QueryInput struct {
	Preset         string // "day" | "week" | "month" | "custom" | ""
	Date           string // YYYY-MM-DD for presets
	From           string // RFC3339 for custom ranges
	To             string // RFC3339 for custom ranges
	Timezone       string
	BucketOverride string // "", "5m", "15m", "1h", "1d", "1w"
}

// Query is a fully resolved range: its timezone, UTC bounds, the effective end
// after clamping to now, the selected bucket size, and the idle-gap cap.
type Query struct {
	Timezone      string
	Loc           *time.Location
	RangeStart    time.Time
	RangeEnd      time.Time
	EffectiveEnd  time.Time
	Partial       bool
	Bucket        BucketSpec
	GapCapSeconds float64
}

// BucketWindow is a half-open [Start, End) timeline bucket; bounds are UTC.
type BucketWindow struct {
	Start time.Time
	End   time.Time
}

const (
	maxRange      = 365 * 24 * time.Hour
	maxBuckets    = 2000
	defaultGapCap = 300.0
)

// ResolveQuery turns a raw QueryInput into a fully resolved Query, expanding
// presets in the requested timezone, validating the range, clamping the
// effective end to now, and selecting (and bounding) the bucket size.
func ResolveQuery(input QueryInput, now time.Time) (Query, error) {
	loc, err := loadLocation(input.Timezone)
	if err != nil {
		return Query{}, err
	}
	start, end, err := resolveRange(input, loc, now)
	if err != nil {
		return Query{}, err
	}
	if !start.Before(end) {
		return Query{}, fmt.Errorf("from must be before to")
	}
	if end.Sub(start) > maxRange {
		return Query{}, fmt.Errorf("range exceeds maximum of one year")
	}
	bucket, err := ResolveBucket(start, end, input.BucketOverride, loc)
	if err != nil {
		return Query{}, err
	}
	windows, err := BuildBuckets(start, end, bucket, loc)
	if err != nil {
		return Query{}, err
	}
	if len(windows) > maxBuckets {
		return Query{}, fmt.Errorf("bucket configuration would produce too many buckets")
	}
	nowUTC := now.UTC()
	return Query{
		Timezone:      input.Timezone,
		Loc:           loc,
		RangeStart:    start,
		RangeEnd:      end,
		EffectiveEnd:  maxTime(start, minTime(end, nowUTC)),
		Partial:       nowUTC.Before(end),
		Bucket:        bucket,
		GapCapSeconds: defaultGapCap,
	}, nil
}

// loadLocation resolves an IANA timezone name; empty and "UTC" map to time.UTC.
func loadLocation(tz string) (*time.Location, error) {
	if tz == "" || tz == "UTC" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone: %s", tz)
	}
	return loc, nil
}

// resolveRange picks the UTC [start, end) bounds for the input, expanding a
// preset in loc or parsing the custom From/To instants. An empty request
// defaults to the day preset, and a preset with an empty Date defaults to
// today in loc, so ResolveQuery(QueryInput{}, now) resolves the current day.
func resolveRange(
	input QueryInput, loc *time.Location, now time.Time,
) (time.Time, time.Time, error) {
	preset := input.Preset
	if preset == "" && input.From == "" && input.To == "" {
		preset = "day"
	}
	if preset == "custom" {
		return parseCustomRange(input)
	}
	date := input.Date
	if date == "" {
		date = now.In(loc).Format("2006-01-02")
	}
	return expandPreset(preset, date, loc)
}

// parseCustomRange parses the RFC3339 From/To bounds; both are required.
func parseCustomRange(input QueryInput) (time.Time, time.Time, error) {
	if input.From == "" || input.To == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("custom range requires both from and to")
	}
	from, err := time.Parse(time.RFC3339, input.From)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid from: %s", input.From)
	}
	to, err := time.Parse(time.RFC3339, input.To)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid to: %s", input.To)
	}
	return from.UTC(), to.UTC(), nil
}

// expandPreset expands a day/week/month preset anchored at Date into UTC
// bounds, walking calendar units in loc so DST shifts are reflected.
func expandPreset(preset, date string, loc *time.Location) (time.Time, time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid date: %s", date)
	}
	switch preset {
	case "day":
		return d.UTC(), d.AddDate(0, 0, 1).UTC(), nil
	case "week":
		monday := d.AddDate(0, 0, -isoMondayOffset(d.Weekday()))
		return monday.UTC(), monday.AddDate(0, 0, 7).UTC(), nil
	case "month":
		first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, loc)
		return first.UTC(), first.AddDate(0, 1, 0).UTC(), nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("invalid preset: %s", preset)
	}
}

// isoMondayOffset is the number of days from the ISO Monday of the week
// containing wd to wd itself (Mon=0, Tue=1, ..., Sun=6).
func isoMondayOffset(wd time.Weekday) int {
	return (int(wd) + 6) % 7
}

// ResolveBucket selects a bucket size for [start, end). An empty override
// applies the duration-based auto policy; otherwise the allow-list is consulted.
func ResolveBucket(start, end time.Time, override string, _ *time.Location) (BucketSpec, error) {
	if override != "" {
		return bucketFromOverride(override)
	}
	switch d := end.Sub(start); {
	case d <= 36*time.Hour:
		return BucketSpec{BucketMinute, 300}, nil
	case d <= 14*24*time.Hour:
		return BucketSpec{BucketHour, 3600}, nil
	case d <= 90*24*time.Hour:
		return BucketSpec{BucketDay, 86400}, nil
	default:
		return BucketSpec{BucketWeek, 604800}, nil
	}
}

// bucketFromOverride maps an allow-listed override string to a BucketSpec.
func bucketFromOverride(override string) (BucketSpec, error) {
	switch override {
	case "5m":
		return BucketSpec{BucketMinute, 300}, nil
	case "15m":
		return BucketSpec{BucketMinute, 900}, nil
	case "1h":
		return BucketSpec{BucketHour, 3600}, nil
	case "1d":
		return BucketSpec{BucketDay, 86400}, nil
	case "1w":
		return BucketSpec{BucketWeek, 604800}, nil
	default:
		return BucketSpec{}, fmt.Errorf("invalid bucket: %s", override)
	}
}

// BuildBuckets tiles [start, end) into half-open UTC windows for spec. Minute
// and hour buckets are fixed-duration; day and week buckets follow local
// calendar boundaries in loc so they absorb DST shifts.
func BuildBuckets(start, end time.Time, spec BucketSpec, loc *time.Location) ([]BucketWindow, error) {
	switch spec.Unit {
	case BucketMinute, BucketHour:
		return fixedBuckets(start, end, time.Duration(spec.NominalSeconds)*time.Second), nil
	case BucketDay:
		return calendarBuckets(start, end, loc, 1), nil
	case BucketWeek:
		return calendarBuckets(start, end, loc, 7), nil
	default:
		return nil, fmt.Errorf("invalid bucket unit: %s", spec.Unit)
	}
}

// fixedBuckets tiles [start, end) into windows of a constant duration anchored
// at start; the final window is clipped to end and may be shorter than step.
func fixedBuckets(start, end time.Time, step time.Duration) []BucketWindow {
	if step <= 0 || !start.Before(end) {
		return nil
	}
	var out []BucketWindow
	for bStart := start; bStart.Before(end); bStart = bStart.Add(step) {
		bEnd := minTime(bStart.Add(step), end)
		out = append(out, BucketWindow{Start: bStart, End: bEnd})
	}
	return out
}

// calendarBuckets tiles [start, end) into local calendar windows of stepDays
// days each, anchored at the local midnight (ISO Monday for weeks) at or before
// start. The first Start is clipped up to start and the last End down to end.
func calendarBuckets(start, end time.Time, loc *time.Location, stepDays int) []BucketWindow {
	if !start.Before(end) {
		return nil
	}
	local := localMidnight(start, loc)
	if stepDays == 7 {
		local = local.AddDate(0, 0, -isoMondayOffset(local.Weekday()))
	}
	var out []BucketWindow
	for {
		next := local.AddDate(0, 0, stepDays)
		bStart := maxTime(local.UTC(), start)
		bEnd := minTime(next.UTC(), end)
		if bEnd.After(bStart) {
			out = append(out, BucketWindow{Start: bStart, End: bEnd})
		}
		if !next.UTC().Before(end) {
			break
		}
		local = next
	}
	return out
}

// localMidnight returns the local 00:00 wall time on the calendar day that
// contains t in loc.
func localMidnight(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}
