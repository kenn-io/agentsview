package server

import (
	"context"
	"fmt"
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
	Preset    string `query:"preset" enum:"day,week,month,custom" doc:"Range preset"`
	Date      string `query:"date" format:"date" doc:"Calendar day (YYYY-MM-DD) for presets"`
	From      string `query:"from" doc:"Range start (RFC3339) for custom ranges"`
	To        string `query:"to" doc:"Range end (RFC3339) for custom ranges"`
	Timezone  string `query:"timezone" doc:"IANA timezone name"`
	Bucket    string `query:"bucket" enum:"5m,15m,1h,1d,1w" doc:"Timeline bucket size override"`
	Project   string `query:"project" doc:"Filter by project"`
	GitBranch string `query:"git_branch" doc:"Filter by git branch; opaque (project, branch) tokens from the /branches endpoint"`
	Agent     string `query:"agent" doc:"Filter by agent"`
	Machine   string `query:"machine" doc:"Filter by machine"`
	// Automation classes the report: "all" (default) keeps both, "interactive"
	// drops automated sessions, "automated" drops interactive ones. Empty is
	// treated as "all"; any other value is rejected.
	Automation string `query:"automation" default:"all" doc:"Automation class: all, interactive, or automated"`
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
	excludeAutomated, excludeInteractive, err := activityAutomationFilter(in.Automation)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	// The activity report intentionally includes one-shot sessions, unlike
	// analytics which excludes them by default. The automation class is the
	// caller's choice (default "all" keeps both automated and interactive).
	f := db.AnalyticsFilter{
		Timezone: tz, Project: in.Project, GitBranch: in.GitBranch,
		Agent: in.Agent, Machine: in.Machine,
		ExcludeOneShot:     false,
		ExcludeAutomated:   excludeAutomated,
		ExcludeInteractive: excludeInteractive,
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

// activityAutomationFilter maps the activity report's automation query value to
// the AnalyticsFilter class exclusions. Empty and "all" keep both classes;
// "interactive" drops automated sessions; "automated" drops interactive ones.
// Any other value is an error so a typo surfaces as 400 rather than silently
// returning the unfiltered report.
func activityAutomationFilter(
	automation string,
) (excludeAutomated, excludeInteractive bool, err error) {
	switch automation {
	case "", "all":
		return false, false, nil
	case "interactive":
		return true, false, nil
	case "automated":
		return false, true, nil
	default:
		return false, false, fmt.Errorf(
			"invalid automation %q (want all, interactive, or automated)",
			automation)
	}
}
