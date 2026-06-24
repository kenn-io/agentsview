package server

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// Comparison holds the prior-period cost comparison returned by
// GET /api/v1/usage/comparison.
type Comparison struct {
	PriorFrom      string  `json:"priorFrom"`
	PriorTo        string  `json:"priorTo"`
	PriorTotalCost float64 `json:"priorTotalCost"`
	DeltaPct       float64 `json:"deltaPct"`
}

// UsageSummaryResponse preserves the public OpenAPI schema for
// GET /api/v1/usage/summary while the handler delegates assembly to
// the transport-neutral service layer.
type UsageSummaryResponse struct {
	From          string                 `json:"from"`
	To            string                 `json:"to"`
	Totals        db.UsageTotals         `json:"totals"`
	Daily         []db.DailyUsageEntry   `json:"daily"`
	ProjectTotals []service.ProjectTotal `json:"projectTotals"`
	ModelTotals   []service.ModelTotal   `json:"modelTotals"`
	AgentTotals   []service.AgentTotal   `json:"agentTotals"`
	SessionCounts db.UsageSessionCounts  `json:"sessionCounts"`
	CacheStats    service.CacheStats     `json:"cacheStats"`
	Comparison    *Comparison            `json:"comparison,omitempty"`
}

func usageSummaryResponseFromService(
	res *service.UsageSummaryResult,
) UsageSummaryResponse {
	return UsageSummaryResponse{
		From:          res.From,
		To:            res.To,
		Totals:        res.Totals,
		Daily:         res.Daily,
		ProjectTotals: res.ProjectTotals,
		ModelTotals:   res.ModelTotals,
		AgentTotals:   res.AgentTotals,
		SessionCounts: res.SessionCounts,
		CacheStats:    res.CacheStats,
	}
}
