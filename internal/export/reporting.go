package export

import (
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

const (
	reportingHourLayout = "2006-01-02-15"
	reportingDateLayout = "2006-01-02"

	// ReportingSchemaVersion is the wire version for hour, day, and digest
	// exports consumed by downstream integrations.
	ReportingSchemaVersion = 1
)

// ReportingHour is one immutable UTC-hour export. Digest identifies the
// canonical document body with the derived Digest field omitted.
type ReportingHour struct {
	SchemaVersion int               `json:"schema_version"`
	Period        string            `json:"period"`
	Digest        string            `json:"digest"`
	HasData       bool              `json:"has_data"`
	Activity      ReportingActivity `json:"activity"`
	Usage         ReportingUsage    `json:"usage"`
}

// ReportingDay is one UTC date exported from a single read snapshot. A
// completed date always has 24 hours and a digest, including a date with no
// observed activity or usage.
type ReportingDay struct {
	SchemaVersion int             `json:"schema_version"`
	Date          string          `json:"date"`
	Complete      bool            `json:"complete"`
	HasData       bool            `json:"has_data"`
	Digest        string          `json:"digest,omitempty"`
	Hours         []ReportingHour `json:"hours"`
}

// ReportingDigest is the compact date-range response used to screen for
// changed completed days without transferring hour document bodies.
type ReportingDigest struct {
	SchemaVersion int                  `json:"schema_version"`
	From          string               `json:"from"`
	To            string               `json:"to"`
	Days          []ReportingDigestDay `json:"days"`
}

// ReportingDigestDay carries one ordered set of closed-hour digests. DayDigest
// is present only when all 24 UTC hours have closed.
type ReportingDigestDay struct {
	Date        string   `json:"date"`
	Complete    bool     `json:"complete"`
	HasData     bool     `json:"has_data"`
	DayDigest   string   `json:"day_digest,omitempty"`
	HourDigests []string `json:"hour_digests"`
}

type ReportingActivity struct {
	Totals    ReportingActivityTotals             `json:"totals"`
	Peak      ReportingActivityPeak               `json:"peak"`
	Buckets   []ReportingActivityBucket           `json:"buckets"`
	ByModel   []ReportingActivityBreakdown        `json:"by_model"`
	ByAgent   []ReportingActivityBreakdown        `json:"by_agent"`
	ByProject []ReportingActivityProjectBreakdown `json:"by_project"`
}

type ReportingActivityTotals struct {
	ActiveMinutes           float64     `json:"active_minutes"`
	IdleMinutes             float64     `json:"idle_minutes"`
	AgentMinutes            float64     `json:"agent_minutes"`
	AutomatedAgentMinutes   float64     `json:"automated_agent_minutes"`
	InteractiveAgentMinutes float64     `json:"interactive_agent_minutes"`
	OutputTokens            int64       `json:"output_tokens"`
	Cost                    money.Money `json:"cost"`
	AutomatedCost           money.Money `json:"automated_cost"`
	InteractiveCost         money.Money `json:"interactive_cost"`
	NewSessions             int         `json:"new_sessions"`
	NewAutomatedSessions    int         `json:"new_automated_sessions"`
	NewInteractiveSessions  int         `json:"new_interactive_sessions"`
	NewUntimedSessions      int         `json:"new_untimed_sessions"`
	NewProjects             int         `json:"new_projects"`
	NewModels               int         `json:"new_models"`
}

type ReportingActivityPeak struct {
	Agents int     `json:"agents"`
	At     *string `json:"at"`
}

type ReportingActivityBucket struct {
	Start             string      `json:"start"`
	AgentMinutes      float64     `json:"agent_minutes"`
	MaxAgents         int         `json:"max_agents"`
	OutputTokens      int64       `json:"output_tokens"`
	Cost              money.Money `json:"cost"`
	AutomatedAtPeak   int         `json:"automated_at_peak"`
	InteractiveAtPeak int         `json:"interactive_at_peak"`
}

type ReportingActivityBreakdown struct {
	Key                     string      `json:"key"`
	AgentMinutes            float64     `json:"agent_minutes"`
	AutomatedAgentMinutes   float64     `json:"automated_agent_minutes"`
	InteractiveAgentMinutes float64     `json:"interactive_agent_minutes"`
	Cost                    money.Money `json:"cost"`
	AutomatedCost           money.Money `json:"automated_cost"`
	InteractiveCost         money.Money `json:"interactive_cost"`
}

type ReportingActivityProjectBreakdown struct {
	Project                 string      `json:"project"`
	ProjectKey              string      `json:"project_key"`
	AgentMinutes            float64     `json:"agent_minutes"`
	AutomatedAgentMinutes   float64     `json:"automated_agent_minutes"`
	InteractiveAgentMinutes float64     `json:"interactive_agent_minutes"`
	Cost                    money.Money `json:"cost"`
	AutomatedCost           money.Money `json:"automated_cost"`
	InteractiveCost         money.Money `json:"interactive_cost"`
}

type ReportingUsage struct {
	Totals    ReportingUsageTotals             `json:"totals"`
	ByModel   []ReportingUsageBreakdown        `json:"by_model"`
	ByAgent   []ReportingUsageBreakdown        `json:"by_agent"`
	ByProject []ReportingUsageProjectBreakdown `json:"by_project"`
}

type ReportingUsageTotals struct {
	InputTokens         int64       `json:"input_tokens"`
	OutputTokens        int64       `json:"output_tokens"`
	CacheCreationTokens int64       `json:"cache_creation_tokens"`
	CacheReadTokens     int64       `json:"cache_read_tokens"`
	Cost                money.Money `json:"cost"`
}

type ReportingUsageBreakdown struct {
	Key                 string      `json:"key"`
	InputTokens         int64       `json:"input_tokens"`
	OutputTokens        int64       `json:"output_tokens"`
	CacheCreationTokens int64       `json:"cache_creation_tokens"`
	CacheReadTokens     int64       `json:"cache_read_tokens"`
	Cost                money.Money `json:"cost"`
}

type ReportingUsageProjectBreakdown struct {
	Project             string      `json:"project"`
	ProjectKey          string      `json:"project_key"`
	InputTokens         int64       `json:"input_tokens"`
	OutputTokens        int64       `json:"output_tokens"`
	CacheCreationTokens int64       `json:"cache_creation_tokens"`
	CacheReadTokens     int64       `json:"cache_read_tokens"`
	Cost                money.Money `json:"cost"`
}

// ParseReportingHourKey parses a canonical UTC hour key and rejects hours
// which have not closed yet.
func ParseReportingHourKey(value string, now time.Time) (time.Time, error) {
	hour, err := time.Parse(reportingHourLayout, value)
	if err != nil || hour.Format(reportingHourLayout) != value {
		return time.Time{}, fmt.Errorf("invalid reporting hour %q", value)
	}
	if !hour.Before(now.UTC().Truncate(time.Hour)) {
		return time.Time{}, fmt.Errorf("reporting hour %q has not closed", value)
	}
	return hour, nil
}

// ParseReportingDate parses one canonical UTC calendar date.
func ParseReportingDate(value string) (time.Time, error) {
	date, err := time.Parse(reportingDateLayout, value)
	if err != nil || date.Format(reportingDateLayout) != value {
		return time.Time{}, fmt.Errorf("invalid reporting date %q", value)
	}
	return date, nil
}

// FinalizeReportingHour returns a normalized copy, its content-derived
// digest, and its canonical JSON bytes.
func FinalizeReportingHour(hour ReportingHour) (ReportingHour, []byte, error) {
	if hour.SchemaVersion != ReportingSchemaVersion {
		return ReportingHour{}, nil, fmt.Errorf(
			"unsupported reporting schema version %d", hour.SchemaVersion,
		)
	}
	hourStart, err := parseReportingHour(hour.Period)
	if err != nil {
		return ReportingHour{}, nil, err
	}
	hour = normalizeReportingHour(hour)
	if err := validateReportingBuckets(hourStart, hour.Activity.Buckets); err != nil {
		return ReportingHour{}, nil, err
	}

	digest, err := DigestCanonical(reportingHourDigestInput{
		SchemaVersion: hour.SchemaVersion,
		Period:        hour.Period,
		HasData:       hour.HasData,
		Activity:      hour.Activity,
		Usage:         hour.Usage,
	})
	if err != nil {
		return ReportingHour{}, nil, fmt.Errorf("digest reporting hour: %w", err)
	}
	hour.Digest = digest
	canonical, err := MarshalCanonical(hour)
	if err != nil {
		return ReportingHour{}, nil, fmt.Errorf("marshal reporting hour: %w", err)
	}
	return hour, canonical, nil
}

// FinalizeReportingDay normalizes and finalizes every hour, then derives the
// completed-day digest from the ordered hour digests.
func FinalizeReportingDay(day ReportingDay) (ReportingDay, []byte, error) {
	if day.SchemaVersion != ReportingSchemaVersion {
		return ReportingDay{}, nil, fmt.Errorf(
			"unsupported reporting schema version %d", day.SchemaVersion,
		)
	}
	if _, err := ParseReportingDate(day.Date); err != nil {
		return ReportingDay{}, nil, err
	}

	hours := cloneOrEmpty(day.Hours)
	sort.SliceStable(hours, func(i, j int) bool {
		return hours[i].Period < hours[j].Period
	})
	if day.Complete && len(hours) != 24 {
		return ReportingDay{}, nil, fmt.Errorf(
			"completed reporting date requires 24 hours, got %d", len(hours),
		)
	}
	if len(hours) > 24 {
		return ReportingDay{}, nil, fmt.Errorf(
			"reporting date has %d hours, maximum is 24", len(hours),
		)
	}

	digests := make([]string, len(hours))
	hasData := false
	for i := range hours {
		wantPeriod := fmt.Sprintf("%s-%02d", day.Date, i)
		if hours[i].Period != wantPeriod {
			return ReportingDay{}, nil, fmt.Errorf(
				"reporting date hour %d is %q, want %q",
				i, hours[i].Period, wantPeriod,
			)
		}
		finalized, _, err := FinalizeReportingHour(hours[i])
		if err != nil {
			return ReportingDay{}, nil, fmt.Errorf(
				"finalize reporting hour %q: %w", hours[i].Period, err,
			)
		}
		hours[i] = finalized
		digests[i] = finalized.Digest
		hasData = hasData || finalized.HasData
	}

	day.Hours = hours
	day.HasData = hasData
	day.Digest = ""
	if day.Complete {
		digest, err := DigestCanonical(digests)
		if err != nil {
			return ReportingDay{}, nil, fmt.Errorf("digest reporting date: %w", err)
		}
		day.Digest = digest
	}
	canonical, err := MarshalCanonical(day)
	if err != nil {
		return ReportingDay{}, nil, fmt.Errorf("marshal reporting date: %w", err)
	}
	return day, canonical, nil
}

type reportingHourDigestInput struct {
	SchemaVersion int               `json:"schema_version"`
	Period        string            `json:"period"`
	HasData       bool              `json:"has_data"`
	Activity      ReportingActivity `json:"activity"`
	Usage         ReportingUsage    `json:"usage"`
}

func parseReportingHour(value string) (time.Time, error) {
	hour, err := time.Parse(reportingHourLayout, value)
	if err != nil || hour.Format(reportingHourLayout) != value {
		return time.Time{}, fmt.Errorf("invalid reporting hour %q", value)
	}
	return hour, nil
}

func normalizeReportingHour(hour ReportingHour) ReportingHour {
	hour.Activity.Buckets = cloneOrEmpty(hour.Activity.Buckets)
	hour.Activity.ByModel = cloneOrEmpty(hour.Activity.ByModel)
	hour.Activity.ByAgent = cloneOrEmpty(hour.Activity.ByAgent)
	hour.Activity.ByProject = cloneOrEmpty(hour.Activity.ByProject)
	hour.Usage.ByModel = cloneOrEmpty(hour.Usage.ByModel)
	hour.Usage.ByAgent = cloneOrEmpty(hour.Usage.ByAgent)
	hour.Usage.ByProject = cloneOrEmpty(hour.Usage.ByProject)

	sort.SliceStable(hour.Activity.Buckets, func(i, j int) bool {
		return hour.Activity.Buckets[i].Start < hour.Activity.Buckets[j].Start
	})
	sort.SliceStable(hour.Activity.ByModel, func(i, j int) bool {
		return hour.Activity.ByModel[i].Key < hour.Activity.ByModel[j].Key
	})
	sort.SliceStable(hour.Activity.ByAgent, func(i, j int) bool {
		return hour.Activity.ByAgent[i].Key < hour.Activity.ByAgent[j].Key
	})
	sort.SliceStable(hour.Activity.ByProject, func(i, j int) bool {
		return reportingProjectLess(
			hour.Activity.ByProject[i].ProjectKey,
			hour.Activity.ByProject[i].Project,
			hour.Activity.ByProject[j].ProjectKey,
			hour.Activity.ByProject[j].Project,
		)
	})
	sort.SliceStable(hour.Usage.ByModel, func(i, j int) bool {
		return hour.Usage.ByModel[i].Key < hour.Usage.ByModel[j].Key
	})
	sort.SliceStable(hour.Usage.ByAgent, func(i, j int) bool {
		return hour.Usage.ByAgent[i].Key < hour.Usage.ByAgent[j].Key
	})
	sort.SliceStable(hour.Usage.ByProject, func(i, j int) bool {
		return reportingProjectLess(
			hour.Usage.ByProject[i].ProjectKey,
			hour.Usage.ByProject[i].Project,
			hour.Usage.ByProject[j].ProjectKey,
			hour.Usage.ByProject[j].Project,
		)
	})
	return hour
}

func cloneOrEmpty[T any](values []T) []T {
	if len(values) == 0 {
		return []T{}
	}
	return append([]T(nil), values...)
}

func reportingProjectLess(aKey, aProject, bKey, bProject string) bool {
	if aKey != bKey {
		return aKey < bKey
	}
	return aProject < bProject
}

func validateReportingBuckets(
	hourStart time.Time, buckets []ReportingActivityBucket,
) error {
	if len(buckets) != 12 {
		return fmt.Errorf("reporting hour requires 12 activity buckets, got %d", len(buckets))
	}
	for i, bucket := range buckets {
		want := hourStart.Add(time.Duration(i) * 5 * time.Minute).
			Format(time.RFC3339)
		if bucket.Start != want {
			return fmt.Errorf(
				"reporting bucket %d starts at %q, want %q",
				i, bucket.Start, want,
			)
		}
	}
	return nil
}
