// ABOUTME: Per-session usage documents shared by `session usage`, the MCP
// ABOUTME: get_session_usage tool, and every SessionService backend.
package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
)

// SessionUsageRequest selects one session's usage. ID must be a stored
// session ID; callers resolve raw IDs first. OwnOnly reports only the
// session's own transcript. Otherwise the result folds in every
// delegated subagent, as SessionUsageWithSubagents describes.
type SessionUsageRequest struct {
	ID      string
	OwnOnly bool
}

// SessionUsageReport is one session's token usage and cost estimate with
// its full per-row breakdown. Plan and Coverage expose the session's plan
// usage row the way the HTTP usage response does.
type SessionUsageReport struct {
	db.SessionUsage
	Plan     *db.PlanUsageView       `json:"plan_usage,omitempty"`
	Coverage *db.CaptureCoverageView `json:"capture_coverage,omitempty"`
}

// NewSessionUsageReport wraps a usage result read from a store. It fills
// an empty agent from the session ID prefix.
func NewSessionUsageReport(u *db.SessionUsage) *SessionUsageReport {
	if u.Agent == "" {
		if def, ok := parser.AgentByPrefix(u.SessionID); ok {
			u.Agent = string(def.Type)
		}
	}
	return &SessionUsageReport{
		SessionUsage: *u,
		Plan:         u.PlanUsage.PlanUsageView(),
		Coverage:     u.PlanUsage.CaptureCoverageView(),
	}
}

// LoadSessionUsage reads one session's usage from store with the full
// breakdown. It returns nil when the session does not exist. Pricing is
// whatever store already holds; callers refresh it first when they can.
func LoadSessionUsage(
	ctx context.Context, store db.Store, req SessionUsageRequest,
) (*SessionUsageReport, error) {
	var (
		u   *db.SessionUsage
		err error
	)
	if req.OwnOnly {
		u, err = store.GetSessionUsage(ctx, req.ID, true)
	} else {
		u, err = SessionUsageWithSubagents(ctx, store, req.ID, true)
	}
	if err != nil || u == nil {
		return nil, err
	}
	return NewSessionUsageReport(u), nil
}

// SessionUsageModel sums one model's breakdown rows. HasCost is false
// when any of the model's rows has no price. Cost then covers the priced
// rows plus known fees, such as web searches, on the unpriced ones.
type SessionUsageModel struct {
	Model                    string      `json:"model"`
	Rows                     int         `json:"rows"`
	InputTokens              int         `json:"input_tokens"`
	OutputTokens             int         `json:"output_tokens"`
	CacheCreationInputTokens int         `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int         `json:"cache_read_input_tokens"`
	WebSearchRequests        int         `json:"web_search_requests,omitzero"`
	Cost                     money.Money `json:"cost"`
	HasCost                  bool        `json:"has_cost"`
}

// ByModel sums the report's breakdown rows per model, most expensive
// first, then by model name.
func (r *SessionUsageReport) ByModel() ([]SessionUsageModel, error) {
	index := map[string]int{}
	out := []SessionUsageModel{}
	for _, row := range r.Breakdown {
		i, ok := index[row.Model]
		if !ok {
			i = len(out)
			index[row.Model] = i
			out = append(out, SessionUsageModel{Model: row.Model, HasCost: true})
		}
		m := &out[i]
		m.Rows++
		m.InputTokens += row.InputTokens
		m.OutputTokens += row.OutputTokens
		m.CacheCreationInputTokens += row.CacheCreationInputTokens
		m.CacheReadInputTokens += row.CacheReadInputTokens
		m.WebSearchRequests += row.WebSearchRequests
		if !row.HasCost {
			m.HasCost = false
		}
		cost, err := money.Add(m.Cost, row.Cost)
		if err != nil {
			return nil, fmt.Errorf("summing %s cost: %w", row.Model, err)
		}
		m.Cost = cost
	}
	slices.SortStableFunc(out, func(a, b SessionUsageModel) int {
		return cmp.Or(
			cmp.Compare(b.Cost.Microdollars, a.Cost.Microdollars),
			cmp.Compare(a.Model, b.Model),
		)
	})
	return out, nil
}
