// Package activity aggregates a resolved time range of agent activity into a
// concurrency- and usage-oriented report. It operates on in-memory input
// streams supplied by a storage backend, so the same aggregation runs
// identically across SQLite, PostgreSQL, and DuckDB. Export contract types are
// referenced only for optional report metadata.
package activity

import (
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
	Intervals          []ReportInterval                  `json:"intervals"`
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
	startUTC, endUTC, effEnd := p.RangeStart, p.RangeEnd, p.EffectiveEnd
	var asOf *string
	if p.Partial {
		s := effEnd.Format(time.RFC3339)
		asOf = &s
	}

	windows := rangeWindows(p)
	intervals := buildIntervals(activity, gapCap, startUTC, effEnd)
	automatedBy := automatedSet(sessions)

	r := Report{
		Timezone:      paramsLoc(p).String(),
		RangeStart:    startUTC.Format(time.RFC3339),
		RangeEnd:      endUTC.Format(time.RFC3339),
		BucketUnit:    string(p.Bucket.Unit),
		BucketSeconds: p.Bucket.NominalSeconds,
		Partial:       p.Partial,
		AsOf:          asOf,
		EffectiveEnd:  effEnd.Format(time.RFC3339),
		Buckets:       []Bucket{},
		ByProject:     []KeyMinutes{},
		ByModel:       []KeyMinutes{},
		ByAgent:       []KeyMinutes{},
		BySession:     []SessionRow{},
		Intervals:     []ReportInterval{},
	}
	r.BucketCount = len(windows)
	r.ElapsedBucketCount = elapsedBucketCount(windows, effEnd)

	buildBuckets(&r, windows, effEnd, intervals, automatedBy)
	r.Peak, r.Totals.ActiveMinutes, _, _ = sweepLine(intervals, automatedBy)
	r.Totals.AgentMinutes = sumIntervalMinutes(intervals)
	r.Totals.AutomatedAgentMinutes, r.Totals.InteractiveAgentMinutes =
		splitIntervalMinutes(intervals, automatedBy)
	r.Totals.IdleMinutes = effEnd.Sub(startUTC).Minutes() - r.Totals.ActiveMinutes
	if r.Totals.IdleMinutes < 0 {
		r.Totals.IdleMinutes = 0
	}

	if err := applyUsage(
		&r, p, windows, startUTC, endUTC, usage, automatedBy,
	); err != nil {
		return Report{}, err
	}
	if err := buildSessionsTable(
		&r, startUTC, endUTC, effEnd, sessions, intervals, usage,
	); err != nil {
		return Report{}, err
	}
	r.Intervals = reportIntervals(intervals)
	return r, nil
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

// buildIntervals groups activity by session (already ordered by ordinal),
// emits one interval per adjacent pair with positive gap, caps at cap, and
// clips to [start, effEnd). The interval's model is the closing assistant
// message's model, else the session's last known model, else "unknown".
func buildIntervals(activity []ActivityEvent, cap time.Duration,
	start, effEnd time.Time) []interval {
	bySession := map[string][]ActivityEvent{}
	order := []string{}
	for _, e := range activity {
		if _, ok := bySession[e.SessionID]; !ok {
			order = append(order, e.SessionID)
		}
		bySession[e.SessionID] = append(bySession[e.SessionID], e)
	}
	var out []interval
	for _, sid := range order {
		evs := bySession[sid]
		lastModel := "unknown"
		for i := 1; i < len(evs); i++ {
			prev, ts := parseTS(evs[i-1].Timestamp)
			cur, ts2 := parseTS(evs[i].Timestamp)
			if !ts || !ts2 || !cur.After(prev) {
				continue
			}
			intervalStart, intervalEnd, effective :=
				EffectiveIntervalBounds(prev, cur, start, effEnd, cap)
			// Model attribution: closing assistant message wins.
			if evs[i].Role == "assistant" && evs[i].Model != "" {
				lastModel = evs[i].Model
			}
			if effective {
				out = append(out, interval{
					sessionID: sid,
					start:     intervalStart,
					end:       intervalEnd,
					model:     lastModel,
				})
			}
		}
	}
	return out
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

func clip(iv interval, start, end time.Time) (interval, bool) {
	if iv.start.Before(start) {
		iv.start = start
	}
	if iv.end.After(end) {
		iv.end = end
	}
	if !iv.end.After(iv.start) {
		return interval{}, false
	}
	return iv, true
}

// reportIntervals maps the aggregator's internal active intervals to the
// exposed payload form, sorted on the time.Time bounds by (start, end,
// sessionID) for a deterministic, format-independent order. Bounds are
// formatted at second resolution (RFC3339), matching every other timestamp in
// the report and keeping the payload identical across SQLite, PostgreSQL, and
// DuckDB: the mirror backends store timestamps at microsecond resolution, so
// exposing finer precision would let one session serialize differently per
// backend. A sub-second span therefore collapses to a point (start == end); the
// client places that instant in the slot containing it. Always returns a
// non-nil slice.
func reportIntervals(intervals []interval) []ReportInterval {
	sorted := make([]interval, len(intervals))
	copy(sorted, intervals)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.start.Equal(b.start) {
			return a.start.Before(b.start)
		}
		if !a.end.Equal(b.end) {
			return a.end.Before(b.end)
		}
		return a.sessionID < b.sessionID
	})
	out := make([]ReportInterval, 0, len(sorted))
	for _, iv := range sorted {
		out = append(out, ReportInterval{
			SessionID: iv.sessionID,
			Start:     iv.start.Format(time.RFC3339),
			End:       iv.end.Format(time.RFC3339),
		})
	}
	return out
}

func sumIntervalMinutes(ivs []interval) float64 {
	var m float64
	for _, iv := range ivs {
		m += iv.end.Sub(iv.start).Minutes()
	}
	return m
}

// splitIntervalMinutes sums interval minutes into automated and interactive
// totals by each interval's session class. The two sum to sumIntervalMinutes.
func splitIntervalMinutes(ivs []interval, automatedBy map[string]bool) (float64, float64) {
	var auto, inter float64
	for _, iv := range ivs {
		m := iv.end.Sub(iv.start).Minutes()
		if automatedBy[iv.sessionID] {
			auto += m
		} else {
			inter += m
		}
	}
	return auto, inter
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

// sweepLine returns the exact peak concurrency (and the first instant it
// occurs), the wall-clock minutes where >=1 interval is live, and the
// automated/interactive split of the live count AT the peak instant. The split
// counts sum to peak.Agents because they are snapshotted at the same event that
// sets the peak, so a stacked bar never exceeds the true peak.
func sweepLine(ivs []interval, automatedBy map[string]bool) (Peak, float64, int, int) {
	type ev struct {
		t     time.Time
		delta int
		auto  bool
	}
	evs := make([]ev, 0, len(ivs)*2)
	for _, iv := range ivs {
		a := automatedBy[iv.sessionID]
		evs = append(evs, ev{iv.start, 1, a}, ev{iv.end, -1, a})
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t.Equal(evs[j].t) {
			// Intervals are half-open [start,end): process closes (-1)
			// before opens (+1) at a tie so two abutting intervals from one
			// session are not counted as overlapping at the shared boundary.
			return evs[i].delta < evs[j].delta
		}
		return evs[i].t.Before(evs[j].t)
	})
	var peak Peak
	live, liveAuto, liveInter := 0, 0, 0
	var autoAtPeak, interAtPeak int
	var active time.Duration
	var lastT time.Time
	for i, e := range evs {
		if i > 0 && live > 0 {
			active += e.t.Sub(lastT)
		}
		live += e.delta
		if e.auto {
			liveAuto += e.delta
		} else {
			liveInter += e.delta
		}
		if live > peak.Agents {
			peak.Agents = live
			at := e.t.Format(time.RFC3339)
			peak.At = &at
			autoAtPeak, interAtPeak = liveAuto, liveInter
		}
		lastT = e.t
	}
	return peak, active.Minutes(), autoAtPeak, interAtPeak
}

// buildBuckets emits one r.Buckets entry per window (with Start/End bounds for
// ALL windows, elapsed or not, since clients need every window's bounds) and
// fills agent_minutes / max_agents for windows overlapping [.., effEnd). Each
// window's own [Start, End) bounds the per-bucket clip, so variable-width
// calendar buckets accumulate over their actual span, not a fixed step.
func buildBuckets(r *Report, windows []BucketWindow, effEnd time.Time,
	ivs []interval, automatedBy map[string]bool) {
	r.Buckets = make([]Bucket, len(windows))
	for i, w := range windows {
		r.Buckets[i] = Bucket{
			Start: w.Start.Format(time.RFC3339),
			End:   w.End.Format(time.RFC3339),
		}
		if !w.Start.Before(effEnd) {
			continue // window has not begun; leave metrics zero
		}
		for _, iv := range ivs {
			lo := maxTime(iv.start, w.Start)
			hi := minTime(iv.end, w.End)
			if hi.After(lo) {
				r.Buckets[i].AgentMinutes += hi.Sub(lo).Minutes()
			}
		}
	}
	fillBucketMaxAgents(r, windows, effEnd, ivs, automatedBy)
}

// fillBucketMaxAgents sets r.Buckets[b].MaxAgents to the peak concurrency seen
// within window b. For each elapsed window it clips every interval to the
// window's half-open span and runs the shared sweep over the survivors. A
// range has at most maxBuckets windows, so a per-window sweep is fine.
func fillBucketMaxAgents(r *Report, windows []BucketWindow, effEnd time.Time,
	ivs []interval, automatedBy map[string]bool) {
	for b, w := range windows {
		if !w.Start.Before(effEnd) {
			continue
		}
		var clipped []interval
		for _, iv := range ivs {
			if c, ok := clip(iv, w.Start, w.End); ok {
				clipped = append(clipped, c)
			}
		}
		if len(clipped) == 0 {
			continue
		}
		peak, _, autoAtPeak, interAtPeak := sweepLine(clipped, automatedBy)
		r.Buckets[b].MaxAgents = peak.Agents
		r.Buckets[b].AutomatedAtPeak = autoAtPeak
		r.Buckets[b].InteractiveAtPeak = interAtPeak
	}
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
	usage = dedupUsage(start, end, p.EffectiveEnd, usage)
	allocated := AllocateUsageCosts(usage)
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

// buildSessionsTable populates r.BySession plus the by-project/agent/model
// breakdowns and distinct counts. Per-session interval minutes, model minutes,
// and the active window come from the timed intervals; cost, output tokens, and
// model fallbacks come from the deduped usage survivors. Breakdowns roll the
// per-session minutes up by project/agent (timed sessions only) and by interval
// model, all sorted by minutes descending with empty/zero keys dropped.
func buildSessionsTable(r *Report, start, end, effEnd time.Time,
	sessions []SessionMeta, ivs []interval, usage []UsageRow) error {
	// Sort sessions by ID so the cost and minute rollups below accumulate in
	// one deterministic order. addKey sums float64 values across sessions and
	// float addition is not associative, so the unspecified per-backend row
	// order (no activityReportSessions query imposes ORDER BY) would otherwise
	// yield 1-ULP-different breakdown costs across SQLite, PostgreSQL, and
	// DuckDB for identical data.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	// Per-session interval minutes + model minutes + active window.
	type sAgg struct {
		minutes     float64
		modelMins   map[string]float64
		first, last time.Time
		hasIv       bool
	}
	agg := map[string]*sAgg{}
	for _, iv := range ivs {
		a := agg[iv.sessionID]
		if a == nil {
			a = &sAgg{modelMins: map[string]float64{}}
			agg[iv.sessionID] = a
		}
		m := iv.end.Sub(iv.start).Minutes()
		a.minutes += m
		a.modelMins[iv.model] += m
		if !a.hasIv || iv.start.Before(a.first) {
			a.first = iv.start
		}
		if !a.hasIv || iv.end.After(a.last) {
			a.last = iv.end
		}
		a.hasIv = true
	}
	// Per-session cost/tokens/models from deduped usage.
	cost := map[string]*usageAgg{}
	usage = dedupUsage(start, end, effEnd, usage)
	allocated := AllocateUsageCosts(usage)
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
			row.PrimaryModel, row.Models = primaryAndModels(a.modelMins)
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
		return minutesOf(r.BySession[i]) > minutesOf(r.BySession[j])
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

// primaryAndModels returns the highest-weight model and the sorted set. When
// no model carries positive weight (e.g. zero-cost or unpriced usage) it falls
// back to the first model in sorted order, so a known-model session still
// reports a primary; the primary is "" only when the set is empty. Caller
// renders "mixed" when len>1.
func primaryAndModels(w map[string]float64) (string, []string) {
	var keys []string
	primary := ""
	var best float64
	for k, v := range w {
		if k == "" || k == "unknown" {
			continue
		}
		keys = append(keys, k)
		if v > best {
			best, primary = v, k
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
