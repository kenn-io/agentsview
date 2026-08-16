// Package activity aggregates a resolved time range of agent activity into a
// concurrency- and usage-oriented report. It operates on in-memory input
// streams supplied by a storage backend, so the same aggregation runs
// identically across SQLite, PostgreSQL, and DuckDB. Export contract types are
// referenced only for optional report metadata.
package activity

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// Params controls one range aggregation. RangeStart/RangeEnd are the resolved
// UTC bounds; EffectiveEnd clamps the end to now for an in-progress range
// (Partial); Bucket is the resolved timeline bucket size. They are copied
// verbatim from a resolved Query so the range/bucket logic lives only in the
// query engine.
type Params struct {
	RangeStart    time.Time
	RangeEnd      time.Time
	Loc           *time.Location
	EffectiveEnd  time.Time
	Partial       bool
	GapCapSeconds float64
	Bucket        BucketSpec
}

// SessionMeta is one candidate session whose window intersects the day.
type SessionMeta struct {
	SessionID   string
	Title       string
	Project     string
	Agent       string
	Machine     string
	StartedAt   string // RFC3339 or ""
	EndedAt     string // RFC3339 or ""
	IsAutomated bool   // automated (e.g. roborev) vs interactive session
}

// ActivityEvent is one timestamped message (backends send only timestamped rows).
type ActivityEvent struct {
	SessionID string
	Ordinal   int
	Timestamp string // RFC3339 (non-empty)
	Role      string // "user" | "assistant" | ...
	Model     string // "" when unknown
}

// UsageRow is one cost/token row from the usage-row union, with cost already
// computed by the backend (so cost logic stays in each backend, matching
// GetDailyUsage). Rows MUST be delivered in the timestamp, session ID, and
// message-ordinal survivor order required by the caller.
type UsageRow struct {
	SessionID           string // session receiving attribution
	SourceSessionID     string // transcript that supplied the surviving row
	Model               string
	Timestamp           string // ts, RFC3339 or ""
	Project             string
	Machine             string
	MessageOrdinal      int64 // COALESCE(message_ordinal, -1)
	UsageSource         string
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	// WebSearchRequests is how many billed Anthropic server-side web
	// searches the row's stored usage reports; it prices at a flat
	// per-request fee on top of tokens.
	WebSearchRequests int
	Cost              money.Money
	CostSource        export.CostSource
	SessionCost       *money.Money
	Priced            bool
	Contributes       bool
	Agent             string
	ClaudeMessageID   string
	ClaudeRequestID   string
	SourceUUID        string
	UsageDedupKey     string
}

// SessionUsageRows is a globally ordered, cross-session-deduplicated usage
// row set. RawOutputTokensBySession records output from every usage row before
// snapshot selection or cross-session deduplication. DeduplicatedOutputTokens
// records output removed from each session when an earlier transcript already
// represented the same usage.
// DiscardedContributingSessions records sessions whose snapshot or duplicate
// rows carried billable usage before they were removed.
type SessionUsageRows struct {
	Rows                          []UsageRow
	RawOutputTokensBySession      map[string]int
	DeduplicatedOutputTokens      map[string]int
	DiscardedContributingSessions map[string]struct{}
}

// UsageDataContributes reports whether normalized usage data represents spend.
func UsageDataContributes(
	hasExplicitCost bool,
	inputTokens, outputTokens, reasoningTokens int,
	cacheCreationTokens, cacheReadTokens, webSearchRequests int,
) bool {
	return hasExplicitCost || inputTokens != 0 || outputTokens != 0 ||
		reasoningTokens != 0 || cacheCreationTokens != 0 ||
		cacheReadTokens != 0 || webSearchRequests != 0
}

type UsageCostAllocation struct {
	Cost        money.Money
	CostSource  export.CostSource
	Priced      bool
	Contributes bool
}

// AllocateUsageCosts selects aggregate row costs. A session may carry one
// session total; when it does, that settlement replaces the session's row
// estimates and is distributed by their catalog-cost weights.
func AllocateUsageCosts(usage []UsageRow) []UsageCostAllocation {
	type sessionCost struct {
		carrier int
		cost    money.Money
		indices []int
	}
	allocated := make([]UsageCostAllocation, len(usage))
	sessionCosts := make(map[string]*sessionCost)
	for i, row := range usage {
		allocated[i] = UsageCostAllocation{
			Cost: row.Cost, CostSource: row.CostSource,
			Priced: row.Priced, Contributes: row.Contributes,
		}
		if row.SessionCost != nil {
			sessionCosts[row.SessionID] = &sessionCost{
				carrier: i,
				cost:    *row.SessionCost,
			}
		}
	}
	for i, row := range usage {
		selected := sessionCosts[row.SessionID]
		if selected == nil || !allocated[i].Contributes {
			continue
		}
		selected.indices = append(selected.indices, i)
	}
	for _, selected := range sessionCosts {
		if len(selected.indices) == 0 {
			allocated[selected.carrier] = UsageCostAllocation{
				Cost: selected.cost, CostSource: export.CostSourceReported,
				Priced: true, Contributes: true,
			}
			continue
		}
		weights := make([]money.Money, len(selected.indices))
		for i, index := range selected.indices {
			weights[i] = usage[index].Cost
		}
		costs := export.AllocateCostByWeight(selected.cost, weights)
		for i, index := range selected.indices {
			allocated[index] = UsageCostAllocation{
				Cost: costs[i], CostSource: export.CostSourceReported,
				Priced: true, Contributes: true,
			}
		}
	}
	return allocated
}

// Report is the API payload.
type Report struct {
	SchemaVersion      int                               `json:"schema_version,omitempty"`
	ReportID           string                            `json:"report_id,omitempty"`
	Pricing            *export.PricingBlock              `json:"pricing,omitempty"`
	Projects           map[string]export.ProjectMapEntry `json:"projects"`
	Timezone           string                            `json:"timezone"`
	RangeStart         string                            `json:"range_start"`
	RangeEnd           string                            `json:"range_end"`
	BucketUnit         string                            `json:"bucket_unit"`
	BucketSeconds      int                               `json:"bucket_seconds"`
	BucketCount        int                               `json:"bucket_count"`
	Partial            bool                              `json:"partial"`
	AsOf               *string                           `json:"as_of"`
	EffectiveEnd       string                            `json:"effective_end"`
	ElapsedBucketCount int                               `json:"elapsed_bucket_count"`
	Buckets            []Bucket                          `json:"buckets"`
	Peak               Peak                              `json:"peak"`
	Totals             Totals                            `json:"totals"`
	ByProject          []KeyMinutes                      `json:"by_project"`
	ByModel            []KeyMinutes                      `json:"by_model"`
	ByAgent            []KeyMinutes                      `json:"by_agent"`
	BySession          []SessionRow                      `json:"by_session"`
	SessionsNextCursor string                            `json:"sessions_next_cursor,omitempty"`
	SessionsTotal      int                               `json:"sessions_total"`
	Intervals          []ReportInterval                  `json:"-"`
}

func SanitizeProjectLabels(
	report *Report, projects map[string]export.ProjectMapEntry,
) {
	for i := range report.ByProject {
		report.ByProject[i].ProjectKey = export.ProjectKeyForEntry(
			projects[report.ByProject[i].Key],
		)
		report.ByProject[i].Key = export.SafeProjectDisplayLabel(
			report.ByProject[i].Key,
		)
	}
	for i := range report.BySession {
		title := export.SafeProjectDisplayLabel(report.BySession[i].Title)
		if title == "" {
			title = report.BySession[i].SessionID
		}
		report.BySession[i].Title = title
		report.BySession[i].ProjectKey = export.ProjectKeyForEntry(
			projects[report.BySession[i].Project],
		)
		report.BySession[i].Project = export.SafeProjectDisplayLabel(
			report.BySession[i].Project,
		)
	}
}

type Bucket struct {
	Start        string      `json:"start"`
	End          string      `json:"end"`
	MaxAgents    int         `json:"max_agents"`
	AgentMinutes float64     `json:"agent_minutes"`
	OutputTokens int         `json:"output_tokens"`
	Cost         money.Money `json:"cost"`
	// Automated/interactive split of the concurrency peak: the live automated
	// and interactive counts AT the instant MaxAgents first occurs. They sum to
	// MaxAgents, so a stacked bar reflects the true peak rather than stacking two
	// independent peaks (which could exceed it).
	AutomatedAtPeak   int `json:"automated_at_peak"`
	InteractiveAtPeak int `json:"interactive_at_peak"`
}

// ReportInterval is one half-open active span [Start, End) for a single
// session, exposed so the UI can list the sessions active during a clicked
// timeline slot. buildIntervals can emit several intervals per session within
// one slot (one per consecutive message pair), so consumers dedup by SessionID.
type ReportInterval struct {
	SessionID string `json:"session_id"`
	Start     string `json:"start"` // RFC3339 UTC
	End       string `json:"end"`   // RFC3339 UTC
}

type Peak struct {
	Agents int     `json:"agents"`
	At     *string `json:"at"`
}

type Totals struct {
	ActiveMinutes    float64     `json:"active_minutes"`
	IdleMinutes      float64     `json:"idle_minutes"`
	AgentMinutes     float64     `json:"agent_minutes"`
	Sessions         int         `json:"sessions"`
	UntimedSessions  int         `json:"untimed_sessions"`
	DistinctProjects int         `json:"distinct_projects"`
	DistinctModels   int         `json:"distinct_models"`
	OutputTokens     int         `json:"output_tokens"`
	Cost             money.Money `json:"cost"`
	// Additive automated/interactive segments (segment + segment == combined).
	AutomatedAgentMinutes   float64     `json:"automated_agent_minutes"`
	InteractiveAgentMinutes float64     `json:"interactive_agent_minutes"`
	AutomatedCost           money.Money `json:"automated_cost"`
	InteractiveCost         money.Money `json:"interactive_cost"`
	// Session counts split by class (AutomatedSessions + InteractiveSessions
	// == Sessions), so the summary card can show "total (auto / int)".
	AutomatedSessions   int `json:"automated_sessions"`
	InteractiveSessions int `json:"interactive_sessions"`
}

// KeyMinutes is one breakdown row (by project/model/agent). It carries both the
// combined agent-minutes and cost (so the UI can sort by either metric) plus the
// additive automated/interactive segments of each, exposed for a stacked-bar
// rendering the current UI does not yet draw (it shows the combined metric).
type KeyMinutes struct {
	ProjectKey              string      `json:"project_key,omitempty"`
	Key                     string      `json:"key"`
	AgentMinutes            float64     `json:"agent_minutes"`
	Cost                    money.Money `json:"cost"`
	AutomatedAgentMinutes   float64     `json:"automated_agent_minutes"`
	InteractiveAgentMinutes float64     `json:"interactive_agent_minutes"`
	AutomatedCost           money.Money `json:"automated_cost"`
	InteractiveCost         money.Money `json:"interactive_cost"`
}

type SessionRow struct {
	SessionID     string      `json:"session_id"`
	ProjectKey    string      `json:"project_key"`
	Title         string      `json:"title"`
	Project       string      `json:"project"`
	Agent         string      `json:"agent"`
	PrimaryModel  string      `json:"primary_model"`
	Models        []string    `json:"models"`
	AgentMinutes  *float64    `json:"agent_minutes"` // nil when untimed
	Cost          money.Money `json:"cost"`
	OutputTokens  int         `json:"output_tokens"`
	FirstActive   *string     `json:"first_active"`
	LastActive    *string     `json:"last_active"`
	TimingQuality string      `json:"timing_quality"` // "timed" | "untimed"
	IsAutomated   bool        `json:"is_automated"`
}

// interval is an internal half-open active span anchored to one session.
type interval struct {
	sessionID string
	start     time.Time
	end       time.Time
	model     string // model attributed to this interval
}

// parseTS parses an RFC3339(/Nano) timestamp; ok=false on empty/invalid.
func parseTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// rangeWindows tiles the range into bucket windows. BuildBuckets only errors
// for an unvalidated bucket spec (Query validates upstream); on error or a
// nil Loc it falls back to a single [start, end) window so the report stays
// well-formed, mirroring the old dayWindow fallback.
func rangeWindows(p Params) []BucketWindow {
	windows, err := BuildBuckets(p.RangeStart, p.RangeEnd, p.Bucket, paramsLoc(p))
	if err != nil || len(windows) == 0 {
		return []BucketWindow{{Start: p.RangeStart, End: p.RangeEnd}}
	}
	return windows
}

// Aggregate builds the range's report from the three input streams.
func Aggregate(
	p Params, sessions []SessionMeta, activity []ActivityEvent, usage []UsageRow,
) (Report, error) {
	gapCap := time.Duration(p.GapCapSeconds) * time.Second
	candidates := PairActivityEvents(
		activity, p.RangeStart, p.EffectiveEnd, gapCap,
	)
	return AggregateCandidates(
		context.Background(), p, sessions, candidates, usage,
	)
}

func newReport(p Params, windows []BucketWindow) Report {
	var asOf *string
	if p.Partial {
		s := p.EffectiveEnd.Format(time.RFC3339)
		asOf = &s
	}
	return Report{
		Timezone:           paramsLoc(p).String(),
		RangeStart:         p.RangeStart.Format(time.RFC3339),
		RangeEnd:           p.RangeEnd.Format(time.RFC3339),
		BucketUnit:         string(p.Bucket.Unit),
		BucketSeconds:      p.Bucket.NominalSeconds,
		BucketCount:        len(windows),
		Partial:            p.Partial,
		AsOf:               asOf,
		EffectiveEnd:       p.EffectiveEnd.Format(time.RFC3339),
		ElapsedBucketCount: elapsedBucketCount(windows, p.EffectiveEnd),
		Buckets:            []Bucket{},
		ByProject:          []KeyMinutes{},
		ByModel:            []KeyMinutes{},
		ByAgent:            []KeyMinutes{},
		BySession:          []SessionRow{},
		Intervals:          []ReportInterval{},
	}
}

// paramsLoc returns the params timezone, defaulting nil to UTC.
func paramsLoc(p Params) *time.Location {
	if p.Loc == nil {
		return time.UTC
	}
	return p.Loc
}

// elapsedBucketCount counts windows that have begun by effEnd.
func elapsedBucketCount(windows []BucketWindow, effEnd time.Time) int {
	n := 0
	for _, w := range windows {
		if w.Start.Before(effEnd) {
			n++
		}
	}
	return n
}

// EffectiveIntervalBounds applies the activity gap cap and clips one positive
// message pair to [start, end). It returns false when no activity from the pair
// falls inside the requested range.
func EffectiveIntervalBounds(
	previous, current, start, end time.Time,
	gapCap time.Duration,
) (time.Time, time.Time, bool) {
	if !current.After(previous) {
		return time.Time{}, time.Time{}, false
	}
	intervalEnd := current
	if capped := previous.Add(gapCap); intervalEnd.After(capped) {
		intervalEnd = capped
	}
	if previous.Before(start) {
		previous = start
	}
	if intervalEnd.After(end) {
		intervalEnd = end
	}
	if !intervalEnd.After(previous) {
		return time.Time{}, time.Time{}, false
	}
	return previous, intervalEnd, true
}

// automatedSet maps each session id to its automated class for the segment
// split. Sessions absent from the map are treated as interactive (false).
func automatedSet(sessions []SessionMeta) map[string]bool {
	m := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		m[s.SessionID] = s.IsAutomated
	}
	return m
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// usageAgg accumulates per-session cost, output tokens, and per-model cost.
// buildSessionsTable consumes it to build per-session rows and model breakdowns
// from the same deduped survivor set dedupUsage returns.
type usageAgg struct {
	cost         money.Money
	outputTokens int
	models       map[string]money.Money // model -> cost (for primary/mixed)
}

type sessionIntervalAgg struct {
	minutes       float64
	modelMins     map[string]float64
	modelDuration map[string]time.Duration
	first, last   time.Time
	hasIv         bool
}

type usageDedupToken struct {
	kind  string
	value string
}

type claudeUsageSnapshotToken struct {
	messageID string
	requestID string
}

func usageDedupTokenForRow(u UsageRow) (usageDedupToken, bool) {
	if u.ClaudeMessageID != "" && u.ClaudeRequestID != "" {
		return usageDedupToken{
			kind:  "claude",
			value: u.ClaudeMessageID + ":" + u.ClaudeRequestID,
		}, true
	}
	if u.Agent != "" && u.SourceUUID != "" {
		return usageDedupToken{
			kind:  "source",
			value: u.Agent + ":" + u.SourceUUID,
		}, true
	}
	if u.UsageDedupKey != "" {
		return usageDedupToken{
			kind:  "usage",
			value: u.UsageDedupKey,
		}, true
	}
	return usageDedupToken{}, false
}

// ClaudeSnapshotSurvivorSelection also returns the earliest session and the
// maximum billed web-search count for each surviving snapshot. Callers use the
// former for attribution and the latter when pricing the selected token row.
func ClaudeSnapshotSurvivorSelection(
	usage []UsageRow,
) (mask []bool, attribution []string, webSearchRequests []int) {
	return claudeSnapshotSurvivorSelection(usage, nil)
}

func claudeSnapshotSurvivorSelection(
	usage []UsageRow, eligible []bool,
) (mask []bool, attribution []string, webSearchRequests []int) {
	mask = make([]bool, len(usage))
	attribution = make([]string, len(usage))
	webSearchRequests = make([]int, len(usage))
	best := make(map[claudeUsageSnapshotToken]int)
	earliest := make(map[claudeUsageSnapshotToken]int)
	maximumWebSearchRequests := make(map[claudeUsageSnapshotToken]int)
	for i, u := range usage {
		if eligible != nil && !eligible[i] {
			continue
		}
		if u.ClaudeMessageID == "" || u.ClaudeRequestID == "" {
			mask[i] = true
			attribution[i] = u.SessionID
			webSearchRequests[i] = u.WebSearchRequests
			continue
		}
		key := claudeUsageSnapshotToken{
			messageID: u.ClaudeMessageID,
			requestID: u.ClaudeRequestID,
		}
		previous, ok := best[key]
		if first, exists := earliest[key]; !exists ||
			earlierClaudeSnapshotAttribution(u, usage[first]) {
			earliest[key] = i
		}
		if !ok || laterClaudeSnapshot(u, usage[previous]) {
			best[key] = i
		}
		maximumWebSearchRequests[key] = max(
			maximumWebSearchRequests[key], u.WebSearchRequests)
	}
	for key, i := range best {
		mask[i] = true
		attribution[i] = usage[earliest[key]].SessionID
		webSearchRequests[i] = maximumWebSearchRequests[key]
	}
	return mask, attribution, webSearchRequests
}

func earlierClaudeSnapshotAttribution(candidate, current UsageRow) bool {
	candidateTS, candidateErr := time.Parse(time.RFC3339Nano, candidate.Timestamp)
	currentTS, currentErr := time.Parse(time.RFC3339Nano, current.Timestamp)
	if candidateErr == nil && currentErr == nil && !candidateTS.Equal(currentTS) {
		return candidateTS.Before(currentTS)
	}
	if (candidateErr == nil) != (currentErr == nil) {
		return candidateErr == nil
	}
	if candidate.SessionID != current.SessionID {
		return candidate.SessionID < current.SessionID
	}
	if candidate.MessageOrdinal != current.MessageOrdinal {
		return candidate.MessageOrdinal < current.MessageOrdinal
	}
	return candidateErr != nil && candidate.Timestamp < current.Timestamp
}

func laterClaudeSnapshot(candidate, current UsageRow) bool {
	if candidate.OutputTokens != current.OutputTokens {
		return candidate.OutputTokens > current.OutputTokens
	}
	candidateTS, candidateErr := time.Parse(time.RFC3339Nano, candidate.Timestamp)
	currentTS, currentErr := time.Parse(time.RFC3339Nano, current.Timestamp)
	if candidateErr == nil && currentErr == nil && !candidateTS.Equal(currentTS) {
		return candidateTS.After(currentTS)
	}
	if (candidateErr == nil) != (currentErr == nil) {
		return candidateErr == nil
	}
	if candidate.SessionID != current.SessionID {
		return candidate.SessionID > current.SessionID
	}
	if candidate.MessageOrdinal != current.MessageOrdinal {
		return candidate.MessageOrdinal > current.MessageOrdinal
	}
	return candidateErr != nil && candidate.Timestamp > current.Timestamp
}

// LegacyUsageSurvivorMask preserves reporting schema v1's range filtering and
// first-seen deduplication. It intentionally does not rank Claude snapshots.
func LegacyUsageSurvivorMask(
	start, end, effEnd time.Time, usage []UsageRow,
) []bool {
	seen := map[usageDedupToken]struct{}{}
	mask := make([]bool, len(usage))
	for i, row := range usage {
		ts, ok := parseTS(row.Timestamp)
		if !ok || ts.Before(start) || !ts.Before(end) || !ts.Before(effEnd) {
			continue
		}
		if key, ok := usageDedupTokenForRow(row); ok {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
		}
		mask[i] = true
	}
	return mask
}

// UsageSurvivorSelection returns the survivor mask, accounting destination,
// and maximum billed web-search count for each surviving row. A complete
// snapshot can come from a later transcript while retaining the earliest
// transcript's attribution and an earlier snapshot's server-tool count.
func UsageSurvivorSelection(
	start, end, effEnd time.Time, usage []UsageRow,
) (mask []bool, attribution []string, webSearchRequests []int) {
	return usageSurvivorSelection(start, end, effEnd, usage, nil)
}

// UsageSurvivorSelectionForSessions selects complete Claude snapshots across
// all supplied rows, then limits them to the requested accounting destinations
// before applying generic first-seen deduplication. This prevents an excluded
// source-UUID or usage-key duplicate from suppressing an included row.
func UsageSurvivorSelectionForSessions(
	start, end, effEnd time.Time, usage []UsageRow, sessionIDs []string,
) (mask []bool, attribution []string, webSearchRequests []int) {
	allowed := make(map[string]struct{}, len(sessionIDs))
	for _, id := range sessionIDs {
		allowed[id] = struct{}{}
	}
	return usageSurvivorSelection(start, end, effEnd, usage, allowed)
}

func usageSurvivorSelection(
	start, end, effEnd time.Time,
	usage []UsageRow,
	allowed map[string]struct{},
) (mask []bool, attribution []string, webSearchRequests []int) {
	eligible := make([]bool, len(usage))
	for i, u := range usage {
		t, ok := parseTS(u.Timestamp)
		if !ok || t.Before(start) || !t.Before(end) {
			continue
		}
		if !t.Before(effEnd) {
			continue
		}
		eligible[i] = true
	}
	snapshots, snapshotAttribution, snapshotWebSearchRequests :=
		claudeSnapshotSurvivorSelection(usage, eligible)
	mask = make([]bool, len(usage))
	attribution = make([]string, len(usage))
	webSearchRequests = make([]int, len(usage))
	seen := map[usageDedupToken]struct{}{}
	for i, u := range usage {
		if !snapshots[i] {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[snapshotAttribution[i]]; !ok {
				continue
			}
		}
		if k, ok := usageDedupTokenForRow(u); ok {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		mask[i] = true
		attribution[i] = snapshotAttribution[i]
		webSearchRequests[i] = snapshotWebSearchRequests[i]
	}
	return mask, attribution, webSearchRequests
}

// dedupUsage filters usage rows to the range, keeps the fullest Claude
// snapshot across the candidate rows, then applies first-seen dedup that
// mirrors the usage stores. Rows arrive pre-sorted by (ts, session_id,
// COALESCE(message_ordinal,-1)). The half-open instant filter drops rows before
// start or at/after end; on a partial range effEnd is the as-of clip, so rows
// at or after effEnd are dropped before they can claim a dedup key, matching
// the activity/bucket clipping Aggregate applies. For a full range
// effEnd == end, so nothing extra is excluded.
func dedupUsage(start, end, effEnd time.Time, usage []UsageRow) []UsageRow {
	out := make([]UsageRow, 0, len(usage))
	mask, attribution, webSearchRequests :=
		usageSurvivorSelection(start, end, effEnd, usage, nil)
	for i, keep := range mask {
		if keep {
			row := usage[i]
			row.SessionID = attribution[i]
			row.WebSearchRequests = webSearchRequests[i]
			out = append(out, row)
		}
	}
	return out
}

// applyUsage dedups usage rows to the range, then accumulates output tokens
// and cost into r.Totals and the window whose [Start, End) contains each row's
// timestamp.
func applyUsage(r *Report, p Params, windows []BucketWindow, start, end time.Time,
	usage []UsageRow, automatedBy map[string]bool) error {
	survivors := dedupUsage(start, end, p.EffectiveEnd, usage)
	return applyUsageRows(r, windows, survivors, AllocateUsageCosts(survivors),
		automatedBy)
}

func applyUsageRows(
	r *Report, windows []BucketWindow, usage []UsageRow,
	allocated []UsageCostAllocation,
	automatedBy map[string]bool,
) error {
	for i, u := range usage {
		r.Totals.OutputTokens += u.OutputTokens
		var err error
		r.Totals.Cost, err = money.Add(r.Totals.Cost, allocated[i].Cost)
		if err != nil {
			return fmt.Errorf("summing activity report cost: %w", err)
		}
		if automatedBy[u.SessionID] {
			r.Totals.AutomatedCost, err = money.Add(
				r.Totals.AutomatedCost, allocated[i].Cost,
			)
			if err != nil {
				return fmt.Errorf("summing automated activity report cost: %w", err)
			}
		} else {
			r.Totals.InteractiveCost, err = money.Add(
				r.Totals.InteractiveCost, allocated[i].Cost,
			)
			if err != nil {
				return fmt.Errorf("summing interactive activity report cost: %w", err)
			}
		}
		t, _ := parseTS(u.Timestamp)
		if b := windowIndex(windows, t); b >= 0 && b < len(r.Buckets) {
			r.Buckets[b].OutputTokens += u.OutputTokens
			r.Buckets[b].Cost, err = money.Add(
				r.Buckets[b].Cost, allocated[i].Cost,
			)
			if err != nil {
				return fmt.Errorf("summing activity bucket cost: %w", err)
			}
		}
	}
	return nil
}

// windowIndex returns the index of the ascending-sorted window whose half-open
// [Start, End) contains t, or -1 if none does. Uses binary search since
// windows are sorted by Start.
func windowIndex(windows []BucketWindow, t time.Time) int {
	lo, hi := 0, len(windows)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		w := windows[mid]
		switch {
		case t.Before(w.Start):
			hi = mid - 1
		case !t.Before(w.End):
			lo = mid + 1
		default:
			return mid
		}
	}
	return -1
}

func buildSessionsTableFromDedupedUsage(
	r *Report, sessions []SessionMeta, agg map[string]*sessionIntervalAgg,
	usage []UsageRow, allocated []UsageCostAllocation,
) error {
	// Sort sessions by ID so the cost and minute rollups below accumulate in
	// one deterministic order. addKey sums float64 values across sessions and
	// float addition is not associative, so the unspecified per-backend row
	// order (no activityReportSessions query imposes ORDER BY) would otherwise
	// yield 1-ULP-different breakdown costs across SQLite, PostgreSQL, and
	// DuckDB for identical data.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	// Per-session cost/tokens/models from deduped usage.
	cost := map[string]*usageAgg{}
	for i, u := range usage {
		c := cost[u.SessionID]
		if c == nil {
			c = &usageAgg{models: map[string]money.Money{}}
			cost[u.SessionID] = c
		}
		var err error
		c.cost, err = money.Add(c.cost, allocated[i].Cost)
		if err != nil {
			return fmt.Errorf("summing activity session cost: %w", err)
		}
		c.outputTokens += u.OutputTokens
		if u.Model != "" {
			c.models[u.Model], err = money.Add(
				c.models[u.Model], allocated[i].Cost,
			)
			if err != nil {
				return fmt.Errorf("summing activity session model cost: %w", err)
			}
		}
	}
	projSet := map[string]struct{}{}
	modelSet := map[string]struct{}{}
	byProject := map[string]*keyAgg{}
	byAgent := map[string]*keyAgg{}
	byModel := map[string]*keyAgg{}
	r.BySession = make([]SessionRow, 0, len(sessions))
	for _, s := range sessions {
		au := s.IsAutomated
		if au {
			r.Totals.AutomatedSessions++
		} else {
			r.Totals.InteractiveSessions++
		}
		projSet[s.Project] = struct{}{}
		row := SessionRow{
			SessionID: s.SessionID, Title: s.Title, Project: s.Project,
			Agent: s.Agent, TimingQuality: "untimed", IsAutomated: au,
		}
		if a := agg[s.SessionID]; a != nil && a.hasIv {
			mins := a.minutes
			row.AgentMinutes = &mins
			row.TimingQuality = "timed"
			f := a.first.Format(time.RFC3339)
			l := a.last.Format(time.RFC3339)
			row.FirstActive, row.LastActive = &f, &l
			row.PrimaryModel, row.Models = primaryAndDurations(a.modelDuration)
			if err := addKey(byProject, s.Project, mins, money.Money{}, au); err != nil {
				return fmt.Errorf("summing activity project minutes: %w", err)
			}
			if err := addKey(byAgent, s.Agent, mins, money.Money{}, au); err != nil {
				return fmt.Errorf("summing activity agent minutes: %w", err)
			}
			for m, mm := range a.modelMins {
				if err := addKey(byModel, m, mm, money.Money{}, au); err != nil {
					return fmt.Errorf("summing activity model minutes: %w", err)
				}
			}
		} else {
			r.Totals.UntimedSessions++
		}
		if c := cost[s.SessionID]; c != nil {
			row.Cost = c.cost
			row.OutputTokens = c.outputTokens
			if row.PrimaryModel == "" {
				row.PrimaryModel, row.Models = primaryAndMoneyModels(c.models)
			}
			// Cost rolls up for every session with usage, timed or not, so the
			// cost breakdown sums to Totals.Cost. Minutes stay timed-only above.
			if err := addKey(byProject, s.Project, 0, c.cost, au); err != nil {
				return fmt.Errorf("summing activity project cost: %w", err)
			}
			if err := addKey(byAgent, s.Agent, 0, c.cost, au); err != nil {
				return fmt.Errorf("summing activity agent cost: %w", err)
			}
			for m, mc := range c.models {
				if err := addKey(byModel, m, 0, mc, au); err != nil {
					return fmt.Errorf("summing activity model cost: %w", err)
				}
			}
		}
		for _, m := range row.Models {
			modelSet[m] = struct{}{}
		}
		r.BySession = append(r.BySession, row)
	}
	sort.Slice(r.BySession, func(i, j int) bool {
		left, right := minutesOf(r.BySession[i]), minutesOf(r.BySession[j])
		if left != right {
			return left > right
		}
		return r.BySession[i].SessionID < r.BySession[j].SessionID
	})
	r.Totals.Sessions = len(sessions)
	r.Totals.DistinctProjects = len(projSet)
	r.Totals.DistinctModels = len(modelSet)
	r.ByProject = breakdownRows(byProject, false)
	r.ByAgent = breakdownRows(byAgent, false)
	r.ByModel = breakdownRows(byModel, true)
	return nil
}

// keyAgg accumulates a breakdown key's combined agent-minutes and cost plus the
// automated/interactive split of each. Minutes come from timed intervals; cost
// from deduped usage (all sessions, timed or not).
type keyAgg struct {
	minutes      float64
	cost         money.Money
	autoMinutes  float64
	interMinutes float64
	autoCost     money.Money
	interCost    money.Money
}

// addKey accumulates minutes and cost into the key's aggregate, routing the
// values into the automated or interactive segment by the session's class.
func addKey(
	m map[string]*keyAgg, key string, minutes float64, cost money.Money,
	automated bool,
) error {
	a := m[key]
	if a == nil {
		a = &keyAgg{}
		m[key] = a
	}
	a.minutes += minutes
	var err error
	a.cost, err = money.Add(a.cost, cost)
	if err != nil {
		return err
	}
	if automated {
		a.autoMinutes += minutes
		a.autoCost, err = money.Add(a.autoCost, cost)
	} else {
		a.interMinutes += minutes
		a.interCost, err = money.Add(a.interCost, cost)
	}
	return err
}

// breakdownRows turns a key->aggregate map into a slice sorted by combined
// agent-minutes descending (the UI re-sorts by the selected metric). It drops
// empty keys and rows with neither minutes nor cost; when dropModelKeys is set
// it also drops the "unknown" model key.
func breakdownRows(m map[string]*keyAgg, dropModelKeys bool) []KeyMinutes {
	out := make([]KeyMinutes, 0, len(m))
	for k, v := range m {
		if k == "" || (v.minutes == 0 && v.cost.Microdollars == 0) {
			continue
		}
		if dropModelKeys && k == "unknown" {
			continue
		}
		out = append(out, KeyMinutes{
			Key:                     k,
			AgentMinutes:            v.minutes,
			Cost:                    v.cost,
			AutomatedAgentMinutes:   v.autoMinutes,
			InteractiveAgentMinutes: v.interMinutes,
			AutomatedCost:           v.autoCost,
			InteractiveCost:         v.interCost,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentMinutes == out[j].AgentMinutes {
			return out[i].Key < out[j].Key
		}
		return out[i].AgentMinutes > out[j].AgentMinutes
	})
	return out
}

func minutesOf(s SessionRow) float64 {
	if s.AgentMinutes == nil {
		return -1
	}
	return *s.AgentMinutes
}

// primaryAndDurations returns the highest-duration model and sorted set.
// Duration keeps primary-model ties deterministic across different stream
// orders without relying on floating-point summation order.
func primaryAndDurations(w map[string]time.Duration) (string, []string) {
	var keys []string
	primary := ""
	var best time.Duration
	for key, duration := range w {
		if key == "" || key == "unknown" {
			continue
		}
		keys = append(keys, key)
		if duration > best || duration == best && (primary == "" || key < primary) {
			best, primary = duration, key
		}
	}
	sort.Strings(keys)
	if primary == "" && len(keys) > 0 {
		primary = keys[0]
	}
	return primary, keys
}

func primaryAndMoneyModels(w map[string]money.Money) (string, []string) {
	var keys []string
	primary := ""
	var best int64
	for k, v := range w {
		if k == "" || k == "unknown" {
			continue
		}
		keys = append(keys, k)
		if v.Microdollars > best {
			best, primary = v.Microdollars, k
		}
	}
	sort.Strings(keys)
	if primary == "" && len(keys) > 0 {
		primary = keys[0]
	}
	return primary, keys
}
