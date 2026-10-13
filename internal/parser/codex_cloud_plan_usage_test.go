package parser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// Plan usage record bodies as the fetcher writes them, with the server's
// field names.
const (
	ccRowOlder = `{"row":{"thread_id":"thread-a","data_status":"partial","usage_source":"wham",` +
		`"weekly_limit_percent":1.5,"five_hour_limit_percent":null,"balance_usage_credits":"0","groups":[]},` +
		`"data_as_of":"2026-10-01T10:00:00Z"}`
	ccRowNewer = `{"row":{"thread_id":"thread-a","data_status":"available","usage_source":"wham",` +
		`"weekly_limit_percent":2.25,"five_hour_limit_percent":12.5,"balance_usage_credits":"0E-10",` +
		`"groups":[{"model":"gpt-6","reasoning_effort":"high","speed":"standard","product_experience":"codex",` +
		`"weekly_limit_percent":2.25,"five_hour_limit_percent":12.5,"balance_usage_credits":"0E-10"}]},` +
		`"data_as_of":"2026-10-02T09:30:00Z","final":true}`
)

func ccOutcome(kind, at, root string) mirror.Record {
	body := `{"kind":"` + kind + `","at":"` + at + `"`
	if root != "" {
		body += `,"root":"` + root + `"`
	}
	return ccRecord(mirror.KindPlanUsageOutcome, body+`}`)
}

// ccNewerUsage is ccRowNewer as the parser should report it.
func ccNewerUsage() ParsedPlanUsage {
	return ParsedPlanUsage{
		DataAsOf:        new(time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)),
		DataStatus:      "available",
		UsageSource:     "wham",
		WeeklyPercent:   new(2.25),
		FiveHourPercent: new(12.5),
		Credits:         new("0E-10"),
		Groups: []ParsedPlanUsageGroup{{
			Model: "gpt-6", ReasoningEffort: "high", Speed: "standard", ProductExperience: "codex",
			WeeklyPercent: new(2.25), FiveHourPercent: new(12.5), Credits: new("0E-10"),
		}},
		Final: true,
	}
}

func TestPlanUsageNewestRowWins(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccRecord(mirror.KindPlanUsageRow, ccRowOlder),
		ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
	)
	result := parseCodexCloud(t, path)
	want := ccNewerUsage()
	assert.Equal(t, &want, result.Session.PlanUsage)
	assert.Empty(t, result.UsageEvents, "plan usage never becomes token usage")
}

func TestPlanUsageRowWithoutFiveHourLimit(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccRecord(mirror.KindPlanUsageRow, ccRowOlder),
	)
	result := parseCodexCloud(t, path)
	assert.Equal(t, &ParsedPlanUsage{
		DataAsOf:      new(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)),
		DataStatus:    "partial",
		UsageSource:   "wham",
		WeeklyPercent: new(1.5),
		Credits:       new("0"),
	}, result.Session.PlanUsage)
}

func TestStaleOutcomeAfterRow(t *testing.T) {
	outcomeAt := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	for _, kind := range []string{
		mirror.OutcomeLeftOut,
		mirror.OutcomeAccountUnavailable,
		mirror.OutcomeTooManyDescendants,
		mirror.OutcomeCouldNotCheck,
	} {
		t.Run(kind, func(t *testing.T) {
			path := writeCodexCloudMirror(t, "thread-a",
				ccRecord(mirror.KindThread, ccListing),
				ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
				ccOutcome(kind, "2026-10-03T08:00:00Z", ""),
			)
			want := ccNewerUsage()
			want.OutcomeKind = kind
			want.OutcomeAt = &outcomeAt
			assert.Equal(t, &want, parseCodexCloud(t, path).Session.PlanUsage,
				"the row stays and the newer outcome is shown beside it")
		})
		t.Run(kind+" then the row again", func(t *testing.T) {
			path := writeCodexCloudMirror(t, "thread-a",
				ccRecord(mirror.KindThread, ccListing),
				ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
				ccOutcome(kind, "2026-10-03T08:00:00Z", ""),
				ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
			)
			want := ccNewerUsage()
			assert.Equal(t, &want, parseCodexCloud(t, path).Session.PlanUsage,
				"an outcome older than the newest row is dropped")
		})
	}
}

// An outcome is ordered by its place in the file, not by its time: the
// outcome below carries an earlier time than the row's data_as_of but was
// written after it.
func TestOutcomeOrderedByPositionNotTime(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
		ccOutcome(mirror.OutcomeLeftOut, "2026-09-30T00:00:00Z", ""),
	)
	usage := parseCodexCloud(t, path).Session.PlanUsage
	require.NotNil(t, usage)
	assert.Equal(t, mirror.OutcomeLeftOut, usage.OutcomeKind)
	assert.Equal(t, new(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)), usage.OutcomeAt)
}

func TestCountedUnderClearsValues(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
		ccOutcome(mirror.OutcomeLeftOut, "2026-10-03T07:00:00Z", ""),
		ccOutcome(mirror.OutcomeCountedUnder, "2026-10-03T08:00:00Z", "thread-root"),
	)
	assert.Equal(t, &ParsedPlanUsage{
		OutcomeKind:  mirror.OutcomeCountedUnder,
		OutcomeAt:    new(time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)),
		CountedUnder: "codex-cloud:thread-root",
	}, parseCodexCloud(t, path).Session.PlanUsage)
}

func TestCountedUnderOlderThanRowIsDropped(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccOutcome(mirror.OutcomeCountedUnder, "2026-10-01T08:00:00Z", "thread-root"),
		ccRecord(mirror.KindPlanUsageRow, ccRowNewer),
	)
	want := ccNewerUsage()
	assert.Equal(t, &want, parseCodexCloud(t, path).Session.PlanUsage)
}

// The fetcher keeps outcomes only in the mirror file, so a full resync
// must rebuild them from it alone: each parse of a file with outcome
// records and no row reports the newest outcome.
func TestOutcomesSurviveReparse(t *testing.T) {
	for _, kind := range []string{
		mirror.OutcomeLeftOut,
		mirror.OutcomeSettledNoRow,
		mirror.OutcomeAccountUnavailable,
	} {
		t.Run(kind, func(t *testing.T) {
			path := writeCodexCloudMirror(t, "thread-a",
				ccRecord(mirror.KindThread, ccListing),
				ccOutcome(mirror.OutcomeCouldNotCheck, "2026-10-02T08:00:00Z", ""),
				ccOutcome(kind, "2026-10-03T08:00:00Z", ""),
			)
			want := &ParsedPlanUsage{
				OutcomeKind: kind,
				OutcomeAt:   new(time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)),
			}
			assert.Equal(t, want, parseCodexCloud(t, path).Session.PlanUsage)
			assert.Equal(t, want, parseCodexCloud(t, path).Session.PlanUsage, "second parse")
		})
	}
}

func TestNoPlanUsageRecords(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a", ccRecord(mirror.KindThread, ccListing))
	assert.Nil(t, parseCodexCloud(t, path).Session.PlanUsage)
}
