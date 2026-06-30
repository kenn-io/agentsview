// ABOUTME: Usage-summary request/response types, validation, and the
// ABOUTME: fold/cache aggregation shared by both SessionService backends.
package service

import (
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
)

// UsageRequest is the transport-neutral usage-summary input. Fields use
// the include_* polarity of the HTTP query parameters; BuildUsageFilter
// inverts them to the db layer's exclude_* form.
type UsageRequest struct {
	From             string `json:"from,omitempty"`
	To               string `json:"to,omitempty"`
	Timezone         string `json:"timezone,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Project          string `json:"project,omitempty"`
	Machine          string `json:"machine,omitempty"`
	GitBranch        string `json:"git_branch,omitempty"`
	ExcludeProject   string `json:"exclude_project,omitempty"`
	ExcludeAgent     string `json:"exclude_agent,omitempty"`
	ExcludeModel     string `json:"exclude_model,omitempty"`
	Model            string `json:"model,omitempty"`
	MinUserMessages  int    `json:"min_user_messages,omitempty"`
	ActiveSince      string `json:"active_since,omitempty"`
	Termination      string `json:"termination,omitempty"`
	IncludeOneShot   bool   `json:"include_one_shot,omitempty"`
	IncludeAutomated bool   `json:"include_automated,omitempty"`
	NoDefaultRange   bool   `json:"no_default_range,omitempty"`
	Breakdowns       *bool  `json:"breakdowns,omitempty"`
	SessionCounts    *bool  `json:"session_counts,omitempty"`
}

// UsageInputError flags an invalid usage filter (bad timezone, date, or
// range). Transports map it to a 400-style client error; it mirrors
// db.SearchInputError so handlers can errors.As it.
type UsageInputError struct{ Msg string }

func (e *UsageInputError) Error() string { return e.Msg }

// BuildUsageFilter validates a UsageRequest and maps it to a
// db.UsageFilter. It is the single source of truth for usage filter
// validation, shared by the usage summary seam method and the server's
// comparison/top-sessions handlers. Date defaulting matches the
// server's analytics range (last 30 days through today, UTC) unless
// NoDefaultRange is set.
func BuildUsageFilter(req UsageRequest) (db.UsageFilter, error) {
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return db.UsageFilter{}, &UsageInputError{Msg: "invalid timezone: " + tz}
	}
	from, to := req.From, req.To
	if !req.NoDefaultRange {
		from, to = defaultUsageDateRange(req.From, req.To)
	}
	if (from != "" && !timeutil.IsValidDate(from)) ||
		(to != "" && !timeutil.IsValidDate(to)) {
		return db.UsageFilter{}, &UsageInputError{
			Msg: "invalid date format: use YYYY-MM-DD",
		}
	}
	if from != "" && to != "" && from > to {
		return db.UsageFilter{}, &UsageInputError{Msg: "from must not be after to"}
	}
	if req.ActiveSince != "" && !timeutil.IsValidTimestamp(req.ActiveSince) {
		return db.UsageFilter{}, &UsageInputError{
			Msg: "invalid active_since: use RFC3339 timestamp",
		}
	}
	breakdowns := true
	if req.Breakdowns != nil {
		breakdowns = *req.Breakdowns
	}
	sessionCounts := true
	if req.SessionCounts != nil {
		sessionCounts = *req.SessionCounts
	}
	return db.UsageFilter{
		From:              from,
		To:                to,
		Agent:             req.Agent,
		Project:           req.Project,
		Machine:           req.Machine,
		GitBranch:         req.GitBranch,
		ExcludeProject:    req.ExcludeProject,
		ExcludeAgent:      req.ExcludeAgent,
		ExcludeModel:      req.ExcludeModel,
		Model:             req.Model,
		Timezone:          tz,
		MinUserMessages:   req.MinUserMessages,
		ExcludeOneShot:    !req.IncludeOneShot,
		ExcludeAutomated:  !req.IncludeAutomated,
		ActiveSince:       req.ActiveSince,
		Termination:       req.Termination,
		Breakdowns:        breakdowns,
		SkipSessionCounts: !sessionCounts,
	}, nil
}

// defaultUsageDateRange fills an empty from/to with the last 30 days
// through today (UTC). It mirrors the server's defaultDateRange so the
// seam and the analytics handlers default identically.
func defaultUsageDateRange(from, to string) (string, string) {
	now := time.Now().UTC()
	if to == "" {
		to = now.Format("2006-01-02")
	}
	if from == "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			t = now
		}
		from = t.AddDate(0, 0, -30).Format("2006-01-02")
	}
	return from, to
}

// ProjectTotal holds range-wide token and cost totals per project.
type ProjectTotal struct {
	Project             string  `json:"project"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// ModelTotal holds range-wide token and cost totals per model.
type ModelTotal struct {
	Model               string  `json:"model"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// AgentTotal holds range-wide token and cost totals per agent.
type AgentTotal struct {
	Agent               string  `json:"agent"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// CacheStats summarizes cache hit/miss for the period.
type CacheStats struct {
	CacheReadTokens     int     `json:"cacheReadTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	UncachedInputTokens int     `json:"uncachedInputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	HitRate             float64 `json:"hitRate"`
	SavingsVsUncached   float64 `json:"savingsVsUncached"`
}

// UsageSummaryResult is the transport-neutral usage-summary response, the
// JSON shape served by GET /api/v1/usage/summary. The prior-period
// comparison is a separate endpoint, so it is intentionally absent here.
type UsageSummaryResult struct {
	From          string                `json:"from"`
	To            string                `json:"to"`
	Totals        db.UsageTotals        `json:"totals"`
	Daily         []db.DailyUsageEntry  `json:"daily"`
	ProjectTotals []ProjectTotal        `json:"projectTotals"`
	ModelTotals   []ModelTotal          `json:"modelTotals"`
	AgentTotals   []AgentTotal          `json:"agentTotals"`
	SessionCounts db.UsageSessionCounts `json:"sessionCounts"`
	CacheStats    CacheStats            `json:"cacheStats"`
}

// buildUsageSummary assembles a UsageSummaryResult from a daily-usage
// query result over the [from, to] range.
func buildUsageSummary(
	f db.UsageFilter, result db.DailyUsageResult,
) *UsageSummaryResult {
	out := &UsageSummaryResult{
		From:          f.From,
		To:            f.To,
		Totals:        result.Totals,
		Daily:         result.Daily,
		SessionCounts: result.SessionCounts,
		CacheStats:    computeCacheStats(result.Totals),
	}
	if f.Breakdowns {
		out.ProjectTotals = foldProjectTotals(result.Daily)
		out.ModelTotals = foldModelTotals(result.Daily)
		out.AgentTotals = foldAgentTotals(result.Daily)
	} else {
		out.ProjectTotals = []ProjectTotal{}
		out.ModelTotals = []ModelTotal{}
		out.AgentTotals = []AgentTotal{}
	}
	return out
}

// foldProjectTotals sums daily project breakdowns into range-wide totals
// sorted by cost descending.
func foldProjectTotals(daily []db.DailyUsageEntry) []ProjectTotal {
	m := make(map[string]*ProjectTotal)
	for _, d := range daily {
		for _, pb := range d.ProjectBreakdowns {
			pt, ok := m[pb.Project]
			if !ok {
				pt = &ProjectTotal{Project: pb.Project}
				m[pb.Project] = pt
			}
			pt.InputTokens += pb.InputTokens
			pt.OutputTokens += pb.OutputTokens
			pt.CacheCreationTokens += pb.CacheCreationTokens
			pt.CacheReadTokens += pb.CacheReadTokens
			pt.Cost += pb.Cost
		}
	}
	out := make([]ProjectTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Project < out[j].Project
	})
	return out
}

// foldModelTotals sums daily model breakdowns into range-wide totals
// sorted by cost descending.
func foldModelTotals(daily []db.DailyUsageEntry) []ModelTotal {
	m := make(map[string]*ModelTotal)
	for _, d := range daily {
		for _, mb := range d.ModelBreakdowns {
			mt, ok := m[mb.ModelName]
			if !ok {
				mt = &ModelTotal{Model: mb.ModelName}
				m[mb.ModelName] = mt
			}
			mt.InputTokens += mb.InputTokens
			mt.OutputTokens += mb.OutputTokens
			mt.CacheCreationTokens += mb.CacheCreationTokens
			mt.CacheReadTokens += mb.CacheReadTokens
			mt.Cost += mb.Cost
		}
	}
	out := make([]ModelTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// foldAgentTotals sums daily agent breakdowns into range-wide totals
// sorted by cost descending.
func foldAgentTotals(daily []db.DailyUsageEntry) []AgentTotal {
	m := make(map[string]*AgentTotal)
	for _, d := range daily {
		for _, ab := range d.AgentBreakdowns {
			at, ok := m[ab.Agent]
			if !ok {
				at = &AgentTotal{Agent: ab.Agent}
				m[ab.Agent] = at
			}
			at.InputTokens += ab.InputTokens
			at.OutputTokens += ab.OutputTokens
			at.CacheCreationTokens += ab.CacheCreationTokens
			at.CacheReadTokens += ab.CacheReadTokens
			at.Cost += ab.Cost
		}
	}
	out := make([]AgentTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// computeCacheStats derives cache hit/miss metrics from totals.
// SavingsVsUncached passes through totals.CacheSavings, which the DB
// layer computes per-message using each row's actual per-model rates,
// so mixed-model periods report the right net delta instead of a single
// hard-coded proxy rate.
func computeCacheStats(t db.UsageTotals) CacheStats {
	// Anthropic reports input_tokens as the NON-cached portion of the
	// input (cache_read and cache_creation are separate fields), so
	// UncachedInputTokens is just t.InputTokens directly.
	cs := CacheStats{
		CacheReadTokens:     t.CacheReadTokens,
		CacheCreationTokens: t.CacheCreationTokens,
		UncachedInputTokens: t.InputTokens,
		OutputTokens:        t.OutputTokens,
		SavingsVsUncached:   t.CacheSavings,
	}
	denominator := t.CacheReadTokens + t.InputTokens
	if denominator > 0 {
		cs.HitRate = float64(t.CacheReadTokens) / float64(denominator)
	}
	return cs
}
