// ABOUTME: Usage-summary request/response types, validation, and the
// ABOUTME: fold/cache aggregation shared by both SessionService backends.
package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
)

// UsageRequest is the transport-neutral usage-summary input. Fields use
// the include_* polarity of the HTTP query parameters; BuildUsageFilter
// inverts them to the db layer's exclude_* form.
type UsageRequest struct {
	From              string `json:"from,omitempty"`
	To                string `json:"to,omitempty"`
	Timezone          string `json:"timezone,omitempty"`
	Agent             string `json:"agent,omitempty"`
	Project           string `json:"project,omitempty"`
	Machine           string `json:"machine,omitempty"`
	GitBranch         string `json:"git_branch,omitempty"`
	ExcludeProject    string `json:"exclude_project,omitempty"`
	ExcludeProjectKey string `json:"exclude_project_key,omitempty"`
	ExcludeAgent      string `json:"exclude_agent,omitempty"`
	ExcludeModel      string `json:"exclude_model,omitempty"`
	Model             string `json:"model,omitempty"`
	MinUserMessages   int    `json:"min_user_messages,omitempty"`
	ActiveSince       string `json:"active_since,omitempty"`
	Termination       string `json:"termination,omitempty"`
	IncludeOneShot    bool   `json:"include_one_shot,omitempty"`
	IncludeAutomated  bool   `json:"include_automated,omitempty"`
	NoDefaultRange    bool   `json:"no_default_range,omitempty"`
	Breakdowns        *bool  `json:"breakdowns,omitempty"`
	SessionCounts     *bool  `json:"session_counts,omitempty"`
	// ProjectLabels and ExcludeProjectLabels carry exact internal labels
	// resolved from opaque keys. Unlike the public string fields, they are
	// never parsed as comma-separated transport input.
	ProjectLabels        []string `json:"-"`
	ExcludeProjectLabels []string `json:"-"`
}

// ResolveUsageProjectKeys translates opaque project-label keys back to the
// source labels understood by storage queries. The translation stays inside
// the local data boundary; callers never need the raw label carried by an
// unsafe path-like project name.
func ResolveUsageProjectKeys(
	ctx context.Context, store db.Store, req UsageRequest,
) (UsageRequest, error) {
	if req.ExcludeProjectKey == "" {
		return req, nil
	}
	resolved, err := resolveUsageProjectKeyLabels(
		ctx, store, req.ExcludeProjectKey,
	)
	if err != nil {
		return UsageRequest{}, err
	}
	req.ExcludeProjectLabels = append(req.ExcludeProjectLabels, resolved...)
	req.ExcludeProjectKey = ""
	return req, nil
}

func resolveUsageProjectKeyLabels(
	ctx context.Context, store db.Store, keys string,
) ([]string, error) {
	labels, err := store.GetActiveProjectLabels(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := store.BuildProjectIdentityMap(ctx, labels)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]string, len(catalog))
	for label, entry := range catalog {
		if entry.ProjectKey != "" {
			byKey[entry.ProjectKey] = label
		}
	}
	resolved := make([]string, 0)
	for _, key := range splitCSVTokens(keys) {
		label, ok := byKey[key]
		if !ok {
			return nil, &UsageInputError{
				Code: UsageErrorCodeUnknownProjectKey,
				Msg:  "unknown project key",
			}
		}
		resolved = append(resolved, label)
	}
	return resolved, nil
}

func ResolveUsagePairwiseProjectKeys(
	ctx context.Context, store db.Store, req UsagePairwiseComparisonRequest,
) (UsagePairwiseComparisonRequest, error) {
	resolvedBase, err := ResolveUsageProjectKeys(ctx, store, req.UsageRequest)
	if err != nil {
		return UsagePairwiseComparisonRequest{}, err
	}
	req.UsageRequest = resolvedBase
	req.LeftProjectLabels, err = resolvePairwiseProjectLabels(
		ctx, store, req.LeftDimension, req.LeftValue,
	)
	if err != nil {
		return UsagePairwiseComparisonRequest{}, err
	}
	req.RightProjectLabels, err = resolvePairwiseProjectLabels(
		ctx, store, req.RightDimension, req.RightValue,
	)
	if err != nil {
		return UsagePairwiseComparisonRequest{}, err
	}
	return req, nil
}

func resolvePairwiseProjectLabels(
	ctx context.Context, store db.Store, dimension, value string,
) ([]string, error) {
	if dimension != "project" || !strings.HasPrefix(value, "pl1:sha256:") {
		return nil, nil
	}
	return resolveUsageProjectKeyLabels(ctx, store, value)
}

// UsageInputError flags an invalid usage filter (bad timezone, date, or
// range). Transports map it to a 400-style client error; it mirrors
// db.SearchInputError so handlers can errors.As it.
const UsageErrorCodeUnknownProjectKey = "unknown_project_key"

type UsageInputError struct {
	Code string
	Msg  string
}

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
		From:    from,
		To:      to,
		Agent:   req.Agent,
		Project: req.Project,
		ProjectLabels: mergeResolvedProjectLabels(
			req.Project, req.ProjectLabels,
		),
		Machine:        req.Machine,
		GitBranch:      req.GitBranch,
		ExcludeProject: req.ExcludeProject,
		ExcludeProjectLabels: mergeResolvedProjectLabels(
			req.ExcludeProject, req.ExcludeProjectLabels,
		),
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

func mergeResolvedProjectLabels(raw string, resolved []string) []string {
	if resolved == nil {
		return nil
	}
	return append(splitCSVTokens(raw), resolved...)
}

// ProjectTotal holds range-wide token and cost totals per project.
type ProjectTotal struct {
	ProjectKey          string      `json:"project_key"`
	Project             string      `json:"project"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// ModelTotal holds range-wide token and cost totals per model.
type ModelTotal struct {
	Model               string      `json:"model"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// AgentTotal holds range-wide token and cost totals per agent.
type AgentTotal struct {
	Agent               string      `json:"agent"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// CacheStats summarizes cache hit/miss for the period.
type CacheStats struct {
	CacheReadTokens     int         `json:"cacheReadTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	UncachedInputTokens int         `json:"uncachedInputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	HitRate             float64     `json:"hitRate"`
	SavingsVsUncached   money.Money `json:"savingsVsUncached"`
}

const UnsupportedUsageKindNoTokenData = "no-token-data"
const UnsupportedUsageKindCopilotNoTokenData = "copilot-no-token-data"

// UnsupportedUsageKindForAgentFilter returns the unsupported-usage
// kind for an agent filter whose agents record no per-message token
// data: the Copilot-specific kind when the filter selects only
// Copilot-family agents, and the generic kind otherwise. Copilot
// branding keys on agent identity, not on the AI-credits capability,
// so another credits-denominated agent degrades to the generic kind
// instead of being described as Copilot.
func UnsupportedUsageKindForAgentFilter(agentFilter string) string {
	if parser.AgentFilterIsCopilot(agentFilter) {
		return UnsupportedUsageKindCopilotNoTokenData
	}
	return UnsupportedUsageKindNoTokenData
}

type UnsupportedUsage struct {
	Kind string `json:"kind"`
}

// UsageSummaryResult is the transport-neutral usage-summary response, the
// JSON shape served by GET /api/v1/usage/summary. The prior-period
// comparison is a separate endpoint, so it is intentionally absent here.
type UsageSummaryResult struct {
	SchemaVersion    int                               `json:"schema_version,omitempty"`
	Pricing          *export.PricingBlock              `json:"pricing,omitempty"`
	Projects         map[string]export.ProjectMapEntry `json:"projects"`
	From             string                            `json:"from"`
	To               string                            `json:"to"`
	Totals           db.UsageTotals                    `json:"totals"`
	Daily            []db.DailyUsageEntry              `json:"daily"`
	ProjectTotals    []ProjectTotal                    `json:"projectTotals"`
	ModelTotals      []ModelTotal                      `json:"modelTotals"`
	AgentTotals      []AgentTotal                      `json:"agentTotals"`
	SessionCounts    db.UsageSessionCounts             `json:"sessionCounts"`
	CacheStats       CacheStats                        `json:"cacheStats"`
	UnsupportedUsage *UnsupportedUsage                 `json:"unsupportedUsage,omitempty"`
}

// UsagePairwiseComparisonSide holds aggregate and derived
// metrics for one side of a pairwise comparison.
type UsagePairwiseComparisonSide struct {
	TotalCost           money.Money  `json:"totalCost"`
	InputTokens         int          `json:"inputTokens"`
	OutputTokens        int          `json:"outputTokens"`
	CacheCreationTokens int          `json:"cacheCreationTokens"`
	CacheReadTokens     int          `json:"cacheReadTokens"`
	TotalTokens         int          `json:"totalTokens"`
	SessionCount        int          `json:"sessionCount"`
	CostPerSession      *money.Money `json:"costPerSession,omitempty"`
	TokensPerSession    *float64     `json:"tokensPerSession,omitempty"`
}

// UsagePairwiseComparisonDelta reports absolute and relative differences
// for each metric between right and left sides.
type UsagePairwiseComparisonDelta struct {
	TotalCostDelta          money.Money  `json:"totalCostDelta"`
	TotalCostDeltaRatio     *float64     `json:"totalCostDeltaRatio"`
	InputTokensDelta        int          `json:"inputTokensDelta"`
	InputTokensDeltaRatio   *float64     `json:"inputTokensDeltaRatio"`
	OutputTokensDelta       int          `json:"outputTokensDelta"`
	OutputTokensDeltaRatio  *float64     `json:"outputTokensDeltaRatio"`
	CacheCreationDelta      int          `json:"cacheCreationDelta"`
	CacheCreationDeltaRatio *float64     `json:"cacheCreationDeltaRatio"`
	CacheReadDelta          int          `json:"cacheReadDelta"`
	CacheReadDeltaRatio     *float64     `json:"cacheReadDeltaRatio"`
	TotalTokensDelta        int          `json:"totalTokensDelta"`
	TotalTokensDeltaRatio   *float64     `json:"totalTokensDeltaRatio"`
	SessionCountDelta       int          `json:"sessionCountDelta"`
	SessionCountDeltaRatio  *float64     `json:"sessionCountDeltaRatio"`
	CostPerSessionDelta     *money.Money `json:"costPerSessionDelta"`
	CostPerSessionRatio     *float64     `json:"costPerSessionRatio"`
	TokensPerSessionDelta   *float64     `json:"tokensPerSessionDelta"`
	TokensPerSessionRatio   *float64     `json:"tokensPerSessionRatio"`
}

// UsagePairwiseComparisonResponse is the backend-computed response
// for model/project pairwise usage comparisons.
type UsagePairwiseComparisonResponse struct {
	Left   UsagePairwiseComparisonSide  `json:"left"`
	Right  UsagePairwiseComparisonSide  `json:"right"`
	Deltas UsagePairwiseComparisonDelta `json:"deltas"`
}

// UsagePairwiseComparisonRequest holds shared usage filters plus one
// extra include filter per side.
type UsagePairwiseComparisonRequest struct {
	UsageRequest
	LeftDimension  string `json:"left_dimension,omitempty"`
	LeftValue      string `json:"left_value,omitempty"`
	RightDimension string `json:"right_dimension,omitempty"`
	RightValue     string `json:"right_value,omitempty"`
	// Project label slices are populated only after resolving opaque keys and
	// preserve exact labels that cannot round-trip through CSV strings.
	LeftProjectLabels  []string `json:"-"`
	RightProjectLabels []string `json:"-"`
}

// buildUsageSummary assembles a UsageSummaryResult from a daily-usage
// query result over the [from, to] range.
func buildUsageSummary(
	f db.UsageFilter, result db.DailyUsageResult,
) *UsageSummaryResult {
	out := &UsageSummaryResult{
		SchemaVersion: result.SchemaVersion,
		Pricing:       result.Pricing,
		Projects:      result.Projects,
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
			pt, ok := m[pb.ProjectKey]
			if !ok {
				pt = &ProjectTotal{
					ProjectKey: pb.ProjectKey,
					Project:    pb.Project,
				}
				m[pb.ProjectKey] = pt
			}
			pt.InputTokens += pb.InputTokens
			pt.OutputTokens += pb.OutputTokens
			pt.CacheCreationTokens += pb.CacheCreationTokens
			pt.CacheReadTokens += pb.CacheReadTokens
			pt.Cost = money.MustAdd(pt.Cost, pb.Cost)
		}
	}
	out := make([]ProjectTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost.Microdollars != out[j].Cost.Microdollars {
			return out[i].Cost.Microdollars > out[j].Cost.Microdollars
		}
		if out[i].ProjectKey != out[j].ProjectKey {
			return out[i].ProjectKey < out[j].ProjectKey
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
			mt.Cost = money.MustAdd(mt.Cost, mb.Cost)
		}
	}
	out := make([]ModelTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost.Microdollars != out[j].Cost.Microdollars {
			return out[i].Cost.Microdollars > out[j].Cost.Microdollars
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
			at.Cost = money.MustAdd(at.Cost, ab.Cost)
		}
	}
	out := make([]AgentTotal, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost.Microdollars != out[j].Cost.Microdollars {
			return out[i].Cost.Microdollars > out[j].Cost.Microdollars
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

func BuildUsagePairwiseComparisonResult(
	left db.DailyUsageResult,
	right db.DailyUsageResult,
) UsagePairwiseComparisonResponse {
	leftSide := usagePairwiseSideFromResult(left)
	rightSide := usagePairwiseSideFromResult(right)
	return UsagePairwiseComparisonResponse{
		Left:   leftSide,
		Right:  rightSide,
		Deltas: pairwiseDeltas(leftSide, rightSide),
	}
}

func BuildUsagePairwiseFilters(
	req UsagePairwiseComparisonRequest,
) (
	db.UsageFilter,
	bool,
	db.UsageFilter,
	bool,
	error,
) {
	base, err := BuildUsageFilter(req.UsageRequest)
	if err != nil {
		return db.UsageFilter{}, false, db.UsageFilter{}, false, err
	}

	left, err := applyPairwiseDimension(
		base,
		req.LeftDimension,
		req.LeftValue,
		req.LeftProjectLabels,
		"left",
	)
	if err != nil {
		return db.UsageFilter{}, false, db.UsageFilter{}, false, err
	}
	right, err := applyPairwiseDimension(
		base,
		req.RightDimension,
		req.RightValue,
		req.RightProjectLabels,
		"right",
	)
	if err != nil {
		return db.UsageFilter{}, false, db.UsageFilter{}, false, err
	}
	return left.filter, left.empty, right.filter, right.empty, nil
}

func intersectCSV(base, add string) (string, bool) {
	out, ok := intersectValues(splitCSVTokens(base), splitCSVTokens(add))
	return joinCSVTokens(out), ok
}

func intersectValues(base, add []string) ([]string, bool) {
	if len(add) == 0 {
		return base, len(base) > 0
	}
	if len(base) == 0 {
		return append([]string(nil), add...), true
	}
	addSet := map[string]struct{}{}
	for _, token := range add {
		addSet[token] = struct{}{}
	}
	out := make([]string, 0)
	seen := map[string]struct{}{}
	for _, token := range base {
		if _, ok := addSet[token]; !ok {
			continue
		}
		if _, seenOk := seen[token]; seenOk {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func splitCSVTokens(raw string) []string {
	out := make([]string, 0)
	for token := range strings.SplitSeq(raw, ",") {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func joinCSVTokens(tokens []string) string {
	return strings.Join(tokens, ",")
}

type pairwiseFilterResult struct {
	filter db.UsageFilter
	empty  bool
}

func applyPairwiseDimension(
	base db.UsageFilter, dimension, value string, projectLabels []string,
	label string,
) (pairwiseFilterResult, error) {
	filter := base
	if value == "" {
		return pairwiseFilterResult{},
			&UsageInputError{Msg: label + "_value is required"}
	}
	var ok bool
	switch dimension {
	case "model":
		filter.Model, ok = intersectCSV(filter.Model, value)
		return pairwiseFilterResult{filter: filter, empty: !ok}, nil
	case "project":
		if projectLabels == nil {
			projectLabels = splitCSVTokens(value)
		}
		filter.ProjectLabels, ok = intersectValues(
			filter.ProjectFilterLabels(), projectLabels,
		)
		filter.Project = joinCSVTokens(filter.ProjectLabels)
		return pairwiseFilterResult{filter: filter, empty: !ok}, nil
	case "":
		return pairwiseFilterResult{},
			&UsageInputError{Msg: label + "_dimension is required"}
	default:
		return pairwiseFilterResult{},
			&UsageInputError{
				Msg: label + "_dimension must be model or project",
			}
	}
}

func safePerTurnDenominator(count int) bool {
	return count > 0
}

func maybeFloatRatio(left, delta float64) *float64 {
	if left == 0 {
		return nil
	}
	r := delta / left
	return &r
}

func usagePairwiseSideFromResult(r db.DailyUsageResult) UsagePairwiseComparisonSide {
	total := r.Totals
	side := UsagePairwiseComparisonSide{
		TotalCost:           total.TotalCost,
		InputTokens:         total.InputTokens,
		OutputTokens:        total.OutputTokens,
		CacheCreationTokens: total.CacheCreationTokens,
		CacheReadTokens:     total.CacheReadTokens,
		SessionCount:        r.SessionCounts.Total,
	}
	side.TotalTokens = side.InputTokens + side.OutputTokens +
		side.CacheCreationTokens + side.CacheReadTokens
	if safePerTurnDenominator(r.SessionCounts.Total) {
		costPerSession, err := money.Divide(side.TotalCost, int64(r.SessionCounts.Total))
		if err != nil {
			panic(err)
		}
		tokensPerSession := float64(side.TotalTokens) / float64(r.SessionCounts.Total)
		side.CostPerSession = &costPerSession
		side.TokensPerSession = &tokensPerSession
	}
	return side
}

func pairwiseDeltas(left, right UsagePairwiseComparisonSide) UsagePairwiseComparisonDelta {
	costPerSessionDelta, costPerSessionRatio := deltaWithMoneyRatio(
		left.CostPerSession, right.CostPerSession,
	)
	tokensPerSessionDelta, tokensPerSessionRatio := deltaWithRatio(
		left.TokensPerSession, right.TokensPerSession,
	)
	totalCostDelta := money.MustSub(right.TotalCost, left.TotalCost)
	inputTokensDelta := right.InputTokens - left.InputTokens
	outputTokensDelta := right.OutputTokens - left.OutputTokens
	cacheCreationDelta := right.CacheCreationTokens - left.CacheCreationTokens
	cacheReadDelta := right.CacheReadTokens - left.CacheReadTokens
	totalTokensDelta := right.TotalTokens - left.TotalTokens
	sessionCountDelta := right.SessionCount - left.SessionCount
	return UsagePairwiseComparisonDelta{
		TotalCostDelta:          totalCostDelta,
		TotalCostDeltaRatio:     maybeMoneyRatio(left.TotalCost, totalCostDelta),
		InputTokensDelta:        inputTokensDelta,
		InputTokensDeltaRatio:   maybeFloatRatio(float64(left.InputTokens), float64(inputTokensDelta)),
		OutputTokensDelta:       outputTokensDelta,
		OutputTokensDeltaRatio:  maybeFloatRatio(float64(left.OutputTokens), float64(outputTokensDelta)),
		CacheCreationDelta:      cacheCreationDelta,
		CacheCreationDeltaRatio: maybeFloatRatio(float64(left.CacheCreationTokens), float64(cacheCreationDelta)),
		CacheReadDelta:          cacheReadDelta,
		CacheReadDeltaRatio:     maybeFloatRatio(float64(left.CacheReadTokens), float64(cacheReadDelta)),
		TotalTokensDelta:        totalTokensDelta,
		TotalTokensDeltaRatio:   maybeFloatRatio(float64(left.TotalTokens), float64(totalTokensDelta)),
		SessionCountDelta:       sessionCountDelta,
		SessionCountDeltaRatio:  maybeFloatRatio(float64(left.SessionCount), float64(sessionCountDelta)),
		CostPerSessionDelta:     costPerSessionDelta,
		CostPerSessionRatio:     costPerSessionRatio,
		TokensPerSessionDelta:   tokensPerSessionDelta,
		TokensPerSessionRatio:   tokensPerSessionRatio,
	}
}

func deltaWithRatio(left, right *float64) (*float64, *float64) {
	if left == nil || right == nil {
		return nil, nil
	}
	delta := *right - *left
	return &delta, maybeFloatRatio(*left, delta)
}

func maybeMoneyRatio(left, delta money.Money) *float64 {
	if left.Microdollars == 0 {
		return nil
	}
	ratio := float64(delta.Microdollars) / float64(left.Microdollars)
	return &ratio
}

func deltaWithMoneyRatio(
	left, right *money.Money,
) (*money.Money, *float64) {
	if left == nil || right == nil {
		return nil, nil
	}
	delta := money.MustSub(*right, *left)
	return &delta, maybeMoneyRatio(*left, delta)
}
