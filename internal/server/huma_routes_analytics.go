package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (s *Server) registerAnalyticsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/analytics", "Analytics")

	get(s, group, "/summary", "Get analytics summary", s.humaAnalyticsSummary)
	get(s, group, "/activity", "Get analytics activity", s.humaAnalyticsActivity)
	get(s, group, "/heatmap", "Get analytics heatmap", s.humaAnalyticsHeatmap)
	get(s, group, "/projects", "Get analytics by project", s.humaAnalyticsProjects)
	get(s, group, "/hour-of-week", "Get analytics by hour of week", s.humaAnalyticsHourOfWeek)
	get(s, group, "/sessions", "Get session shape analytics", s.humaAnalyticsSessionShape)
	get(s, group, "/velocity", "Get velocity analytics", s.humaAnalyticsVelocity)
	get(s, group, "/tools", "Get tool analytics", s.humaAnalyticsTools)
	get(s, group, "/skills", "Get skill analytics", s.humaAnalyticsSkills)
	get(s, group, "/top-sessions", "Get top sessions", s.humaAnalyticsTopSessions)
	get(s, group, "/signals", "Get signal analytics", s.humaAnalyticsSignals)
	get(s, group, "/signal-sessions", "Get signal session examples", s.humaAnalyticsSignalSessions)
	get(s, group, "/issue-review", "Get proactive issue review", s.humaAnalyticsIssueReview)
}

type analyticsGranularity string

type heatmapMetric string

type topSessionMetric string

type AnalyticsFilterInput struct {
	From             string           `query:"from" format:"date" doc:"Range start date"`
	To               string           `query:"to" format:"date" doc:"Range end date"`
	Timezone         string           `query:"timezone" doc:"IANA timezone name"`
	Machine          string           `query:"machine" doc:"Filter by machine"`
	Project          string           `query:"project" doc:"Filter by project"`
	GitBranch        string           `query:"git_branch" doc:"Filter by git branch; opaque (project, branch) tokens from the /branches endpoint"`
	Agent            string           `query:"agent" doc:"Filter by agent"`
	Model            string           `query:"model" doc:"Comma-separated model filter"`
	DayOfWeek        optionalIntParam `query:"dow" minimum:"0" maximum:"6" doc:"Day of week, Monday=0 through Sunday=6"`
	Hour             optionalIntParam `query:"hour" minimum:"0" maximum:"23" doc:"Hour of day, 0 through 23"`
	MinUserMessages  int              `query:"min_user_messages" minimum:"0" doc:"Minimum user message count"`
	ActiveSince      string           `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	AutomatedScope   string           `query:"automated_scope" enum:"human,all,automated" doc:"Automation scope"`
	IncludeOneShot   bool             `query:"include_one_shot" doc:"Include one-shot sessions"`
	IncludeAutomated bool             `query:"include_automated" doc:"Include automated sessions"`
	Termination      string           `query:"termination" doc:"Filter by termination reason"`
}

type analyticsActivityInput struct {
	AnalyticsFilterInput
	Granularity analyticsGranularity `query:"granularity" enum:"day,week,month" default:"day" doc:"Time bucket granularity"`
}

type analyticsSkillsInput struct {
	AnalyticsFilterInput
	Granularity analyticsGranularity `query:"granularity" enum:"day,week,month" default:"week" doc:"Trend bucket granularity"`
}

type analyticsHeatmapInput struct {
	AnalyticsFilterInput
	Metric heatmapMetric `query:"metric" enum:"messages,sessions,output_tokens" default:"messages" doc:"Heatmap metric"`
}

type analyticsTopSessionsInput struct {
	AnalyticsFilterInput
	Metric topSessionMetric `query:"metric" enum:"messages,duration,output_tokens" default:"messages" doc:"Ranking metric"`
}

type analyticsSignalSessionsInput struct {
	AnalyticsFilterInput
	Signal string `query:"signal" required:"true" doc:"Signal name"`
	Limit  int    `query:"limit" minimum:"0" maximum:"20" default:"10" doc:"Maximum number of session examples"`
}

type analyticsIssueReviewInput struct {
	AnalyticsFilterInput
	SessionID          string `query:"session_id" doc:"Exact chat session ID"`
	Folder             string `query:"folder" doc:"Exact session working directory"`
	Category           string `query:"category" doc:"Issue reason code"`
	Reason             string `query:"reason" doc:"Issue reason code alias"`
	Tool               string `query:"tool" doc:"Normalized tool name"`
	Source             string `query:"source" doc:"Evidence source"`
	Outcome            string `query:"outcome" doc:"Session outcome"`
	Severity           string `query:"severity" enum:"high,medium,low" doc:"Finding severity"`
	Confidence         string `query:"confidence" enum:"high,medium,low" doc:"Finding confidence"`
	Status             string `query:"status" enum:"open,recovered,recurring,observed" doc:"Finding status"`
	RecommendationType string `query:"recommendation_type" enum:"skill,script,rule,tool_fix" doc:"Suggested action type"`
	MinOccurrences     int    `query:"min_occurrences" minimum:"1" default:"1" doc:"Minimum repeated occurrences"`
	MinSessions        int    `query:"min_sessions" minimum:"1" default:"1" doc:"Minimum distinct chats"`
	MinProjects        int    `query:"min_projects" minimum:"0" default:"0" doc:"Minimum distinct projects"`
	MinWastedMS        int64  `query:"min_wasted_ms" minimum:"0" default:"0" doc:"Minimum estimated wasted duration in milliseconds"`
	Sort               string `query:"sort" enum:"impact,frequency,recent,waste,duration" default:"impact" doc:"Finding sort order"`
	Refresh            bool   `query:"refresh" default:"false" doc:"Bypass the short analysis cache"`
	Offset             int    `query:"offset" minimum:"0" default:"0" doc:"Findings to skip after filtering and sorting"`
	Limit              int    `query:"limit" minimum:"1" maximum:"100" default:"50" doc:"Maximum findings"`
}

type analyticsIssueReviewStore interface {
	GetAnalyticsIssueReview(context.Context, db.AnalyticsFilter, db.IssueReviewQuery) (db.IssueReviewResponse, error)
}

func analyticsFilterFromInput(in AnalyticsFilterInput) (db.AnalyticsFilter, error) {
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "invalid timezone: "+tz)
	}
	from, to := defaultDateRange(in.From, in.To)
	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
	}
	if from > to {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "from must not be after to")
	}
	if in.ActiveSince != "" && !timeutil.IsValidTimestamp(in.ActiveSince) {
		return db.AnalyticsFilter{}, apiError(http.StatusBadRequest, "invalid active_since: use RFC3339 timestamp")
	}
	return db.AnalyticsFilter{
		From:             from,
		To:               to,
		Machine:          in.Machine,
		Project:          in.Project,
		GitBranch:        in.GitBranch,
		Agent:            in.Agent,
		Model:            in.Model,
		Timezone:         tz,
		DayOfWeek:        optionalIntValue(in.DayOfWeek),
		Hour:             optionalIntValue(in.Hour),
		MinUserMessages:  in.MinUserMessages,
		ExcludeOneShot:   !in.IncludeOneShot,
		ExcludeAutomated: !in.IncludeAutomated,
		AutomatedScope:   in.AutomatedScope,
		ActiveSince:      in.ActiveSince,
		Termination:      in.Termination,
	}, nil
}

func (s *Server) humaAnalyticsSummary(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.AnalyticsSummary], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSummary(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.AnalyticsSummary]{Body: result}, nil
}

func (s *Server) humaAnalyticsActivity(
	ctx context.Context,
	in *analyticsActivityInput,
) (*jsonOutput[db.ActivityResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsActivity(ctx, f, string(in.Granularity))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.ActivityResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsHeatmap(
	ctx context.Context,
	in *analyticsHeatmapInput,
) (*jsonOutput[db.HeatmapResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsHeatmap(ctx, f, string(in.Metric))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.HeatmapResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsProjects(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.ProjectsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsProjects(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.ProjectsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsHourOfWeek(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.HourOfWeekResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsHourOfWeek(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.HourOfWeekResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsSessionShape(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.SessionShapeResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSessionShape(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.SessionShapeResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsVelocity(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.VelocityResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsVelocity(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.VelocityResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsTools(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.ToolsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsTools(ctx, f)
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.ToolsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsSkills(
	ctx context.Context,
	in *analyticsSkillsInput,
) (*jsonOutput[db.SkillsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSkills(ctx, f, string(in.Granularity))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.SkillsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsTopSessions(
	ctx context.Context,
	in *analyticsTopSessionsInput,
) (*jsonOutput[db.TopSessionsResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsTopSessions(ctx, f, string(in.Metric))
	if err != nil {
		return nil, internalError("analytics error", err)
	}
	return &jsonOutput[db.TopSessionsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsSignals(
	ctx context.Context,
	in *AnalyticsFilterInput,
) (*jsonOutput[db.SignalsAnalyticsResponse], error) {
	f, err := analyticsFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSignals(ctx, f)
	if err != nil {
		return nil, internalError("analytics signals error", err)
	}
	return &jsonOutput[db.SignalsAnalyticsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsSignalSessions(
	ctx context.Context,
	in *analyticsSignalSessionsInput,
) (*jsonOutput[db.SignalSessionsResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetAnalyticsSignalSessions(
		ctx, f, in.Signal, in.Limit,
	)
	if err != nil {
		if errors.Is(err, db.ErrUnsupportedAnalyticsSignal) {
			return nil, apiError(http.StatusBadRequest,
				"unsupported signal")
		}
		return nil, internalError("analytics signal sessions error", err)
	}
	return &jsonOutput[db.SignalSessionsResponse]{Body: result}, nil
}

func (s *Server) humaAnalyticsIssueReview(
	ctx context.Context,
	in *analyticsIssueReviewInput,
) (*jsonOutput[db.IssueReviewResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	store, ok := s.db.(analyticsIssueReviewStore)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "issue review is not supported by this store")
	}
	reason := in.Category
	if reason == "" {
		reason = in.Reason
	}
	result, err := store.GetAnalyticsIssueReview(ctx, f, db.IssueReviewQuery{
		SessionID:           in.SessionID,
		Folder:              in.Folder,
		Reason:              reason,
		Tool:                in.Tool,
		Source:              in.Source,
		Outcome:             in.Outcome,
		Severity:            in.Severity,
		Confidence:          in.Confidence,
		Status:              in.Status,
		RecommendationType:  in.RecommendationType,
		MinOccurrences:      in.MinOccurrences,
		MinSessions:         in.MinSessions,
		MinProjects:         in.MinProjects,
		MinWastedDurationMS: in.MinWastedMS,
		Sort:                in.Sort,
		Refresh:             in.Refresh,
		Offset:              in.Offset,
		Limit:               in.Limit,
	})
	if err != nil {
		return nil, internalError("analytics issue review error", err)
	}
	return &jsonOutput[db.IssueReviewResponse]{Body: result}, nil
}
