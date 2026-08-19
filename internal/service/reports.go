package service

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

// ReportFilter is the shared read-only filter for analytics-style reports.
type ReportFilter struct {
	From, To, Timezone, Machine, Project, GitBranch, Agent, Model string
	MinUserMessages                                               int
	IncludeOneShot, IncludeAutomated                              bool
}

type ActivityReportRequest struct {
	ReportFilter
	Preset, Date, Bucket, Automation string
}

type TrendsRequest struct {
	ReportFilter
	Terms       []string
	Granularity string
}

type InsightsRequest struct {
	Type, Project, DateFrom, DateTo string
}

// ReportService provides the focused read-only reports shared by MCP clients.
type ReportService interface {
	AnalyticsReport(context.Context, ReportFilter) (*db.AnalyticsSummary, error)
	ActivityReport(context.Context, ActivityReportRequest) (*activity.Report, error)
	Trends(context.Context, TrendsRequest) (*db.TrendsTermsResponse, error)
	ListPins(context.Context, string) ([]db.PinnedMessage, error)
	ListInsights(context.Context, InsightsRequest) ([]db.Insight, error)
	ListRecentEdits(context.Context, db.RecentEditsParams) (*db.RecentEditsResult, error)
}

func (b *directBackend) AnalyticsReport(ctx context.Context, in ReportFilter) (*db.AnalyticsSummary, error) {
	f, err := reportDBFilter(in)
	if err != nil {
		return nil, err
	}
	out, err := b.db.GetAnalyticsSummary(ctx, f)
	return &out, err
}

func (b *directBackend) ActivityReport(ctx context.Context, in ActivityReportRequest) (*activity.Report, error) {
	tz := defaultTimezone(in.Timezone)
	if _, err := time.LoadLocation(tz); err != nil {
		return nil, err
	}
	query, err := activity.ResolveQuery(activity.QueryInput{Preset: in.Preset, Date: in.Date, From: in.From, To: in.To, Timezone: tz, BucketOverride: in.Bucket}, time.Now())
	if err != nil {
		return nil, err
	}
	filter, err := reportDBFilter(in.ReportFilter)
	if err != nil {
		return nil, err
	}
	filter.From, filter.To, filter.Timezone = "", "", tz
	switch in.Automation {
	case "", "all":
		filter.ExcludeAutomated = false
	case "interactive":
		filter.ExcludeAutomated = true
	case "automated":
		filter.ExcludeAutomated, filter.ExcludeInteractive = false, true
	default:
		return nil, errors.New("automation must be all, interactive, or automated")
	}
	out, err := b.db.GetActivityReport(ctx, filter, query)
	return &out, err
}

func (b *directBackend) Trends(ctx context.Context, in TrendsRequest) (*db.TrendsTermsResponse, error) {
	f, err := reportDBFilter(in.ReportFilter)
	if err != nil {
		return nil, err
	}
	terms, err := db.ParseTrendTerms(in.Terms)
	if err != nil {
		return nil, err
	}
	out, err := b.db.GetTrendsTerms(ctx, f, terms, in.Granularity)
	return &out, err
}

func (b *directBackend) ListPins(ctx context.Context, project string) ([]db.PinnedMessage, error) {
	return b.db.ListPinnedMessages(ctx, "", project)
}

func (b *directBackend) ListInsights(ctx context.Context, in InsightsRequest) ([]db.Insight, error) {
	return b.db.ListInsights(ctx, db.InsightFilter{Type: in.Type, Project: in.Project, DateFrom: in.DateFrom, DateTo: in.DateTo})
}

func (b *directBackend) ListRecentEdits(ctx context.Context, in db.RecentEditsParams) (*db.RecentEditsResult, error) {
	out, err := b.db.RecentEdits(ctx, in)
	return &out, err
}

func (b *httpBackend) AnalyticsReport(ctx context.Context, in ReportFilter) (*db.AnalyticsSummary, error) {
	var out db.AnalyticsSummary
	err := b.getJSON(ctx, "/api/v1/analytics/summary?"+reportQuery(in).Encode(), &out)
	return &out, err
}

func (b *httpBackend) ActivityReport(ctx context.Context, in ActivityReportRequest) (*activity.Report, error) {
	q := reportQuery(in.ReportFilter)
	setNotEmpty(q, "preset", in.Preset)
	setNotEmpty(q, "date", in.Date)
	setNotEmpty(q, "bucket", in.Bucket)
	setNotEmpty(q, "automation", in.Automation)
	var out activity.Report
	err := b.getJSON(ctx, "/api/v1/activity/report?"+q.Encode(), &out)
	return &out, err
}

func (b *httpBackend) Trends(ctx context.Context, in TrendsRequest) (*db.TrendsTermsResponse, error) {
	q := reportQuery(in.ReportFilter)
	for _, term := range in.Terms {
		q.Add("term", term)
	}
	setNotEmpty(q, "granularity", in.Granularity)
	var out db.TrendsTermsResponse
	err := b.getJSON(ctx, "/api/v1/trends/terms?"+q.Encode(), &out)
	return &out, err
}

func (b *httpBackend) ListPins(ctx context.Context, project string) ([]db.PinnedMessage, error) {
	q := url.Values{}
	setNotEmpty(q, "project", project)
	var out struct {
		Pins []db.PinnedMessage `json:"pins"`
	}
	err := b.getJSON(ctx, "/api/v1/pins?"+q.Encode(), &out)
	return out.Pins, err
}

func (b *httpBackend) ListInsights(ctx context.Context, in InsightsRequest) ([]db.Insight, error) {
	q := url.Values{}
	setNotEmpty(q, "type", in.Type)
	setNotEmpty(q, "project", in.Project)
	setNotEmpty(q, "date_from", in.DateFrom)
	setNotEmpty(q, "date_to", in.DateTo)
	var out struct {
		Insights []db.Insight `json:"insights"`
	}
	err := b.getJSON(ctx, "/api/v1/insights?"+q.Encode(), &out)
	return out.Insights, err
}

func (b *httpBackend) ListRecentEdits(ctx context.Context, in db.RecentEditsParams) (*db.RecentEditsResult, error) {
	q := url.Values{}
	setNotEmpty(q, "project", in.Project)
	setNotEmpty(q, "search", in.Search)
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	if in.Offset > 0 {
		q.Set("offset", strconv.Itoa(in.Offset))
	}
	var out db.RecentEditsResult
	err := b.getJSON(ctx, "/api/v1/recent-edits?"+q.Encode(), &out)
	return &out, err
}

func reportDBFilter(in ReportFilter) (db.AnalyticsFilter, error) {
	from, to := in.From, in.To
	if to == "" {
		to = time.Now().UTC().Format(time.DateOnly)
	}
	if from == "" {
		parsed, _ := time.Parse(time.DateOnly, to)
		from = parsed.AddDate(0, 0, -30).Format(time.DateOnly)
	}
	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) || from > to {
		return db.AnalyticsFilter{}, errors.New("invalid report date range")
	}
	tz := defaultTimezone(in.Timezone)
	if _, err := time.LoadLocation(tz); err != nil {
		return db.AnalyticsFilter{}, err
	}
	return db.AnalyticsFilter{From: from, To: to, Timezone: tz, Machine: in.Machine, Project: in.Project, GitBranch: in.GitBranch, Agent: in.Agent, Model: in.Model, MinUserMessages: in.MinUserMessages, ExcludeOneShot: !in.IncludeOneShot, ExcludeAutomated: !in.IncludeAutomated}, nil
}

func reportQuery(in ReportFilter) url.Values {
	q := url.Values{}
	setNotEmpty(q, "from", in.From)
	setNotEmpty(q, "to", in.To)
	setNotEmpty(q, "timezone", in.Timezone)
	setNotEmpty(q, "machine", in.Machine)
	setNotEmpty(q, "project", in.Project)
	setNotEmpty(q, "git_branch", in.GitBranch)
	setNotEmpty(q, "agent", in.Agent)
	setNotEmpty(q, "model", in.Model)
	if in.MinUserMessages > 0 {
		q.Set("min_user_messages", strconv.Itoa(in.MinUserMessages))
	}
	if in.IncludeOneShot {
		q.Set("include_one_shot", "true")
	}
	if in.IncludeAutomated {
		q.Set("include_automated", "true")
	}
	return q
}

func setNotEmpty(q url.Values, key, value string) {
	if strings.TrimSpace(value) != "" {
		q.Set(key, value)
	}
}
func defaultTimezone(value string) string {
	if value == "" {
		return "UTC"
	}
	return value
}

var _ ReportService = (*directBackend)(nil)
var _ ReportService = (*httpBackend)(nil)
