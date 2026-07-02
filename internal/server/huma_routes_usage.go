package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) registerUsageRoutes() {
	group := newRouteGroup(s.api, "/api/v1/usage", "Usage")

	get(s, group, "/summary", "Get usage summary", s.humaUsageSummary)
	get(s, group, "/comparison", "Get usage comparison", s.humaUsageComparison)
	get(
		s, group, "/pairwise-comparison",
		"Get usage pairwise comparison", s.humaUsagePairwiseComparison,
	)
	get(s, group, "/top-sessions", "Get top usage sessions", s.humaUsageTopSessions)
}

type UsageFilterInput struct {
	From             string `query:"from" format:"date" doc:"Range start date"`
	To               string `query:"to" format:"date" doc:"Range end date"`
	Timezone         string `query:"timezone" doc:"IANA timezone name"`
	Agent            string `query:"agent" doc:"Filter by agent"`
	Project          string `query:"project" doc:"Filter by project"`
	Machine          string `query:"machine" doc:"Filter by machine"`
	GitBranch        string `query:"git_branch" doc:"Filter by git branch; opaque (project, branch) tokens from the /branches endpoint"`
	ExcludeProject   string `query:"exclude_project" doc:"Exclude a project"`
	ExcludeAgent     string `query:"exclude_agent" doc:"Exclude an agent"`
	ExcludeModel     string `query:"exclude_model" doc:"Exclude a model"`
	Model            string `query:"model" doc:"Filter by model"`
	MinUserMessages  int    `query:"min_user_messages" minimum:"0" doc:"Minimum user message count"`
	ActiveSince      string `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	Termination      string `query:"termination" doc:"Filter by termination status"`
	IncludeOneShot   bool   `query:"include_one_shot" default:"true" doc:"Include one-shot sessions"`
	IncludeAutomated bool   `query:"include_automated" doc:"Include automated sessions"`
	NoDefaultRange   bool   `query:"no_default_range" doc:"Preserve omitted from/to without applying default range"`
	Breakdowns       bool   `query:"breakdowns" default:"true" doc:"Include per-model, per-project, and per-agent breakdowns"`
	SessionCounts    bool   `query:"session_counts" default:"true" doc:"Include distinct session counts"`
}

type usageTopSessionsInput struct {
	UsageFilterInput
	Limit int `query:"limit" minimum:"0" maximum:"100" default:"20" doc:"Maximum number of sessions"`
}

type usageComparisonInput struct {
	UsageFilterInput
	CurrentCost float64 `query:"current_cost" required:"true" doc:"Current period total cost"`
}

type usagePairwiseComparisonInput struct {
	UsageFilterInput
	LeftDimension  string `query:"left_dimension" required:"true" doc:"Left-side comparison dimension"`
	LeftValue      string `query:"left_value" required:"true" doc:"Left-side comparison value"`
	RightDimension string `query:"right_dimension" required:"true" doc:"Right-side comparison dimension"`
	RightValue     string `query:"right_value" required:"true" doc:"Right-side comparison value"`
}

// usageRequestFromInput maps the HTTP query-param struct to the
// transport-neutral service.UsageRequest.
func usageRequestFromInput(in UsageFilterInput) service.UsageRequest {
	return service.UsageRequest{
		From:             in.From,
		To:               in.To,
		Timezone:         in.Timezone,
		Agent:            in.Agent,
		Project:          in.Project,
		Machine:          in.Machine,
		GitBranch:        in.GitBranch,
		ExcludeProject:   in.ExcludeProject,
		ExcludeAgent:     in.ExcludeAgent,
		ExcludeModel:     in.ExcludeModel,
		Model:            in.Model,
		MinUserMessages:  in.MinUserMessages,
		ActiveSince:      in.ActiveSince,
		Termination:      in.Termination,
		IncludeOneShot:   in.IncludeOneShot,
		IncludeAutomated: in.IncludeAutomated,
		NoDefaultRange:   in.NoDefaultRange,
		Breakdowns:       &in.Breakdowns,
		SessionCounts:    &in.SessionCounts,
	}
}

func usagePairwiseRequestFromInput(
	in usagePairwiseComparisonInput,
) (service.UsagePairwiseComparisonRequest, error) {
	if in.LeftDimension == "" || in.LeftValue == "" {
		return service.UsagePairwiseComparisonRequest{}, &service.UsageInputError{
			Msg: "left side requires left_dimension and left_value",
		}
	}
	if in.RightDimension == "" || in.RightValue == "" {
		return service.UsagePairwiseComparisonRequest{}, &service.UsageInputError{
			Msg: "right side requires right_dimension and right_value",
		}
	}
	return service.UsagePairwiseComparisonRequest{
		UsageRequest:   usageRequestFromInput(in.UsageFilterInput),
		LeftDimension:  in.LeftDimension,
		LeftValue:      in.LeftValue,
		RightDimension: in.RightDimension,
		RightValue:     in.RightValue,
	}, nil
}

// usageFilterFromInput validates and builds a db.UsageFilter via the
// shared service validator (the single source of truth, also used by the
// usage-summary seam method), mapping a validation failure to HTTP 400.
func usageFilterFromInput(in UsageFilterInput) (db.UsageFilter, error) {
	f, err := service.BuildUsageFilter(usageRequestFromInput(in))
	if err != nil {
		var ue *service.UsageInputError
		if errors.As(err, &ue) {
			return db.UsageFilter{}, apiError(http.StatusBadRequest, ue.Msg)
		}
		return db.UsageFilter{}, err
	}
	return f, nil
}

func (s *Server) humaUsageSummary(
	ctx context.Context,
	in *UsageFilterInput,
) (*jsonOutput[UsageSummaryResponse], error) {
	res, err := s.sessions.UsageSummary(ctx, usageRequestFromInput(*in))
	if err != nil {
		var ue *service.UsageInputError
		if errors.As(err, &ue) {
			return nil, apiError(http.StatusBadRequest, ue.Msg)
		}
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage summary error", err)
	}
	return &jsonOutput[UsageSummaryResponse]{
		Body: usageSummaryResponseFromService(res),
	}, nil
}

func (s *Server) humaUsageComparison(
	ctx context.Context,
	in *usageComparisonInput,
) (*jsonOutput[Comparison], error) {
	f, err := usageFilterFromInput(in.UsageFilterInput)
	if err != nil {
		return nil, err
	}
	if in.NoDefaultRange && (f.From == "" || f.To == "") {
		return nil, apiError(
			http.StatusBadRequest,
			"usage comparison requires from and to when no_default_range is true",
		)
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

func (s *Server) humaUsagePairwiseComparison(
	ctx context.Context,
	in *usagePairwiseComparisonInput,
) (*jsonOutput[service.UsagePairwiseComparisonResponse], error) {
	req, err := usagePairwiseRequestFromInput(*in)
	if err != nil {
		var ue *service.UsageInputError
		if errors.As(err, &ue) {
			return nil, apiError(http.StatusBadRequest, ue.Msg)
		}
		return nil, err
	}
	comparison, err := s.sessions.UsagePairwiseComparison(ctx, req)
	if err != nil {
		var ue *service.UsageInputError
		if errors.As(err, &ue) {
			return nil, apiError(http.StatusBadRequest, ue.Msg)
		}
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("usage pairwise comparison error", err)
	}
	return &jsonOutput[service.UsagePairwiseComparisonResponse]{
		Body: *comparison,
	}, nil
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
		GitBranch:        f.GitBranch,
		Model:            f.Model,
		ExcludeProject:   f.ExcludeProject,
		ExcludeAgent:     f.ExcludeAgent,
		ExcludeModel:     f.ExcludeModel,
		Timezone:         f.Timezone,
		MinUserMessages:  f.MinUserMessages,
		ExcludeOneShot:   f.ExcludeOneShot,
		ExcludeAutomated: f.ExcludeAutomated,
		ActiveSince:      f.ActiveSince,
		Termination:      f.Termination,
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
