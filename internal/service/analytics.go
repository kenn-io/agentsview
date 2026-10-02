package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

// AnalyticsRequest is the shared filter for tool and quality-signal
// analytics. It mirrors the /api/v1/analytics query: an empty range covers
// the 30 days ending today (UTC), and one-shot and automated sessions are
// excluded unless included.
type AnalyticsRequest struct {
	From             string `json:"from,omitempty"`
	To               string `json:"to,omitempty"`
	Timezone         string `json:"timezone,omitempty"`
	Project          string `json:"project,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Machine          string `json:"machine,omitempty"`
	IncludeOneShot   bool   `json:"include_one_shot,omitempty"`
	IncludeAutomated bool   `json:"include_automated,omitempty"`
}

// SignalSessionsRequest asks for example sessions behind one quality signal.
type SignalSessionsRequest struct {
	AnalyticsRequest
	Signal string `json:"signal"`
	Limit  int    `json:"limit,omitempty"`
}

// ActivityReportRequest selects an Activity report range. Preset is day,
// week, month, or custom (From/To as RFC3339); Date anchors presets and
// defaults to today in Timezone. Automation is all, interactive, or
// automated.
type ActivityReportRequest struct {
	Preset     string `json:"preset,omitempty"`
	Date       string `json:"date,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
	Project    string `json:"project,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Machine    string `json:"machine,omitempty"`
	Automation string `json:"automation,omitempty"`
}

// AnalyticsInputError flags an invalid analytics or activity filter so
// transports can report it as a client error.
type AnalyticsInputError struct{ Msg string }

func (e *AnalyticsInputError) Error() string { return e.Msg }

// BuildAnalyticsFilter validates req and applies the analytics defaults.
// Machine is passed through unresolved.
func BuildAnalyticsFilter(req AnalyticsRequest, now time.Time) (db.AnalyticsFilter, error) {
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return db.AnalyticsFilter{}, &AnalyticsInputError{Msg: "invalid timezone: " + tz}
	}
	from, to := req.From, req.To
	if to == "" {
		to = now.UTC().Format(time.DateOnly)
	}
	if from == "" {
		end, err := time.Parse(time.DateOnly, to)
		if err != nil {
			end = now.UTC()
		}
		from = end.AddDate(0, 0, -30).Format(time.DateOnly)
	}
	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) {
		return db.AnalyticsFilter{}, &AnalyticsInputError{Msg: "invalid date format: use YYYY-MM-DD"}
	}
	if from > to {
		return db.AnalyticsFilter{}, &AnalyticsInputError{Msg: "from must not be after to"}
	}
	return db.AnalyticsFilter{
		From: from, To: to, Timezone: tz,
		Project: req.Project, Agent: req.Agent, Machine: req.Machine,
		ExcludeOneShot:   !req.IncludeOneShot,
		ExcludeAutomated: !req.IncludeAutomated,
	}, nil
}

// BuildActivityReportSelection resolves req into the store query and filter
// the activity report route uses. Machine is passed through unresolved.
func BuildActivityReportSelection(
	req ActivityReportRequest, now time.Time,
) (activity.Query, db.AnalyticsFilter, error) {
	if err := validateActivityFilterSize(req); err != nil {
		return activity.Query{}, db.AnalyticsFilter{}, err
	}
	// The daemon validates date as YYYY-MM-DD even for custom ranges.
	if req.Date != "" && !timeutil.IsValidDate(req.Date) {
		return activity.Query{}, db.AnalyticsFilter{}, &AnalyticsInputError{Msg: "invalid date: use YYYY-MM-DD"}
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return activity.Query{}, db.AnalyticsFilter{}, &AnalyticsInputError{Msg: "invalid timezone: " + tz}
	}
	input := activity.QueryInput{
		Preset: req.Preset, Date: req.Date, From: req.From, To: req.To, Timezone: tz,
	}
	if input.Date == "" && input.From == "" {
		input.Date = now.In(loc).Format(time.DateOnly)
	}
	q, err := activity.ResolveQuery(input, now)
	if err != nil {
		return activity.Query{}, db.AnalyticsFilter{}, &AnalyticsInputError{Msg: err.Error()}
	}
	f := db.AnalyticsFilter{
		Timezone: tz, Project: req.Project, Agent: req.Agent, Machine: req.Machine,
	}
	switch req.Automation {
	case "", "all":
	case "interactive":
		f.ExcludeAutomated = true
	case "automated":
		f.ExcludeInteractive = true
	default:
		return activity.Query{}, db.AnalyticsFilter{}, &AnalyticsInputError{Msg: fmt.Sprintf(
			"invalid automation %q (want all, interactive, or automated)", req.Automation)}
	}
	return q, f, nil
}

// Activity filter size limits, matching GET /api/v1/activity/report, keep
// signed report IDs bounded.
const (
	activityFilterValueMaxBytes = 1024
	activityFilterTotalMaxBytes = 3072
	activityTimezoneMaxBytes    = 128
)

func validateActivityFilterSize(req ActivityReportRequest) error {
	if len(req.Timezone) > activityTimezoneMaxBytes {
		return &AnalyticsInputError{Msg: fmt.Sprintf("activity timezone exceeds %d bytes", activityTimezoneMaxBytes)}
	}
	total := 0
	for _, field := range []struct{ name, value string }{
		{"project", req.Project}, {"agent", req.Agent}, {"machine", req.Machine},
	} {
		if len(field.value) > activityFilterValueMaxBytes {
			return &AnalyticsInputError{Msg: fmt.Sprintf(
				"activity %s filter exceeds %d bytes", field.name, activityFilterValueMaxBytes)}
		}
		total += len(field.value)
	}
	if total > activityFilterTotalMaxBytes {
		return &AnalyticsInputError{Msg: fmt.Sprintf("activity filters exceed %d bytes total", activityFilterTotalMaxBytes)}
	}
	return nil
}

func (b *directBackend) analyticsFilter(
	ctx context.Context, req AnalyticsRequest,
) (db.AnalyticsFilter, error) {
	f, err := BuildAnalyticsFilter(req, time.Now())
	if err != nil {
		return db.AnalyticsFilter{}, err
	}
	f.Machine, err = db.ResolveMachineFilter(ctx, b.db, f.Machine)
	return f, err
}

func (b *directBackend) ToolAnalytics(
	ctx context.Context, req AnalyticsRequest,
) (*db.ToolsAnalyticsResponse, error) {
	f, err := b.analyticsFilter(ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := b.db.GetAnalyticsTools(ctx, f)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (b *directBackend) SignalAnalytics(
	ctx context.Context, req AnalyticsRequest,
) (*db.SignalsAnalyticsResponse, error) {
	f, err := b.analyticsFilter(ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := b.db.GetAnalyticsSignals(ctx, f)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (b *directBackend) SignalSessions(
	ctx context.Context, req SignalSessionsRequest,
) (*db.SignalSessionsResponse, error) {
	f, err := b.analyticsFilter(ctx, req.AnalyticsRequest)
	if err != nil {
		return nil, err
	}
	res, err := b.db.GetAnalyticsSignalSessions(ctx, f, req.Signal, req.Limit)
	if errors.Is(err, db.ErrUnsupportedAnalyticsSignal) {
		return nil, &AnalyticsInputError{Msg: "unsupported signal"}
	}
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (b *directBackend) ActivityReport(
	ctx context.Context, req ActivityReportRequest,
) (*activity.Report, error) {
	q, f, err := BuildActivityReportSelection(req, time.Now())
	if err != nil {
		return nil, err
	}
	f.Machine, err = db.ResolveMachineFilter(ctx, b.db, f.Machine)
	if err != nil {
		return nil, err
	}
	report, err := b.db.GetActivityReport(ctx, f, q)
	if err != nil {
		return nil, err
	}
	return &report, nil
}
