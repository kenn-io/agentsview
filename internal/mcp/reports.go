// ABOUTME: Read-only MCP tools for archive-wide reports: the Activity
// ABOUTME: report, tool usage, quality signals, and usage comparison.
package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

const (
	defaultActivitySessions = 20
	maxActivitySessions     = 100
	defaultSignalExamples   = 5
	maxSignalExamples       = 20
)

// analyticsIn is the filter shared by get_tool_usage and get_quality_signals.
type analyticsIn struct {
	From             string `json:"from,omitempty" jsonschema:"Range start date (YYYY-MM-DD). Default: 30 days before to."`
	To               string `json:"to,omitempty" jsonschema:"Range end date (YYYY-MM-DD). Default: today (UTC)."`
	Project          string `json:"project,omitempty" jsonschema:"Filter by project."`
	Agent            string `json:"agent,omitempty" jsonschema:"Filter by agent."`
	Machine          string `json:"machine,omitempty" jsonschema:"Filter by machine."`
	IncludeOneShot   bool   `json:"include_one_shot,omitempty" jsonschema:"Include one-shot sessions. Default false."`
	IncludeAutomated bool   `json:"include_automated,omitempty" jsonschema:"Include automated sessions. Default false."`
}

func (in analyticsIn) request() service.AnalyticsRequest {
	return service.AnalyticsRequest{
		From: in.From, To: in.To, Project: in.Project, Agent: in.Agent, Machine: in.Machine,
		IncludeOneShot: in.IncludeOneShot, IncludeAutomated: in.IncludeAutomated,
	}
}

// --- get_tool_usage ---

func (t *toolset) toolUsage(
	ctx context.Context, _ *mcp.CallToolRequest, in analyticsIn,
) (*mcp.CallToolResult, *db.ToolsAnalyticsResponse, error) {
	res, err := t.svc.ToolAnalytics(ctx, in.request())
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}

// --- get_quality_signals ---

type qualitySignalsIn struct {
	analyticsIn `json:",inline"`
	Signal      string `json:"signal,omitempty" jsonschema:"Also return example sessions for this signal, such as tool_failure_signals, tool_retries, edit_churn, outcome_errored, outcome_abandoned, high_pressure_sessions, runaway_tool_loop_count, or frustration_marker_count."`
	Examples    int    `json:"examples,omitempty" jsonschema:"Max example sessions when signal is set, default 5, max 20."`
}

type qualitySignalsOut struct {
	Signals  *db.SignalsAnalyticsResponse `json:"signals"`
	Examples *db.SignalSessionsResponse   `json:"examples,omitempty"`
}

func (t *toolset) qualitySignals(
	ctx context.Context, _ *mcp.CallToolRequest, in qualitySignalsIn,
) (*mcp.CallToolResult, qualitySignalsOut, error) {
	if in.Signal != "" && !db.IsSupportedAnalyticsSignal(in.Signal) {
		return nil, qualitySignalsOut{}, fmt.Errorf("unsupported signal %q", in.Signal)
	}
	signals, err := t.svc.SignalAnalytics(ctx, in.request())
	if err != nil {
		return nil, qualitySignalsOut{}, err
	}
	out := qualitySignalsOut{Signals: signals}
	if in.Signal != "" {
		out.Examples, err = t.svc.SignalSessions(ctx, service.SignalSessionsRequest{
			AnalyticsRequest: in.request(), Signal: in.Signal,
			Limit: clampLimit(in.Examples, defaultSignalExamples, maxSignalExamples),
		})
		if err != nil {
			return nil, qualitySignalsOut{}, err
		}
	}
	return nil, out, nil
}

// --- get_activity_report ---

type activityReportIn struct {
	Preset         string `json:"preset,omitempty" jsonschema:"day (default), week, month, or custom."`
	Date           string `json:"date,omitempty" jsonschema:"Day inside the day, week, or month to report (YYYY-MM-DD). Default: today in timezone."`
	From           string `json:"from,omitempty" jsonschema:"Custom range start (RFC3339)."`
	To             string `json:"to,omitempty" jsonschema:"Custom range end (RFC3339)."`
	Timezone       string `json:"timezone,omitempty" jsonschema:"IANA timezone for the range, e.g. America/New_York. Default UTC."`
	Project        string `json:"project,omitempty" jsonschema:"Filter by project."`
	Agent          string `json:"agent,omitempty" jsonschema:"Filter by agent."`
	Machine        string `json:"machine,omitempty" jsonschema:"Filter by machine."`
	Automation     string `json:"automation,omitempty" jsonschema:"all (default), interactive, or automated."`
	SessionsSort   string `json:"sessions_sort,omitempty" jsonschema:"Order of the session list: agent_minutes (default) or cost, largest first."`
	SessionsLimit  int    `json:"sessions_limit,omitempty" jsonschema:"Sessions per page, default 20, max 100."`
	SessionsCursor int    `json:"sessions_cursor,omitempty" jsonschema:"Pagination cursor from a previous sessions_next_cursor."`
}

type activityReportOut struct {
	Timezone           string                `json:"timezone"`
	RangeStart         string                `json:"range_start"`
	RangeEnd           string                `json:"range_end"`
	Partial            bool                  `json:"partial" jsonschema:"True when the range extends past now."`
	Totals             activity.Totals       `json:"totals"`
	Peak               activity.Peak         `json:"peak" jsonschema:"Most agents running at once."`
	ByProject          []activity.KeyMinutes `json:"by_project"`
	ByModel            []activity.KeyMinutes `json:"by_model"`
	ByAgent            []activity.KeyMinutes `json:"by_agent"`
	Sessions           []activity.SessionRow `json:"sessions"`
	SessionsTotal      int                   `json:"sessions_total"`
	SessionsNextCursor *int                  `json:"sessions_next_cursor,omitempty"`
}

func (t *toolset) activityReport(
	ctx context.Context, _ *mcp.CallToolRequest, in activityReportIn,
) (*mcp.CallToolResult, activityReportOut, error) {
	if in.SessionsCursor < 0 {
		return nil, activityReportOut{}, errors.New("sessions_cursor must not be negative")
	}
	sortKey := activity.SessionSortAgentMinutes
	switch in.SessionsSort {
	case "", string(activity.SessionSortAgentMinutes):
	case string(activity.SessionSortCost):
		sortKey = activity.SessionSortCost
	default:
		return nil, activityReportOut{}, errors.New("sessions_sort must be agent_minutes or cost")
	}
	report, err := t.svc.ActivityReport(ctx, service.ActivityReportRequest{
		Preset: in.Preset, Date: in.Date, From: in.From, To: in.To, Timezone: in.Timezone,
		Project: in.Project, Agent: in.Agent, Machine: in.Machine, Automation: in.Automation,
	})
	if err != nil {
		return nil, activityReportOut{}, err
	}
	page, err := activity.PageSessions(report.BySession, nil, activity.SessionPageOptions{
		Limit:  clampLimit(in.SessionsLimit, defaultActivitySessions, maxActivitySessions),
		Offset: in.SessionsCursor, Sort: sortKey, Direction: "desc",
	})
	if err != nil {
		return nil, activityReportOut{}, err
	}
	out := activityReportOut{
		Timezone: report.Timezone, RangeStart: report.RangeStart, RangeEnd: report.RangeEnd,
		Partial: report.Partial, Totals: report.Totals, Peak: report.Peak,
		ByProject: report.ByProject, ByModel: report.ByModel, ByAgent: report.ByAgent,
		Sessions: page.Sessions, SessionsTotal: page.Total,
	}
	if next := in.SessionsCursor + len(page.Sessions); next < page.Total {
		out.SessionsNextCursor = &next
	}
	return nil, out, nil
}

// --- compare_usage ---

type compareUsageIn struct {
	Dimension string `json:"dimension" jsonschema:"What the two sides name: model or project."`
	Left      string `json:"left" jsonschema:"Left side: one model or project name, or several separated by commas."`
	Right     string `json:"right" jsonschema:"Right side, in the same form as left."`
	From      string `json:"from,omitempty" jsonschema:"Range start date (YYYY-MM-DD)."`
	To        string `json:"to,omitempty" jsonschema:"Range end date (YYYY-MM-DD)."`
	Agent     string `json:"agent,omitempty" jsonschema:"Filter both sides by agent."`
	Machine   string `json:"machine,omitempty" jsonschema:"Filter both sides by machine."`
}

func (t *toolset) compareUsage(
	ctx context.Context, _ *mcp.CallToolRequest, in compareUsageIn,
) (*mcp.CallToolResult, *service.UsagePairwiseComparisonResponse, error) {
	if in.Dimension != "model" && in.Dimension != "project" {
		return nil, nil, errors.New("dimension must be model or project")
	}
	res, err := t.svc.UsagePairwiseComparison(ctx, service.UsagePairwiseComparisonRequest{
		From: in.From, To: in.To, Agent: in.Agent, Machine: in.Machine,
		// Count one-shot sessions like get_usage_summary does.
		IncludeOneShot: true,
		LeftDimension:  in.Dimension, LeftValue: in.Left,
		RightDimension: in.Dimension, RightValue: in.Right,
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}
