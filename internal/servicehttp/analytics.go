package servicehttp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// maxActivityRefreshes bounds how often ActivityReport restarts when the
// daemon reports that the archive changed while it was paging.
const maxActivityRefreshes = 3

// analyticsQuery holds the query values shared by the analytics routes.
type analyticsQuery struct {
	from, to                          *runtime.Date
	timezone, project, agent, machine *string
	includeOneShot, includeAutomated  *bool
}

func newAnalyticsQuery(req service.AnalyticsRequest) (analyticsQuery, error) {
	// Validate locally so both backends reject bad filters the same way.
	if _, err := service.BuildAnalyticsFilter(req, time.Now()); err != nil {
		return analyticsQuery{}, err
	}
	q := analyticsQuery{
		includeOneShot:   new(req.IncludeOneShot),
		includeAutomated: new(req.IncludeAutomated),
	}
	for _, d := range []struct {
		value string
		dst   **runtime.Date
	}{{req.From, &q.from}, {req.To, &q.to}} {
		if d.value == "" {
			continue
		}
		parsed, err := time.Parse(time.DateOnly, d.value)
		if err != nil {
			return analyticsQuery{}, &service.AnalyticsInputError{Msg: "invalid date format: use YYYY-MM-DD"}
		}
		*d.dst = &runtime.Date{Time: parsed}
	}
	for _, s := range []struct {
		value string
		dst   **string
	}{
		{req.Timezone, &q.timezone},
		{req.Project, &q.project},
		{req.Agent, &q.agent},
		{req.Machine, &q.machine},
	} {
		if s.value != "" {
			*s.dst = new(s.value)
		}
	}
	return q, nil
}

func (b *httpBackend) ToolAnalytics(
	ctx context.Context, req service.AnalyticsRequest,
) (*db.ToolsAnalyticsResponse, error) {
	f, err := newAnalyticsQuery(req)
	if err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1AnalyticsToolsWithResponse(ctx, &apiclient.GetAPIV1AnalyticsToolsRequestOptions{Query: &apiclient.GetAPIV1AnalyticsToolsQuery{
		From: f.from, To: f.to, Timezone: f.timezone, Project: f.project, Agent: f.agent,
		Machine: f.machine, IncludeOneShot: f.includeOneShot, IncludeAutomated: f.includeAutomated,
	}})
	if response == nil {
		return nil, err
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	return response.JSON200, nil
}

func (b *httpBackend) SignalAnalytics(
	ctx context.Context, req service.AnalyticsRequest,
) (*db.SignalsAnalyticsResponse, error) {
	f, err := newAnalyticsQuery(req)
	if err != nil {
		return nil, err
	}
	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1AnalyticsSignalsWithResponse(ctx, &apiclient.GetAPIV1AnalyticsSignalsRequestOptions{Query: &apiclient.GetAPIV1AnalyticsSignalsQuery{
		From: f.from, To: f.to, Timezone: f.timezone, Project: f.project, Agent: f.agent,
		Machine: f.machine, IncludeOneShot: f.includeOneShot, IncludeAutomated: f.includeAutomated,
	}})
	if response == nil {
		return nil, err
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	return response.JSON200, nil
}

func (b *httpBackend) SignalSessions(
	ctx context.Context, req service.SignalSessionsRequest,
) (*db.SignalSessionsResponse, error) {
	if !db.IsSupportedAnalyticsSignal(req.Signal) {
		return nil, &service.AnalyticsInputError{Msg: "unsupported signal"}
	}
	f, err := newAnalyticsQuery(req.AnalyticsRequest)
	if err != nil {
		return nil, err
	}
	q := &apiclient.GetAPIV1AnalyticsSignalSessionsQuery{
		From: f.from, To: f.to, Timezone: f.timezone, Project: f.project, Agent: f.agent,
		Machine: f.machine, IncludeOneShot: f.includeOneShot, IncludeAutomated: f.includeAutomated,
		Signal: req.Signal,
	}
	if req.Limit > 0 && req.Limit <= 20 {
		q.Limit = new(int64(req.Limit))
	}
	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1AnalyticsSignalSessionsWithResponse(ctx, &apiclient.GetAPIV1AnalyticsSignalSessionsRequestOptions{Query: q})
	if response == nil {
		return nil, err
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	return response.JSON200, nil
}

// ActivityReport fetches the report, then follows the daemon's session
// cursor so BySession holds every row, matching the direct backend.
func (b *httpBackend) ActivityReport(
	ctx context.Context, req service.ActivityReportRequest,
) (*activity.Report, error) {
	if _, _, err := service.BuildActivityReportSelection(req, time.Now()); err != nil {
		return nil, err
	}
	q := &apiclient.GetAPIV1ActivityReportQuery{}
	if req.Preset != "" {
		q.Preset = new(apiclient.GetAPIV1ActivityReportQueryPreset(req.Preset))
	}
	if req.Date != "" {
		date, err := time.Parse(time.DateOnly, req.Date)
		if err != nil {
			return nil, &service.AnalyticsInputError{Msg: "invalid date: use YYYY-MM-DD"}
		}
		q.Date = &runtime.Date{Time: date}
	}
	for _, s := range []struct {
		value string
		dst   **string
	}{
		{req.From, &q.From},
		{req.To, &q.To},
		{req.Timezone, &q.Timezone},
		{req.Project, &q.Project},
		{req.Agent, &q.Agent},
		{req.Machine, &q.Machine},
		{req.Automation, &q.Automation},
	} {
		if s.value != "" {
			*s.dst = new(s.value)
		}
	}
	api, err := b.apiClient(b.longRunningClient)
	if err != nil {
		return nil, err
	}
	acceptJSON := func(_ context.Context, r *http.Request) error {
		r.Header.Set("Accept", "application/json")
		return nil
	}
	response, err := api.GetAPIV1ActivityReportWithResponse(ctx, &apiclient.GetAPIV1ActivityReportRequestOptions{Query: q}, acceptJSON)
	if response == nil {
		return nil, err
	}
	if err := serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	report := response.JSON200
	refreshes := 0
	for report.SessionsNextCursor != "" {
		page, err := api.GetAPIV1ActivityReportReportIDSessionsWithResponse(ctx, &apiclient.GetAPIV1ActivityReportReportIDSessionsRequestOptions{
			PathParams: &apiclient.GetAPIV1ActivityReportReportIDSessionsPath{ReportID: url.PathEscape(report.ReportID)},
			Query: &apiclient.GetAPIV1ActivityReportReportIDSessionsQuery{
				Limit: new(int64(activity.MaxSessionPageLimit)), Cursor: new(report.SessionsNextCursor),
			},
		})
		if page == nil {
			return nil, err
		}
		if err := serviceResponseError(page.HTTPResponse, page.Body, err); err != nil {
			return nil, err
		}
		body := page.JSON200
		if body.RefreshRequired != nil && *body.RefreshRequired && body.Report != nil {
			// The archive changed mid-read; start over from the refreshed
			// report's first page.
			if refreshes == maxActivityRefreshes {
				return nil, errors.New("activity data kept changing while reading its sessions; try again")
			}
			refreshes++
			report = body.Report
			continue
		}
		if len(body.Sessions) == 0 {
			return nil, errors.New("daemon returned an empty activity session page")
		}
		report.BySession = append(report.BySession, body.Sessions...)
		report.SessionsNextCursor = ""
		if body.NextCursor != nil {
			report.SessionsNextCursor = *body.NextCursor
		}
	}
	return report, nil
}
