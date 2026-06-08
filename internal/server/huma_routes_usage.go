package server

import (
	"context"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (s *Server) registerUsageRoutes() {
	group := newRouteGroup(s.api, "/api/v1/usage", "Usage")

	get(s, group, "/summary", "Get usage summary", s.humaUsageSummary)
	get(s, group, "/comparison", "Get usage comparison", s.humaUsageComparison)
	get(s, group, "/top-sessions", "Get top usage sessions", s.humaUsageTopSessions)
}

type UsageFilterInput struct {
	From             string `query:"from" format:"date" doc:"Range start date"`
	To               string `query:"to" format:"date" doc:"Range end date"`
	Timezone         string `query:"timezone" doc:"IANA timezone name"`
	Agent            string `query:"agent" doc:"Filter by agent"`
	Project          string `query:"project" doc:"Filter by project"`
	Machine          string `query:"machine" doc:"Filter by machine"`
	ExcludeProject   string `query:"exclude_project" doc:"Exclude a project"`
	ExcludeAgent     string `query:"exclude_agent" doc:"Exclude an agent"`
	ExcludeModel     string `query:"exclude_model" doc:"Exclude a model"`
	Model            string `query:"model" doc:"Filter by model"`
	MinUserMessages  int    `query:"min_user_messages" minimum:"0" doc:"Minimum user message count"`
	ActiveSince      string `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	IncludeOneShot   bool   `query:"include_one_shot" default:"true" doc:"Include one-shot sessions"`
	IncludeAutomated bool   `query:"include_automated" doc:"Include automated sessions"`
}

type usageTopSessionsInput struct {
	UsageFilterInput
	Limit int `query:"limit" minimum:"0" maximum:"100" default:"20" doc:"Maximum number of sessions"`
}

type usageComparisonInput struct {
	UsageFilterInput
	CurrentCost float64 `query:"current_cost" required:"true" doc:"Current period total cost"`
}

func usageFilterFromInput(in UsageFilterInput) (db.UsageFilter, error) {
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "invalid timezone: "+tz)
	}
	from, to := defaultDateRange(in.From, in.To)
	if !timeutil.IsValidDate(from) || !timeutil.IsValidDate(to) {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
	}
	if from > to {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "from must not be after to")
	}
	if in.ActiveSince != "" && !timeutil.IsValidTimestamp(in.ActiveSince) {
		return db.UsageFilter{}, apiError(http.StatusBadRequest, "invalid active_since: use RFC3339 timestamp")
	}
	return db.UsageFilter{
		From:             from,
		To:               to,
		Agent:            in.Agent,
		Project:          in.Project,
		Machine:          in.Machine,
		ExcludeProject:   in.ExcludeProject,
		ExcludeAgent:     in.ExcludeAgent,
		ExcludeModel:     in.ExcludeModel,
		Model:            in.Model,
		Timezone:         tz,
		MinUserMessages:  in.MinUserMessages,
		ExcludeOneShot:   !in.IncludeOneShot,
		ExcludeAutomated: !in.IncludeAutomated,
		ActiveSince:      in.ActiveSince,
		Breakdowns:       true,
	}, nil
}

func (s *Server) humaUsageSummary(
	ctx context.Context,
	in *UsageFilterInput,
) (*jsonOutput[UsageSummaryResponse], error) {
	f, err := usageFilterFromInput(*in)
	if err != nil {
		return nil, err
	}
	result, err := s.db.GetDailyUsage(ctx, f)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage summary error", err)
	}
	resp := UsageSummaryResponse{
		From:          f.From,
		To:            f.To,
		Totals:        result.Totals,
		Daily:         result.Daily,
		ProjectTotals: foldProjectTotals(result.Daily),
		ModelTotals:   foldModelTotals(result.Daily),
		AgentTotals:   foldAgentTotals(result.Daily),
		SessionCounts: result.SessionCounts,
		CacheStats:    computeCacheStats(result.Totals),
	}
	return &jsonOutput[UsageSummaryResponse]{Body: resp}, nil
}

func (s *Server) humaUsageComparison(
	ctx context.Context,
	in *usageComparisonInput,
) (*jsonOutput[Comparison], error) {
	f, err := usageFilterFromInput(in.UsageFilterInput)
	if err != nil {
		return nil, err
	}
	comparison, err := s.computeUsageComparison(ctx, f, in.CurrentCost)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage comparison error", err)
	}
	return &jsonOutput[Comparison]{Body: *comparison}, nil
}

func (s *Server) computeUsageComparison(
	ctx context.Context,
	f db.UsageFilter,
	currentCost float64,
) (*Comparison, error) {
	fromT, err := time.Parse("2006-01-02", f.From)
	if err != nil {
		return nil, err
	}
	toT, err := time.Parse("2006-01-02", f.To)
	if err != nil {
		return nil, err
	}
	days := int(toT.Sub(fromT).Hours()/24) + 1
	priorTo := fromT.AddDate(0, 0, -1)
	priorFrom := priorTo.AddDate(0, 0, -(days - 1))
	priorFilter := db.UsageFilter{
		From:             priorFrom.Format("2006-01-02"),
		To:               priorTo.Format("2006-01-02"),
		Agent:            f.Agent,
		Project:          f.Project,
		Machine:          f.Machine,
		Model:            f.Model,
		ExcludeProject:   f.ExcludeProject,
		ExcludeAgent:     f.ExcludeAgent,
		ExcludeModel:     f.ExcludeModel,
		Timezone:         f.Timezone,
		MinUserMessages:  f.MinUserMessages,
		ExcludeOneShot:   f.ExcludeOneShot,
		ExcludeAutomated: f.ExcludeAutomated,
		ActiveSince:      f.ActiveSince,
		Breakdowns:       false,
	}
	priorResult, err := s.db.GetDailyUsage(ctx, priorFilter)
	if err != nil {
		return nil, err
	}
	c := &Comparison{
		PriorFrom:      priorFilter.From,
		PriorTo:        priorFilter.To,
		PriorTotalCost: priorResult.Totals.TotalCost,
	}
	if c.PriorTotalCost > 0 {
		c.DeltaPct = (currentCost - c.PriorTotalCost) / c.PriorTotalCost
	}
	return c, nil
}

func (s *Server) humaUsageTopSessions(
	ctx context.Context,
	in *usageTopSessionsInput,
) (*jsonOutput[[]db.TopSessionEntry], error) {
	f, err := usageFilterFromInput(in.UsageFilterInput)
	if err != nil {
		return nil, err
	}
	f.Breakdowns = false
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	entries, err := s.db.GetTopSessionsByCost(ctx, f, limit)
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage top sessions error", err)
	}
	return &jsonOutput[[]db.TopSessionEntry]{Body: entries}, nil
}
