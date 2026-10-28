// ABOUTME: The get_session_usage tool: one session's token usage and cost,
// ABOUTME: the same document `agentsview session usage` prints.
package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

const (
	defaultUsageBreakdownRows = 20
	maxUsageBreakdownRows     = 200
)

type sessionUsageIn struct {
	SessionID       string `json:"session_id" jsonschema:"The session to report: a full stored session ID or a raw UUID that matches one session."`
	OwnOnly         bool   `json:"own_only,omitempty" jsonschema:"Report only this session's own transcript. Default false: totals, models, and breakdown also include the subagent sessions it started."`
	BreakdownSort   string `json:"breakdown_sort,omitempty" jsonschema:"Order of the breakdown rows: ordinal (default, chronological) or cost, largest first."`
	BreakdownLimit  int    `json:"breakdown_limit,omitempty" jsonschema:"Breakdown rows per page, default 20, max 200."`
	BreakdownCursor int    `json:"breakdown_cursor,omitempty" jsonschema:"Pagination cursor from a previous breakdown_next_cursor."`
}

// sessionUsageOut carries the fields of `agentsview session usage --format
// json`, a per-model summary of the breakdown, and one page of breakdown
// rows. It leaves out the deprecated cost_usd alias and the CLI-only
// server_running and refresh_incomplete flags.
type sessionUsageOut struct {
	SessionID           string                          `json:"session_id"`
	WebURL              string                          `json:"web_url,omitempty" jsonschema:"Browser URL for this session; use this URL when linking to it."`
	Agent               string                          `json:"agent"`
	Project             string                          `json:"project"`
	TotalOutputTokens   int                             `json:"total_output_tokens"`
	PeakContextTokens   int                             `json:"peak_context_tokens" jsonschema:"Largest context one request used: input plus cache tokens."`
	HasTokenData        bool                            `json:"has_token_data"`
	Cost                money.Money                     `json:"cost" jsonschema:"Session cost. Meaningful only when has_cost is true."`
	HasCost             bool                            `json:"has_cost" jsonschema:"True when the agent reported the cost or every model has a price. False when unpriced_models is not empty."`
	CostSource          export.CostSource               `json:"cost_source,omitempty" jsonschema:"computed (estimated from model pricing), reported (by the agent), or mixed."`
	AICredits           float64                         `json:"ai_credits,omitzero"`
	Models              []string                        `json:"models"`
	UnpricedModels      []string                        `json:"unpriced_models,omitempty" jsonschema:"Models with no known price. Their breakdown rows have has_cost false."`
	SubagentCount       int                             `json:"subagent_count,omitzero" jsonschema:"Subagent sessions included in the figures."`
	PlanUsage           *db.PlanUsageView               `json:"plan_usage,omitempty"`
	CaptureCoverage     *db.CaptureCoverageView         `json:"capture_coverage,omitempty"`
	ByModel             []service.SessionUsageModel     `json:"by_model" jsonschema:"Breakdown rows summed per model, most expensive first. A model's cost covers only its priced rows."`
	BreakdownCount      int                             `json:"breakdown_count" jsonschema:"Breakdown rows across all pages."`
	Breakdown           []db.SessionUsageBreakdownEntry `json:"breakdown" jsonschema:"One page of usage rows with input, output, and cache tokens and cost. Pass a row's message_ordinal to get_messages as around to read that turn."`
	BreakdownNextCursor *int                            `json:"breakdown_next_cursor,omitempty"`
}

func (t *toolset) sessionUsage(
	ctx context.Context, _ *mcp.CallToolRequest, in sessionUsageIn,
) (*mcp.CallToolResult, sessionUsageOut, error) {
	if in.BreakdownCursor < 0 {
		return nil, sessionUsageOut{}, errors.New("breakdown_cursor must not be negative")
	}
	byCost := false
	switch in.BreakdownSort {
	case "", "ordinal":
	case "cost":
		byCost = true
	default:
		return nil, sessionUsageOut{}, errors.New("breakdown_sort must be ordinal or cost")
	}
	detail, err := t.resolveSession(ctx, in.SessionID)
	if err != nil {
		return nil, sessionUsageOut{}, err
	}
	report, err := t.svc.SessionUsage(ctx, service.SessionUsageRequest{
		ID: detail.ID, OwnOnly: in.OwnOnly,
	})
	if err != nil {
		return nil, sessionUsageOut{}, err
	}
	if report == nil {
		return nil, sessionUsageOut{}, fmt.Errorf("session not found: %s", in.SessionID)
	}
	byModel, err := report.ByModel()
	if err != nil {
		return nil, sessionUsageOut{}, err
	}
	rows := slices.Clone(report.Breakdown)
	if byCost {
		slices.SortStableFunc(rows, func(a, b db.SessionUsageBreakdownEntry) int {
			return cmp.Compare(b.Cost.Microdollars, a.Cost.Microdollars)
		})
	}
	limit := clampLimit(in.BreakdownLimit, defaultUsageBreakdownRows, maxUsageBreakdownRows)
	start := min(in.BreakdownCursor, len(rows))
	end := min(start+limit, len(rows))
	out := sessionUsageOut{
		SessionID: report.SessionID, WebURL: detail.WebURL,
		Agent: report.Agent, Project: report.Project,
		TotalOutputTokens: report.TotalOutputTokens, PeakContextTokens: report.PeakContextTokens,
		HasTokenData: report.HasTokenData, Cost: report.Cost, HasCost: report.HasCost,
		CostSource: report.CostSource, AICredits: report.AICredits,
		Models: report.Models, UnpricedModels: report.UnpricedModels,
		SubagentCount: report.SubagentCount,
		PlanUsage:     report.Plan, CaptureCoverage: report.Coverage,
		ByModel: byModel, BreakdownCount: report.BreakdownCount,
		Breakdown: rows[start:end],
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	if end < len(rows) {
		out.BreakdownNextCursor = &end
	}
	return nil, out, nil
}
