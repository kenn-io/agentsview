package server

import (
	"context"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerActivityRoutes() {
	group := newRouteGroup(s.api, "/api/v1/activity", "Activity")
	get(s, group, "/report", "Get activity report", s.humaActivityReport)
}

type activityReportInput struct {
	Preset   string `query:"preset" enum:"day,week,month,custom" doc:"Range preset"`
	Date     string `query:"date" format:"date" doc:"Calendar day (YYYY-MM-DD) for presets"`
	From     string `query:"from" doc:"Range start (RFC3339) for custom ranges"`
	To       string `query:"to" doc:"Range end (RFC3339) for custom ranges"`
	Timezone string `query:"timezone" doc:"IANA timezone name"`
	Bucket   string `query:"bucket" enum:"5m,15m,1h,1d,1w" doc:"Timeline bucket size override"`
	Project  string `query:"project" doc:"Filter by project"`
	Agent    string `query:"agent" doc:"Filter by agent"`
	Machine  string `query:"machine" doc:"Filter by machine"`
}

func (s *Server) humaActivityReport(
	ctx context.Context, in *activityReportInput,
) (*jsonOutput[activity.Report], error) {
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "invalid timezone: "+tz)
	}
	input := activity.QueryInput{
		Preset: in.Preset, Date: in.Date, From: in.From, To: in.To,
		Timezone: tz, BucketOverride: in.Bucket,
	}
	// Presets need an anchor date; default to today in the requested
	// timezone, matching the prior day-only handler's behavior.
	if input.Date == "" && input.From == "" {
		input.Date = time.Now().In(loc).Format("2006-01-02")
	}
	q, err := activity.ResolveQuery(input, time.Now())
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	// The activity report intentionally includes one-shot and automated
	// sessions, unlike analytics which excludes them by default.
	f := db.AnalyticsFilter{
		Timezone: tz, Project: in.Project, Agent: in.Agent, Machine: in.Machine,
		ExcludeOneShot: false, ExcludeAutomated: false,
	}
	r, err := s.db.GetActivityReport(ctx, f, q)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("activity report error", err)
	}
	return &jsonOutput[activity.Report]{Body: r}, nil
}
