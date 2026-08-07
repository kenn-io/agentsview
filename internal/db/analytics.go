package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/agentsview/internal/signals"
)

// maxSQLVars is the maximum bind variables per IN clause to stay
// within SQLite's default SQLITE_MAX_VARIABLE_NUMBER (999).
const maxSQLVars = 500

var ErrUnsupportedAnalyticsSignal = errors.New(
	"unsupported analytics signal",
)

var supportedAnalyticsSignals = map[string]struct{}{
	"outcome_errored":                {},
	"outcome_abandoned":              {},
	"outcome_completed":              {},
	"tool_failure_signals":           {},
	"tool_retries":                   {},
	"edit_churn":                     {},
	"sessions_with_compaction":       {},
	"mid_task_compaction_count":      {},
	"high_pressure_sessions":         {},
	"short_prompt_count":             {},
	"unstructured_start":             {},
	"missing_success_criteria_count": {},
	"missing_verification_count":     {},
	"duplicate_prompt_count":         {},
	"no_code_context_count":          {},
	"runaway_tool_loop_count":        {},
	"frustration_marker_count":       {},
}

func IsSupportedAnalyticsSignal(signal string) bool {
	_, ok := supportedAnalyticsSignals[signal]
	return ok
}

// inPlaceholders returns a "(?,?,...)" string and []any args for
// a slice of string IDs.
func inPlaceholders(ids []string) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(ph, ",") + ")", args
}

// queryChunked executes a callback for each chunk of IDs,
// splitting at maxSQLVars to avoid SQLite bind-variable limits.
func queryChunked(
	ids []string,
	fn func(chunk []string) error,
) error {
	return queryChunkedSize(ids, maxSQLVars, fn)
}

// queryChunkedSize is queryChunked with an explicit per-chunk size, for
// queries that bind each ID more than once (and so need a smaller chunk to
// keep the total bind count within SQLite's variable limit).
func queryChunkedSize(
	ids []string,
	size int,
	fn func(chunk []string) error,
) error {
	for i := 0; i < len(ids); i += size {
		end := min(i+size, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// AnalyticsFilter is the shared filter for all analytics queries.
type AnalyticsFilter struct {
	From    string // ISO date YYYY-MM-DD, inclusive
	To      string // ISO date YYYY-MM-DD, inclusive
	Machine string // optional machine filter
	Project string // optional project filter
	// GitBranch is a branchListSep-joined list of opaque (project, branch) tokens (EncodeBranchFilterToken).
	GitBranch        string
	Agent            string // optional agent filter
	Model            string // optional model filter
	Timezone         string // IANA timezone for day bucketing
	DayOfWeek        *int   // nil = all, 0=Mon, 6=Sun (ISO)
	Hour             *int   // nil = all, 0-23
	MinUserMessages  int    // user_message_count >= N
	ExcludeOneShot   bool   // exclude sessions with user_message_count <= 1
	ExcludeAutomated bool   // exclude automated (roborev) sessions
	// ExcludeInteractive is the mirror of ExcludeAutomated: it keeps only
	// automated sessions. The two are never set together (that would match
	// nothing); the activity report uses them for its automation filter.
	ExcludeInteractive bool
	AutomatedScope     string // "", "human", "all", or "automated"
	ActiveSince        string // ISO timestamp cutoff
	Termination        string // "", "clean", or "unclean"
	// IncludeSubagents counts subagent sessions (including workflow
	// subagents) in token/session aggregates. It is opt-in and set only
	// on the sum/count surfaces GetAnalyticsSummary and
	// GetAnalyticsProjects. Distribution surfaces (session-shape,
	// velocity, timing) leave it false so short subagent sessions do not
	// skew them.
	IncludeSubagents bool
	// IncludeForks counts fork sessions (rewound conversation branches
	// split out by the claude and piebald parsers). Fork sessions hold
	// only their own branch's messages, never replayed copies, so their
	// usage is real spend; GetDailyUsage counts it via per-row dedup.
	// Set only by GetActivityReport so its cost totals match daily
	// usage. Aggregate analytics surfaces leave forks excluded so a
	// rewound branch does not count as an extra session.
	IncludeForks bool
}

// RelationshipExclusionSQL returns the relationship_type predicate for
// analytics aggregation. The default excludes subagent and fork rows
// (matching the session list); IncludeSubagents and IncludeForks each
// lift one exclusion. Exported so the PostgreSQL and DuckDB analytics
// builders apply the same rule.
func (f AnalyticsFilter) RelationshipExclusionSQL() string {
	return RelationshipExclusionSQL(f.IncludeSubagents, f.IncludeForks, "")
}

// RelationshipExclusionSQL is the single source of truth for the
// relationship_type analytics predicate, shared by the analytics
// builders (AnalyticsFilter) and the stats pipeline (StatsFilter).
// colPrefix qualifies the column for callers that alias the sessions
// table (e.g. "s."); pass "" for an unqualified column. Subagent and
// fork rows are excluded unless the corresponding include flag is set;
// with both set the predicate is a no-op so the clause stays composable.
func RelationshipExclusionSQL(includeSubagents, includeForks bool, colPrefix string) string {
	col := colPrefix + "relationship_type"
	switch {
	case includeSubagents && includeForks:
		return "1=1"
	case includeSubagents:
		return col + " NOT IN ('fork')"
	case includeForks:
		return col + " NOT IN ('subagent')"
	default:
		return col + " NOT IN ('subagent', 'fork')"
	}
}

// OneShotExclusionSQL wraps the one-shot exclusion predicate so it does
// not drop subagent rows when subagents are being counted. Workflow
// subagents are inherently one-shot (a single orchestrator prompt
// yields one result) but represent real work, so the one-shot filter
// would otherwise re-hide exactly the sessions IncludeSubagents is
// meant to surface. Exported so the PostgreSQL and DuckDB builders
// apply the same rule. base must be a self-contained boolean clause.
func (f AnalyticsFilter) OneShotExclusionSQL(base string) string {
	if f.IncludeSubagents {
		return "(" + base + " OR relationship_type = 'subagent')"
	}
	return base
}

// location loads the timezone or returns UTC on error.
func (f AnalyticsFilter) location() *time.Location {
	if f.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// utcRange returns UTC time bounds padded by ±14h to cover
// all possible timezone offsets. The WHERE clause uses these
// to leverage the started_at index. Empty From/To inputs
// collapse to wide-open sentinels so a zero AnalyticsFilter
// matches every session (mirrors the PG store).
func (f AnalyticsFilter) utcRange() (string, string) {
	const (
		unboundedFrom = "0001-01-01T00:00:00Z"
		unboundedTo   = "9999-12-31T23:59:59Z"
	)
	from := unboundedFrom
	if f.From != "" {
		from = f.From + "T00:00:00Z"
	}
	to := unboundedTo
	if f.To != "" {
		to = f.To + "T23:59:59Z"
	}

	tFrom, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return unboundedFrom, unboundedTo
	}
	tTo, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return unboundedFrom, unboundedTo
	}

	// Skip ±14h padding on the sentinels to avoid pushing the
	// lower bound below year 1.
	if f.From == "" {
		from = unboundedFrom
	} else {
		from = tFrom.Add(-14 * time.Hour).Format(time.RFC3339)
	}
	if f.To == "" {
		to = unboundedTo
	} else {
		to = tTo.Add(14 * time.Hour).Format(time.RFC3339)
	}
	return from, to
}

// buildWhere returns a WHERE clause and args for common
// analytics filters.
func (f AnalyticsFilter) buildWhere(
	dateCol string,
) (string, []any) {
	return f.buildWhereWithDate(dateCol, true, "sessions.id")
}

func csvFilterValues(raw string) []string {
	values := strings.Split(raw, ",")
	out := values[:0]
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (f AnalyticsFilter) buildWhereWithDate(
	dateCol string,
	includeDate bool,
	sessionIDExpr string,
) (string, []any) {
	if sessionIDExpr == "" {
		sessionIDExpr = "sessions.id"
	}
	preds := []string{
		"message_count > 0",
		f.RelationshipExclusionSQL(),
		"deleted_at IS NULL",
	}
	var args []any

	if includeDate {
		utcFrom, utcTo := f.utcRange()
		preds = append(preds, dateCol+" >= ?")
		args = append(args, utcFrom)
		preds = append(preds, dateCol+" <= ?")
		args = append(args, utcTo)
	}

	if f.Machine != "" {
		machines := csvFilterValues(f.Machine)
		if len(machines) == 1 {
			preds = append(preds, "machine = ?")
			args = append(args, machines[0])
		} else if len(machines) > 1 {
			placeholders := make(
				[]string, len(machines),
			)
			for i, machine := range machines {
				placeholders[i] = "?"
				args = append(args, machine)
			}
			preds = append(preds,
				"machine IN ("+
					strings.Join(placeholders, ",")+
					")",
			)
		}
	}

	if f.Project != "" {
		preds = append(preds, "project = ?")
		args = append(args, f.Project)
	}

	if f.GitBranch != "" {
		var clause string
		clause, args = BranchPairClauseArgs("project", "git_branch", f.GitBranch, args)
		preds = append(preds, clause)
	}

	if f.Agent != "" {
		agents := csvFilterValues(f.Agent)
		if len(agents) == 1 {
			preds = append(preds, "agent = ?")
			args = append(args, agents[0])
		} else if len(agents) > 1 {
			placeholders := make(
				[]string, len(agents),
			)
			for i, a := range agents {
				placeholders[i] = "?"
				args = append(args, a)
			}
			preds = append(preds,
				"agent IN ("+
					strings.Join(placeholders, ",")+
					")",
			)
		}
	}

	if f.Model != "" {
		models := csvFilterValues(f.Model)
		if len(models) == 1 {
			preds = append(preds,
				"EXISTS (SELECT 1 FROM messages m WHERE "+
					"m.session_id = "+sessionIDExpr+" AND "+
					"m.model = ?)")
			args = append(args, models[0])
		} else if len(models) > 1 {
			placeholders := make(
				[]string, len(models),
			)
			for i, m := range models {
				placeholders[i] = "?"
				args = append(args, m)
			}
			preds = append(preds,
				"EXISTS (SELECT 1 FROM messages m WHERE "+
					"m.session_id = "+sessionIDExpr+" AND "+
					"m.model IN ("+
					strings.Join(placeholders, ",")+
					"))")
		}
	}

	if f.MinUserMessages > 0 {
		preds = append(preds, "user_message_count >= ?")
		args = append(args, f.MinUserMessages)
	}
	scope := normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if scope != "human" {
			preds = append(preds,
				f.OneShotExclusionSQL(
					"(user_message_count > 1 OR is_automated = 1)"))
		} else {
			preds = append(preds,
				f.OneShotExclusionSQL("user_message_count > 1"))
		}
	}
	if pred := automatedScopePredicate(scope, "is_automated"); pred != "" {
		preds = append(preds, pred)
	}
	if f.ExcludeInteractive {
		preds = append(preds, "is_automated = 1")
	}

	if f.ActiveSince != "" {
		preds = append(preds,
			"COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at) >= ?")
		args = append(args, f.ActiveSince)
	}

	if pred, pargs := buildTerminationPredSQLite(f.Termination); pred != "" {
		preds = append(preds, pred)
		args = append(args, pargs...)
	}

	return strings.Join(preds, " AND "), args
}

func normalizeAutomatedScope(scope string, excludeAutomated bool) string {
	switch strings.TrimSpace(scope) {
	case "human", "all", "automated":
		return strings.TrimSpace(scope)
	}
	if excludeAutomated {
		return "human"
	}
	return "all"
}

func automatedScopePredicate(scope, col string) string {
	switch scope {
	case "human":
		return col + " = 0"
	case "automated":
		return col + " = 1"
	default:
		return ""
	}
}

// HasTimeFilter returns true when hour-of-day or day-of-week
// filtering is active.
func (f AnalyticsFilter) HasTimeFilter() bool {
	return f.DayOfWeek != nil || f.Hour != nil
}

// matchesTimeFilter checks whether a local time matches the
// active hour and/or day-of-week filter.
func (f AnalyticsFilter) matchesTimeFilter(
	t time.Time,
) bool {
	if f.DayOfWeek != nil {
		dow := (int(t.Weekday()) + 6) % 7 // ISO Mon=0
		if dow != *f.DayOfWeek {
			return false
		}
	}
	if f.Hour != nil {
		if t.Hour() != *f.Hour {
			return false
		}
	}
	return true
}

func (f AnalyticsFilter) sqliteTimeModifier() (string, bool) {
	if f.Timezone == "" || f.Timezone == "UTC" {
		return "", true
	}
	if f.From == "" || f.To == "" {
		return "", false
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return "", false
	}
	start, err := time.Parse("2006-01-02", f.From)
	if err != nil {
		return "", false
	}
	end, err := time.Parse("2006-01-02", f.To)
	if err != nil {
		return "", false
	}
	var offset *int
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		checks := []time.Time{
			time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc),
			time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc),
		}
		for _, local := range checks {
			_, current := local.Zone()
			if current%60 != 0 {
				return "", false
			}
			if offset == nil {
				v := current
				offset = &v
				continue
			}
			if *offset != current {
				return "", false
			}
		}
	}
	if offset == nil {
		return "", false
	}
	sign := "+"
	value := *offset
	if value < 0 {
		sign = "-"
		value = -value
	}
	return fmt.Sprintf("%s%02d:%02d", sign, value/3600, (value%3600)/60), true
}

// localTime parses a UTC timestamp string and converts it to the
// given location. Returns the local time and true on success.
func localTime(
	ts string, loc *time.Location,
) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t.In(loc), true
}

// localDate converts a UTC timestamp string to a local date
// string (YYYY-MM-DD) in the given location.
func localDate(ts string, loc *time.Location) string {
	t, ok := localTime(ts, loc)
	if !ok {
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ""
	}
	return t.Format("2006-01-02")
}

// percentileFloat returns the value at the given percentile
// from a pre-sorted float64 slice.
func percentileFloat(sorted []float64, pct float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(float64(n) * pct)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// inDateRange checks if a local date falls within [from, to].
// Empty bounds are treated as unbounded so callers can pass a
// zero AnalyticsFilter to get every session.
func inDateRange(date, from, to string) bool {
	if from != "" && date < from {
		return false
	}
	if to != "" && date > to {
		return false
	}
	return true
}

// medianInt returns the median of a sorted int slice of
// length n. For even n, returns the average of the two
// middle elements.
func medianInt(sorted []int, n int) int {
	if n == 0 {
		return 0
	}
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// --- Summary ---

// AgentSummary holds per-agent counts for the summary.
type AgentSummary struct {
	Sessions int `json:"sessions"`
	Messages int `json:"messages"`
}

// AnalyticsSummary is the response for the summary endpoint.
type AnalyticsSummary struct {
	TotalSessions          int                      `json:"total_sessions"`
	TotalMessages          int                      `json:"total_messages"`
	TotalOutputTokens      int                      `json:"total_output_tokens"`
	TokenReportingSessions int                      `json:"token_reporting_sessions"`
	Models                 []string                 `json:"models"`
	ActiveProjects         int                      `json:"active_projects"`
	ActiveDays             int                      `json:"active_days"`
	AvgMessages            float64                  `json:"avg_messages"`
	MedianMessages         int                      `json:"median_messages"`
	P90Messages            int                      `json:"p90_messages"`
	MostActive             string                   `json:"most_active_project"`
	Concentration          float64                  `json:"concentration"`
	Agents                 map[string]*AgentSummary `json:"agents"`
}

// GetAnalyticsSummary returns aggregate statistics.

// The summary is a token/session aggregate, so subagent sessions
// (including workflow subagents) are counted here.

// --- Activity ---

// ActivityEntry is one time bucket in the activity timeline.
type ActivityEntry struct {
	Date              string         `json:"date"`
	Sessions          int            `json:"sessions"`
	Messages          int            `json:"messages"`
	UserMessages      int            `json:"user_messages"`
	AssistantMessages int            `json:"assistant_messages"`
	ToolCalls         int            `json:"tool_calls"`
	ThinkingMessages  int            `json:"thinking_messages"`
	ByAgent           map[string]int `json:"by_agent"`
}

// ActivityResponse wraps the activity series.
type ActivityResponse struct {
	Granularity string          `json:"granularity"`
	Series      []ActivityEntry `json:"series"`
}

// bucketDate truncates a date to the start of its bucket.
func bucketDate(date string, granularity string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	switch granularity {
	case "week":
		// ISO week: Monday start
		weekday := int(t.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		t = t.AddDate(0, 0, -(weekday - 1))
		return t.Format("2006-01-02")
	case "month":
		return t.Format("2006-01") + "-01"
	default:
		return date
	}
}

// GetAnalyticsActivity returns session/message counts grouped
// by time bucket.

// session_id -> bucket

// Count this session once per bucket

// Merge tool_call counts per session into buckets.

// Sort by date

// --- Heatmap ---

// HeatmapEntry is one day in the heatmap calendar.
type HeatmapEntry struct {
	Date  string `json:"date"`
	Value int    `json:"value"`
	Level int    `json:"level"`
}

// HeatmapLevels defines the quartile thresholds for levels 1-4.
type HeatmapLevels struct {
	L1 int `json:"l1"`
	L2 int `json:"l2"`
	L3 int `json:"l3"`
	L4 int `json:"l4"`
}

// HeatmapResponse wraps the heatmap data.
type HeatmapResponse struct {
	Metric      string         `json:"metric"`
	Entries     []HeatmapEntry `json:"entries"`
	Levels      HeatmapLevels  `json:"levels"`
	EntriesFrom string         `json:"entries_from"`
}

// GetAnalyticsHeatmap returns daily counts with intensity levels.

// Choose which map to use based on metric

// For output_tokens, an empty source means no sessions
// reported token coverage. Return an empty heatmap so the
// UI can show "no data" instead of a misleading zero grid.

// Determine effective date range (clamped to MaxHeatmapDays)

// Collect non-zero values from the displayed range only,
// so outliers outside the window don't skew intensity.

// Build entries for each day in the clamped range

// computeQuartileLevels computes thresholds from sorted values.
func computeQuartileLevels(sorted []int) HeatmapLevels {
	if len(sorted) == 0 {
		return HeatmapLevels{L1: 1, L2: 2, L3: 3, L4: 4}
	}
	n := len(sorted)
	return HeatmapLevels{
		L1: sorted[0],
		L2: sorted[n/4],
		L3: sorted[n/2],
		L4: sorted[n*3/4],
	}
}

// assignLevel determines the heatmap level (0-4) for a value.
func assignLevel(value int, levels HeatmapLevels) int {
	if value <= 0 {
		return 0
	}
	if value <= levels.L2 {
		return 1
	}
	if value <= levels.L3 {
		return 2
	}
	if value <= levels.L4 {
		return 3
	}
	return 4
}

// MaxHeatmapDays is the maximum number of day entries the
// heatmap will return. Ranges exceeding this are clamped to
// the most recent MaxHeatmapDays from the end date.
const MaxHeatmapDays = 366

// clampFrom returns from clamped so that [from, to] spans at
// most MaxHeatmapDays. If the range is already within bounds,
// from is returned unchanged.
func clampFrom(from, to string) string {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return from
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return from
	}
	earliest := end.AddDate(0, 0, -(MaxHeatmapDays - 1))
	if start.Before(earliest) {
		return earliest.Format("2006-01-02")
	}
	return from
}

// buildDateEntries creates a HeatmapEntry for each day in
// [from, to]. The caller is responsible for clamping the
// range via clampFrom before calling this function.
func buildDateEntries(
	from, to string,
	values map[string]int,
	levels HeatmapLevels,
) []HeatmapEntry {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil
	}

	var entries []HeatmapEntry
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		v := values[date]
		entries = append(entries, HeatmapEntry{
			Date:  date,
			Value: v,
			Level: assignLevel(v, levels),
		})
	}
	return entries
}

// --- Projects ---

// ProjectAnalytics holds analytics for a single project.
type ProjectAnalytics struct {
	Name           string         `json:"name"`
	Sessions       int            `json:"sessions"`
	Messages       int            `json:"messages"`
	FirstSession   string         `json:"first_session"`
	LastSession    string         `json:"last_session"`
	AvgMessages    float64        `json:"avg_messages"`
	MedianMessages int            `json:"median_messages"`
	Agents         map[string]int `json:"agents"`
	DailyTrend     float64        `json:"daily_trend"`
}

// ProjectsAnalyticsResponse wraps the projects list.
type ProjectsAnalyticsResponse struct {
	Projects []ProjectAnalytics `json:"projects"`
}

// GetAnalyticsProjects returns per-project analytics.

// Per-project session/token breakdown is an aggregate, so subagent
// sessions (including workflow subagents) are counted here.

// Daily trend: messages per active day

// Sort by message count descending

// --- Hour-of-Week ---

// HourOfWeekCell is one cell in the 7x24 hour-of-week grid.
type HourOfWeekCell struct {
	DayOfWeek int `json:"day_of_week"` // 0=Mon, 6=Sun
	Hour      int `json:"hour"`        // 0-23
	Messages  int `json:"messages"`
}

// HourOfWeekResponse wraps the hour-of-week heatmap data.
type HourOfWeekResponse struct {
	Cells []HourOfWeekCell `json:"cells"`
}

// GetAnalyticsHourOfWeek returns message counts bucketed by
// day-of-week and hour-of-day in the user's timezone.

// Go Sunday=0, convert to ISO Monday=0

// HourOfWeekResponseFromGrid flattens the 7x24 grid into the dense 168-cell
// response.
func HourOfWeekResponseFromGrid(grid [7][24]int) HourOfWeekResponse {
	cells := make([]HourOfWeekCell, 0, 168)
	for d := range 7 {
		for h := range 24 {
			cells = append(cells, HourOfWeekCell{
				DayOfWeek: d,
				Hour:      h,
				Messages:  grid[d][h],
			})
		}
	}
	return HourOfWeekResponse{Cells: cells}
}

// --- Session Shape ---

// DistributionBucket is a labeled count for histogram display.
type DistributionBucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// SessionShapeResponse holds distribution histograms for session
// characteristics.
type SessionShapeResponse struct {
	Count                int                  `json:"count"`
	LengthDistribution   []DistributionBucket `json:"length_distribution"`
	DurationDistribution []DistributionBucket `json:"duration_distribution"`
	AutonomyDistribution []DistributionBucket `json:"autonomy_distribution"`
}

// lengthBucket returns the bucket label for a message count.
func lengthBucket(mc int) string {
	switch {
	case mc <= 5:
		return "1-5"
	case mc <= 15:
		return "6-15"
	case mc <= 30:
		return "16-30"
	case mc <= 60:
		return "31-60"
	case mc <= 120:
		return "61-120"
	default:
		return "121+"
	}
}

// durationBucket returns the bucket label for a duration in
// minutes.
func durationBucket(mins float64) string {
	switch {
	case mins < 5:
		return "<5m"
	case mins < 15:
		return "5-15m"
	case mins < 30:
		return "15-30m"
	case mins < 60:
		return "30-60m"
	case mins < 120:
		return "1-2h"
	default:
		return "2h+"
	}
}

// autonomyBucket returns the bucket label for an autonomy ratio.
func autonomyBucket(ratio float64) string {
	switch {
	case ratio < 0.5:
		return "<0.5"
	case ratio < 1:
		return "0.5-1"
	case ratio < 2:
		return "1-2"
	case ratio < 5:
		return "2-5"
	case ratio < 10:
		return "5-10"
	default:
		return "10+"
	}
}

// bucketOrder maps label → order index for consistent output.
var (
	lengthOrder = map[string]int{
		"1-5": 0, "6-15": 1, "16-30": 2,
		"31-60": 3, "61-120": 4, "121+": 5,
	}
	durationOrder = map[string]int{
		"<5m": 0, "5-15m": 1, "15-30m": 2,
		"30-60m": 3, "1-2h": 4, "2h+": 5,
	}
	autonomyOrder = map[string]int{
		"<0.5": 0, "0.5-1": 1, "1-2": 2,
		"2-5": 3, "5-10": 4, "10+": 5,
	}
)

// sortBuckets sorts distribution buckets by their defined order.
func sortBuckets(
	buckets []DistributionBucket,
	order map[string]int,
) {
	sort.Slice(buckets, func(i, j int) bool {
		return order[buckets[i].Label] < order[buckets[j].Label]
	})
}

// mapToBuckets converts a label→count map to sorted buckets.
func mapToBuckets(
	m map[string]int, order map[string]int,
) []DistributionBucket {
	buckets := make([]DistributionBucket, 0, len(m))
	for label, count := range m {
		buckets = append(buckets, DistributionBucket{
			Label: label, Count: count,
		})
	}
	sortBuckets(buckets, order)
	return buckets
}

// GetAnalyticsSessionShape returns distribution histograms for
// session length, duration, and autonomy ratio.

// --- Tools ---

// ToolCategoryCount holds a count and percentage for one tool
// category.
type ToolCategoryCount struct {
	Category string  `json:"category"`
	Count    int     `json:"count"`
	Pct      float64 `json:"pct"`
}

// ToolAgentBreakdown holds tool usage breakdown for one agent.
type ToolAgentBreakdown struct {
	Agent      string              `json:"agent"`
	Total      int                 `json:"total"`
	Categories []ToolCategoryCount `json:"categories"`
}

// ToolUsageAnalysis holds ranked usage for one concrete tool name.
type ToolUsageAnalysis struct {
	ToolName     string  `json:"tool_name"`
	Category     string  `json:"category"`
	CallCount    int     `json:"call_count"`
	SessionCount int     `json:"session_count"`
	Pct          float64 `json:"pct"`
}

// ToolTrendEntry holds tool call counts for one time bucket.
type ToolTrendEntry struct {
	Date  string         `json:"date"`
	ByCat map[string]int `json:"by_category"`
}

// ToolsAnalyticsResponse wraps tool usage analytics.
type ToolsAnalyticsResponse struct {
	TotalCalls int                  `json:"total_calls"`
	ByCategory []ToolCategoryCount  `json:"by_category"`
	ByAgent    []ToolAgentBreakdown `json:"by_agent"`
	ByTool     []ToolUsageAnalysis  `json:"by_tool"`
	Trend      []ToolTrendEntry     `json:"trend"`
}

// ToolAnalyticsRow is a backend-neutral intermediate row used to
// aggregate concrete tool usage after native stores apply their filters.
type ToolAnalyticsRow struct {
	SessionID string
	ToolName  string
	Category  string
	Agent     string
	Date      string
	Count     int
}

// SkillAgentBreakdown holds skill usage for one agent.
type SkillAgentBreakdown struct {
	Agent string `json:"agent"`
	Count int    `json:"count"`
}

// SkillProjectBreakdown holds skill usage for one project.
type SkillProjectBreakdown struct {
	Project string `json:"project"`
	Count   int    `json:"count"`
}

// SkillUsage holds usage metrics for one skill name.
type SkillUsage struct {
	SkillName        string                  `json:"skill_name"`
	CallCount        int                     `json:"call_count"`
	SessionCount     int                     `json:"session_count"`
	AgentBreakdown   []SkillAgentBreakdown   `json:"agent_breakdown"`
	ProjectBreakdown []SkillProjectBreakdown `json:"project_breakdown"`
	LastUsedAt       string                  `json:"last_used_at"`
	Pct              float64                 `json:"pct"`
}

// SkillTrendEntry holds skill call counts for one time bucket.
type SkillTrendEntry struct {
	Date    string         `json:"date"`
	BySkill map[string]int `json:"by_skill"`
}

// SkillsAnalyticsResponse wraps skill usage analytics.
type SkillsAnalyticsResponse struct {
	TotalSkillCalls int               `json:"total_skill_calls"`
	DistinctSkills  int               `json:"distinct_skills"`
	BySkill         []SkillUsage      `json:"by_skill"`
	Trend           []SkillTrendEntry `json:"trend"`
}

// SkillAnalyticsRow is a backend-neutral intermediate row used to
// aggregate skill usage after each store applies its native filters.
type SkillAnalyticsRow struct {
	SessionID  string
	SkillName  string
	Agent      string
	Project    string
	Date       string
	LastUsedAt string
	Count      int
}

type toolUsageAccumulator struct {
	toolName   string
	category   string
	callCount  int
	sessionIDs map[string]struct{}
}

// BuildToolsAnalytics folds backend-neutral tool rows into the public
// response shape. Tool names are trimmed and blank names are reported as
// Unknown so legacy rows remain visible instead of disappearing.
func BuildToolsAnalytics(rows []ToolAnalyticsRow) ToolsAnalyticsResponse {
	resp := ToolsAnalyticsResponse{
		ByCategory: []ToolCategoryCount{},
		ByAgent:    []ToolAgentBreakdown{},
		ByTool:     []ToolUsageAnalysis{},
		Trend:      []ToolTrendEntry{},
	}
	if len(rows) == 0 {
		return resp
	}

	catCounts := make(map[string]int)
	agentCats := make(map[string]map[string]int)
	trendBuckets := make(map[string]map[string]int)
	toolCounts := make(map[string]*toolUsageAccumulator)

	for _, row := range rows {
		if row.Count <= 0 {
			continue
		}
		catCounts[row.Category] += row.Count
		resp.TotalCalls += row.Count

		if agentCats[row.Agent] == nil {
			agentCats[row.Agent] = make(map[string]int)
		}
		agentCats[row.Agent][row.Category] += row.Count

		week := bucketDate(row.Date, "week")
		if trendBuckets[week] == nil {
			trendBuckets[week] = make(map[string]int)
		}
		trendBuckets[week][row.Category] += row.Count

		name := strings.TrimSpace(row.ToolName)
		if name == "" {
			name = "Unknown"
		}
		key := row.Category + "\x00" + name
		acc := toolCounts[key]
		if acc == nil {
			acc = &toolUsageAccumulator{
				toolName:   name,
				category:   row.Category,
				sessionIDs: map[string]struct{}{},
			}
			toolCounts[key] = acc
		}
		acc.callCount += row.Count
		if row.SessionID != "" {
			acc.sessionIDs[row.SessionID] = struct{}{}
		}
	}

	if resp.TotalCalls == 0 {
		return resp
	}

	resp.ByCategory = make([]ToolCategoryCount, 0, len(catCounts))
	for cat, count := range catCounts {
		pct := math.Round(
			float64(count)/float64(resp.TotalCalls)*1000,
		) / 10
		resp.ByCategory = append(resp.ByCategory,
			ToolCategoryCount{
				Category: cat, Count: count, Pct: pct,
			})
	}
	sort.Slice(resp.ByCategory, func(i, j int) bool {
		if resp.ByCategory[i].Count != resp.ByCategory[j].Count {
			return resp.ByCategory[i].Count > resp.ByCategory[j].Count
		}
		return resp.ByCategory[i].Category < resp.ByCategory[j].Category
	})

	agentKeys := make([]string, 0, len(agentCats))
	for agent := range agentCats {
		agentKeys = append(agentKeys, agent)
	}
	sort.Strings(agentKeys)
	resp.ByAgent = make([]ToolAgentBreakdown, 0, len(agentKeys))
	for _, agent := range agentKeys {
		cats := agentCats[agent]
		total := 0
		for _, c := range cats {
			total += c
		}
		catList := make([]ToolCategoryCount, 0, len(cats))
		for cat, count := range cats {
			pct := math.Round(
				float64(count)/float64(total)*1000,
			) / 10
			catList = append(catList, ToolCategoryCount{
				Category: cat, Count: count, Pct: pct,
			})
		}
		sort.Slice(catList, func(i, j int) bool {
			if catList[i].Count != catList[j].Count {
				return catList[i].Count > catList[j].Count
			}
			return catList[i].Category < catList[j].Category
		})
		resp.ByAgent = append(resp.ByAgent,
			ToolAgentBreakdown{
				Agent:      agent,
				Total:      total,
				Categories: catList,
			})
	}

	resp.ByTool = make([]ToolUsageAnalysis, 0, len(toolCounts))
	for _, acc := range toolCounts {
		pct := math.Round(
			float64(acc.callCount)/float64(resp.TotalCalls)*1000,
		) / 10
		resp.ByTool = append(resp.ByTool, ToolUsageAnalysis{
			ToolName:     acc.toolName,
			Category:     acc.category,
			CallCount:    acc.callCount,
			SessionCount: len(acc.sessionIDs),
			Pct:          pct,
		})
	}
	sort.Slice(resp.ByTool, func(i, j int) bool {
		if resp.ByTool[i].CallCount != resp.ByTool[j].CallCount {
			return resp.ByTool[i].CallCount > resp.ByTool[j].CallCount
		}
		if resp.ByTool[i].ToolName != resp.ByTool[j].ToolName {
			return resp.ByTool[i].ToolName < resp.ByTool[j].ToolName
		}
		return resp.ByTool[i].Category < resp.ByTool[j].Category
	})

	resp.Trend = make([]ToolTrendEntry, 0, len(trendBuckets))
	for week, cats := range trendBuckets {
		resp.Trend = append(resp.Trend, ToolTrendEntry{
			Date: week, ByCat: cats,
		})
	}
	sort.Slice(resp.Trend, func(i, j int) bool {
		return resp.Trend[i].Date < resp.Trend[j].Date
	})

	return resp
}

type skillUsageAccumulator struct {
	callCount     int
	sessionIDs    map[string]struct{}
	agentCounts   map[string]int
	projectCounts map[string]int
	lastUsedAt    string
}

// timestampAfter reports whether timestamp a is chronologically later
// than b. Both are parsed as UTC timestamps so callers stay correct when
// stores feed differing precisions (for example fractional seconds). It
// falls back to lexical comparison only when a value cannot be parsed.
func timestampAfter(a, b string) bool {
	if b == "" {
		return a != ""
	}
	if a == "" {
		return false
	}
	ta, aok := localTime(a, time.UTC)
	tb, bok := localTime(b, time.UTC)
	if aok && bok {
		return ta.After(tb)
	}
	return a > b
}

// skillsTrendGranularity normalizes a skills trend granularity value.
// Empty defaults to week, the endpoint's historical bucket size.
func skillsTrendGranularity(granularity string) string {
	switch granularity {
	case "day", "week", "month":
		return granularity
	default:
		return "week"
	}
}

// BuildSkillsAnalytics folds backend-neutral skill rows into the public
// response shape. Skill names are trimmed and empty names are ignored.
// granularity picks the trend bucket size (day, week, or month); empty
// or unknown values fall back to week. When from and to are present, the
// trend includes every bucket in that range, including zero-usage buckets.
func BuildSkillsAnalytics(
	rows []SkillAnalyticsRow, from, to, granularity string,
) SkillsAnalyticsResponse {
	resp := SkillsAnalyticsResponse{
		BySkill: []SkillUsage{},
		Trend:   []SkillTrendEntry{},
	}

	bucket := skillsTrendGranularity(granularity)
	bySkill := map[string]*skillUsageAccumulator{}
	trendBuckets := map[string]map[string]int{}

	for _, row := range rows {
		name := strings.TrimSpace(row.SkillName)
		if name == "" || row.Count <= 0 {
			continue
		}
		acc := bySkill[name]
		if acc == nil {
			acc = &skillUsageAccumulator{
				sessionIDs:    map[string]struct{}{},
				agentCounts:   map[string]int{},
				projectCounts: map[string]int{},
			}
			bySkill[name] = acc
		}
		acc.callCount += row.Count
		resp.TotalSkillCalls += row.Count
		if row.SessionID != "" {
			acc.sessionIDs[row.SessionID] = struct{}{}
		}
		if row.Agent != "" {
			acc.agentCounts[row.Agent] += row.Count
		}
		if row.Project != "" {
			acc.projectCounts[row.Project] += row.Count
		}
		if timestampAfter(row.LastUsedAt, acc.lastUsedAt) {
			acc.lastUsedAt = row.LastUsedAt
		}
		if row.Date != "" {
			date := bucketDate(row.Date, bucket)
			if trendBuckets[date] == nil {
				trendBuckets[date] = map[string]int{}
			}
			trendBuckets[date][name] += row.Count
		}
	}

	resp.DistinctSkills = len(bySkill)
	if resp.DistinctSkills == 0 {
		return resp
	}
	for _, entry := range TrendBucketRange(from, to, bucket) {
		if trendBuckets[entry.Date] == nil {
			trendBuckets[entry.Date] = map[string]int{}
		}
	}
	for name, acc := range bySkill {
		usage := SkillUsage{
			SkillName:        name,
			CallCount:        acc.callCount,
			SessionCount:     len(acc.sessionIDs),
			AgentBreakdown:   skillAgentBreakdowns(acc.agentCounts),
			ProjectBreakdown: skillProjectBreakdowns(acc.projectCounts),
			LastUsedAt:       acc.lastUsedAt,
			Pct: math.Round(
				float64(acc.callCount)/
					float64(resp.TotalSkillCalls)*1000,
			) / 10,
		}
		resp.BySkill = append(resp.BySkill, usage)
	}
	sort.Slice(resp.BySkill, func(i, j int) bool {
		if resp.BySkill[i].CallCount != resp.BySkill[j].CallCount {
			return resp.BySkill[i].CallCount > resp.BySkill[j].CallCount
		}
		return resp.BySkill[i].SkillName < resp.BySkill[j].SkillName
	})

	for date, skills := range trendBuckets {
		resp.Trend = append(resp.Trend, SkillTrendEntry{
			Date: date, BySkill: skills,
		})
	}
	sort.Slice(resp.Trend, func(i, j int) bool {
		return resp.Trend[i].Date < resp.Trend[j].Date
	})

	return resp
}

func skillAgentBreakdowns(
	counts map[string]int,
) []SkillAgentBreakdown {
	out := make([]SkillAgentBreakdown, 0, len(counts))
	for agent, count := range counts {
		out = append(out, SkillAgentBreakdown{
			Agent: agent, Count: count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

func skillProjectBreakdowns(
	counts map[string]int,
) []SkillProjectBreakdown {
	out := make([]SkillProjectBreakdown, 0, len(counts))
	for project, count := range counts {
		out = append(out, SkillProjectBreakdown{
			Project: project, Count: count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Project < out[j].Project
	})
	return out
}

func analyticsToolsQuery(
	placeholders string,
	modelPred string,
	includeMessageMeta bool,
) string {
	query := `SELECT tc.session_id, tc.category,
			TRIM(COALESCE(tc.tool_name, '')), COUNT(*)`
	if includeMessageMeta {
		query += `, COALESCE(m.timestamp, '')`
	}
	query += `
		FROM tool_calls tc`
	if includeMessageMeta {
		query += `
		LEFT JOIN messages m
			ON m.session_id = tc.session_id AND m.id = tc.message_id`
	}
	query += `
		WHERE tc.session_id IN ` + placeholders
	if modelPred != "" {
		query += `
			AND ` + modelPred
	}
	query += `
		GROUP BY tc.session_id, tc.category,
			TRIM(COALESCE(tc.tool_name, ''))`
	if includeMessageMeta {
		query += `, COALESCE(m.timestamp, '')`
	}
	return query
}

func analyticsSkillsQuery(
	placeholders string,
	modelPred string,
) string {
	query := `SELECT tc.session_id, TRIM(tc.skill_name), COUNT(*),
			COALESCE(m.timestamp, '')
		FROM tool_calls tc
		LEFT JOIN messages m
			ON m.session_id = tc.session_id AND m.id = tc.message_id
		WHERE tc.session_id IN ` + placeholders + `
			AND TRIM(COALESCE(tc.skill_name, '')) != ''`
	if modelPred != "" {
		query += `
			AND ` + modelPred
	}
	query += `
		GROUP BY tc.session_id, TRIM(tc.skill_name),
			COALESCE(m.timestamp, '')`
	return query
}

// GetAnalyticsTools returns tool usage analytics aggregated
// from the tool_calls table.

// Fetch filtered session IDs and their metadata.

// Query tool_calls for filtered sessions (chunked).

// ResolveSkillRowTime resolves the timestamp for a single skill call and
// applies the date and hour/day-of-week filters to it. The message
// timestamp is authoritative; the session timestamp is used only when the
// message has none. Because skill rows are bucketed by the call's own
// timestamp, the date/time filters must be applied here rather than to the
// owning session, so a session that started outside the range still
// contributes its in-range calls and drops its out-of-range ones.
//
// It returns the resolved timestamp, its local date, and whether the call
// passes the filters.
func (f AnalyticsFilter) ResolveSkillRowTime(
	messageTS, sessionTS string,
) (usedTS, date string, keep bool) {
	loc := f.location()
	usedTS = messageTS
	if strings.TrimSpace(usedTS) == "" {
		usedTS = sessionTS
	}
	date = localDate(usedTS, loc)
	if !inDateRange(date, f.From, f.To) {
		return usedTS, date, false
	}
	if f.HasTimeFilter() {
		t, ok := localTime(usedTS, loc)
		if !ok || !f.matchesTimeFilter(t) {
			return usedTS, date, false
		}
	}
	return usedTS, date, true
}

// GetAnalyticsSkills returns skill usage analytics aggregated
// from non-empty tool_calls.skill_name values. granularity picks the
// trend bucket size (day, week, or month); empty defaults to week.

// --- Velocity ---

// velocityMsg holds per-message data needed for velocity
// calculations.
type velocityMsg struct {
	role          string
	ts            time.Time
	valid         bool
	contentLength int
}

// queryVelocityMsgs fetches messages for a chunk of session IDs
// and appends them to sessionMsgs, keyed by session ID.
func (db *DB) queryVelocityMsgs(
	ctx context.Context,
	chunk []string,
	loc *time.Location,
	sessionMsgs map[string][]velocityMsg,
) error {
	ph, args := inPlaceholders(chunk)
	// COALESCE the nullable timestamp column to '' so a NULL (only present
	// on imported/migrated archives) does not fail rows.Scan with
	// "converting NULL to string is unsupported". localTime treats "" as
	// invalid, so the row is excluded from velocity stats rather than
	// crashing the analytics endpoint. This matches the NULL-safe
	// PostgreSQL and DuckDB velocity twins.
	q := `SELECT session_id, ordinal, role,
		COALESCE(timestamp, ''), content_length
		FROM messages
		WHERE session_id IN ` + ph + `
		ORDER BY session_id, ordinal`

	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf(
			"querying velocity messages: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var ordinal int
		var role, ts string
		var cl int
		if err := rows.Scan(
			&sid, &ordinal, &role, &ts, &cl,
		); err != nil {
			return fmt.Errorf(
				"scanning velocity msg: %w", err,
			)
		}
		t, ok := localTime(ts, loc)
		sessionMsgs[sid] = append(sessionMsgs[sid],
			velocityMsg{
				role: role, ts: t, valid: ok,
				contentLength: cl,
			})
	}
	return rows.Err()
}

// Percentiles holds p50 and p90 values.
type Percentiles struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
}

// VelocityOverview holds aggregate velocity metrics.
type VelocityOverview struct {
	TurnCycleSec          Percentiles `json:"turn_cycle_sec"`
	FirstResponseSec      Percentiles `json:"first_response_sec"`
	MsgsPerActiveMin      float64     `json:"msgs_per_active_min"`
	CharsPerActiveMin     float64     `json:"chars_per_active_min"`
	ToolCallsPerActiveMin float64     `json:"tool_calls_per_active_min"`
}

// VelocityBreakdown is velocity metrics for a subgroup.
type VelocityBreakdown struct {
	Label    string           `json:"label"`
	Sessions int              `json:"sessions"`
	Overview VelocityOverview `json:"overview"`
}

// VelocityResponse wraps overall and grouped velocity metrics.
type VelocityResponse struct {
	Overall      VelocityOverview    `json:"overall"`
	ByAgent      []VelocityBreakdown `json:"by_agent"`
	ByComplexity []VelocityBreakdown `json:"by_complexity"`
}

// complexityBucket returns the complexity label based on
// message count.
func complexityBucket(mc int) string {
	switch {
	case mc <= 15:
		return "1-15"
	case mc <= 60:
		return "16-60"
	default:
		return "61+"
	}
}

// velocityAccumulator collects raw values for a velocity group.
type velocityAccumulator struct {
	turnCycles     []float64
	firstResponses []float64
	totalMsgs      int
	totalChars     int
	totalToolCalls int
	activeMinutes  float64
	sessions       int
}

// populateVelocityAccumulator fetches per-message timestamps and tool
// counts for the given sessions and feeds them through
// processSessionVelocity into a single accumulator. Used by
// GetSessionStats, which already has its filtered session list and
// only needs the overall velocity slice — no agent/complexity
// breakdowns. Sessions with fewer than two messages are silently
// skipped, matching GetAnalyticsVelocity.
func populateVelocityAccumulator(
	ctx context.Context, db *DB, sessionIDs []string,
	loc *time.Location,
) (*velocityAccumulator, error) {
	accum := &velocityAccumulator{}
	if len(sessionIDs) == 0 {
		return accum, nil
	}

	sessionMsgs := make(map[string][]velocityMsg)
	if err := queryChunked(sessionIDs,
		func(chunk []string) error {
			return db.queryVelocityMsgs(
				ctx, chunk, loc, sessionMsgs,
			)
		}); err != nil {
		return nil, err
	}

	toolCountMap := make(map[string]int)
	err := queryChunked(sessionIDs,
		func(chunk []string) error {
			ph, chunkArgs := inPlaceholders(chunk)
			q := `SELECT session_id, COUNT(*)
				FROM tool_calls
				WHERE session_id IN ` + ph + `
				GROUP BY session_id`
			rows, qErr := db.getReader().QueryContext(
				ctx, q, chunkArgs...,
			)
			if qErr != nil {
				return fmt.Errorf(
					"querying velocity tool_calls: %w", qErr,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var sid string
				var count int
				if err := rows.Scan(&sid, &count); err != nil {
					return fmt.Errorf(
						"scanning velocity tool_call: %w", err,
					)
				}
				toolCountMap[sid] = count
			}
			return rows.Err()
		})
	if err != nil {
		return nil, err
	}

	for _, sid := range sessionIDs {
		msgs := sessionMsgs[sid]
		if len(msgs) < 2 {
			continue
		}
		processSessionVelocity(
			[]*velocityAccumulator{accum},
			msgs, toolCountMap[sid],
		)
	}
	return accum, nil
}

// processSessionVelocity updates every accumulator in accums with one
// session's turn cycles, first response, and throughput contribution.
// Shared by GetAnalyticsVelocity (which tracks overall/byAgent/
// byComplexity) and GetSessionStats (which tracks a single overall).
//
// Caller must pass len(msgs) >= 2 in ordinal order. The function
// itself bumps each accumulator's sessions counter.
func processSessionVelocity(
	accums []*velocityAccumulator,
	msgs []velocityMsg,
	toolCount int,
) {
	const maxCycleSec = 1800.0
	// Shared with the Top Sessions "active duration" SQL so the two
	// "active" definitions stay in lockstep.
	const maxGapSec = ActiveGapCapSec

	for _, a := range accums {
		a.sessions++
	}

	// Turn cycles: user→assistant transitions
	for i := 1; i < len(msgs); i++ {
		prev := msgs[i-1]
		cur := msgs[i]
		if !prev.valid || !cur.valid {
			continue
		}
		if prev.role == "user" && cur.role == "assistant" {
			delta := cur.ts.Sub(prev.ts).Seconds()
			if delta > 0 && delta <= maxCycleSec {
				for _, a := range accums {
					a.turnCycles = append(a.turnCycles, delta)
				}
			}
		}
	}

	// First response: first user → first assistant after it.
	// Scan by ordinal (conversation order), not timestamp.
	var firstUser, firstAsst *velocityMsg
	firstUserIdx := -1
	for i := range msgs {
		if msgs[i].role == "user" && msgs[i].valid {
			firstUser = &msgs[i]
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx >= 0 {
		for i := firstUserIdx + 1; i < len(msgs); i++ {
			if msgs[i].role == "assistant" && msgs[i].valid {
				firstAsst = &msgs[i]
				break
			}
		}
	}
	if firstUser != nil && firstAsst != nil {
		delta := firstAsst.ts.Sub(firstUser.ts).Seconds()
		// Clamp negative deltas to 0: ordinal order is
		// authoritative, so a negative delta means clock skew,
		// not a missing response.
		if delta < 0 {
			delta = 0
		}
		for _, a := range accums {
			a.firstResponses = append(a.firstResponses, delta)
		}
	}

	// Active minutes and throughput
	activeSec := 0.0
	asstChars := 0
	for i, m := range msgs {
		if m.role == "assistant" {
			asstChars += m.contentLength
		}
		if i > 0 && msgs[i-1].valid && m.valid {
			gap := m.ts.Sub(msgs[i-1].ts).Seconds()
			if gap > 0 {
				if gap > maxGapSec {
					gap = maxGapSec
				}
				activeSec += gap
			}
		}
	}
	activeMins := activeSec / 60.0
	if activeMins > 0 {
		for _, a := range accums {
			a.totalMsgs += len(msgs)
			a.totalChars += asstChars
			a.totalToolCalls += toolCount
			a.activeMinutes += activeMins
		}
	}
}

// turnCycleMean returns the arithmetic mean of turnCycles, or 0 when
// empty. Session stats reports mean alongside p50/p90 — the retained
// slice lets us compute both from the same sample.
func (a *velocityAccumulator) turnCycleMean() float64 {
	return meanFloats(a.turnCycles)
}

// firstResponseMean returns the arithmetic mean of firstResponses, or
// 0 when empty. See turnCycleMean for rationale.
func (a *velocityAccumulator) firstResponseMean() float64 {
	return meanFloats(a.firstResponses)
}

func meanFloats(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func (a *velocityAccumulator) computeOverview() VelocityOverview {
	sort.Float64s(a.turnCycles)
	sort.Float64s(a.firstResponses)

	var v VelocityOverview
	v.TurnCycleSec = Percentiles{
		P50: math.Round(
			percentileFloat(a.turnCycles, 0.5)*10) / 10,
		P90: math.Round(
			percentileFloat(a.turnCycles, 0.9)*10) / 10,
	}
	v.FirstResponseSec = Percentiles{
		P50: math.Round(
			percentileFloat(a.firstResponses, 0.5)*10) / 10,
		P90: math.Round(
			percentileFloat(a.firstResponses, 0.9)*10) / 10,
	}
	if a.activeMinutes > 0 {
		v.MsgsPerActiveMin = math.Round(
			float64(a.totalMsgs)/a.activeMinutes*10) / 10
		v.CharsPerActiveMin = math.Round(
			float64(a.totalChars)/a.activeMinutes*10) / 10
		v.ToolCallsPerActiveMin = math.Round(
			float64(a.totalToolCalls)/a.activeMinutes*10) / 10
	}
	return v
}

// GetAnalyticsVelocity computes turn cycle, first response, and
// throughput metrics with breakdowns by agent and complexity.

// Phase 1: Get filtered session metadata

// Process per-session metrics

// Build by-agent breakdowns

// Build by-complexity breakdowns

// --- Signals ---

// SignalsAnalyticsResponse holds aggregated session signal data.
type SignalsAnalyticsResponse struct {
	ScoredSessions                int                          `json:"scored_sessions"`
	UnscoredSessions              int                          `json:"unscored_sessions"`
	GradeDistribution             map[string]int               `json:"grade_distribution"`
	AvgHealthScore                *float64                     `json:"avg_health_score"`
	OutcomeDistribution           map[string]int               `json:"outcome_distribution"`
	OutcomeConfidenceDistribution map[string]int               `json:"outcome_confidence_distribution"`
	ToolHealth                    SignalsToolHealth            `json:"tool_health"`
	ContextHealth                 SignalsContextHealth         `json:"context_health"`
	QualityHealth                 SignalsQualityHealth         `json:"quality_health"`
	Trend                         []SignalsTrendBucket         `json:"trend"`
	ByAgent                       []SignalsAgentRow            `json:"by_agent"`
	ByProject                     []SignalsProjectRow          `json:"by_project"`
	Calibration                   map[string]SignalCalibration `json:"calibration"`
}

// SignalsToolHealth holds aggregate tool failure metrics.
type SignalsToolHealth struct {
	TotalFailureSignals  int     `json:"total_failure_signals"`
	TotalRetries         int     `json:"total_retries"`
	TotalEditChurn       int     `json:"total_edit_churn"`
	SessionsWithFailures int     `json:"sessions_with_failures"`
	FailureRate          float64 `json:"failure_rate"`
}

// SignalsContextHealth holds aggregate context pressure metrics.
type SignalsContextHealth struct {
	AvgCompactionCount        float64  `json:"avg_compaction_count"`
	SessionsWithCompaction    int      `json:"sessions_with_compaction"`
	MidTaskCompactionCount    int      `json:"mid_task_compaction_count"`
	SessionsWithMidTaskCompac int      `json:"sessions_with_mid_task_compaction"`
	SessionsWithContextData   int      `json:"sessions_with_context_data"`
	AvgContextPressure        *float64 `json:"avg_context_pressure"`
	HighPressureSessions      int      `json:"high_pressure_sessions"`
}

// SignalsQualityHealth holds aggregate deterministic quality-signal
// metrics. Totals are raw signal sums; SessionsWithSignal counts
// sessions where each signal was non-zero.
type SignalsQualityHealth struct {
	ComputedSessions   int                 `json:"computed_sessions"`
	Totals             QualitySignalTotals `json:"totals"`
	SessionsWithSignal QualitySignalTotals `json:"sessions_with_signal"`
}

// QualitySignalTotals is shared by aggregate quality-signal totals.
type QualitySignalTotals struct {
	ShortPromptCount            int `json:"short_prompt_count"`
	UnstructuredStart           int `json:"unstructured_start"`
	MissingSuccessCriteriaCount int `json:"missing_success_criteria_count"`
	MissingVerificationCount    int `json:"missing_verification_count"`
	DuplicatePromptCount        int `json:"duplicate_prompt_count"`
	NoCodeContextCount          int `json:"no_code_context_count"`
	RunawayToolLoopCount        int `json:"runaway_tool_loop_count"`
	FrustrationMarkerCount      int `json:"frustration_marker_count"`
}

// SignalCalibration compares sessions with a signal to sessions
// without it for the active filter slice.
type SignalCalibration struct {
	Signal                 string   `json:"signal"`
	AffectedSessions       int      `json:"affected_sessions"`
	BaselineSessions       int      `json:"baseline_sessions"`
	AffectedIncompleteRate float64  `json:"affected_incomplete_rate"`
	BaselineIncompleteRate float64  `json:"baseline_incomplete_rate"`
	IncompleteLift         *float64 `json:"incomplete_lift"`
	AvgScoreDelta          *float64 `json:"avg_score_delta"`
}

// SignalSessionsResponse returns concrete sessions that triggered
// an aggregate signal, including the best available message excerpt.
type SignalSessionsResponse struct {
	Signal   string                 `json:"signal"`
	Sessions []SignalSessionExample `json:"sessions" nullable:"false"`
}

type SignalSessionExample struct {
	SessionID      string  `json:"session_id"`
	Project        string  `json:"project"`
	Agent          string  `json:"agent"`
	Date           string  `json:"date"`
	IsAutomated    bool    `json:"is_automated"`
	Outcome        string  `json:"outcome"`
	HealthScore    *int    `json:"health_score"`
	HealthGrade    *string `json:"health_grade"`
	SignalTotal    int     `json:"signal_total"`
	ReasonCode     string  `json:"reason_code"`
	Excerpt        string  `json:"excerpt"`
	MessageOrdinal *int    `json:"message_ordinal,omitempty"`
	FailureSignals int     `json:"failure_signals"`
	Retries        int     `json:"retries"`
	EditChurn      int     `json:"edit_churn"`
}

type SignalMessage struct {
	SessionID  string
	Ordinal    int
	Role       string
	Content    string
	Timestamp  string
	IsSystem   bool
	HasToolUse bool
}

// SignalsTrendBucket holds signal data for one date bucket.
type SignalsTrendBucket struct {
	Date              string   `json:"date"`
	SessionCount      int      `json:"session_count"`
	AvgHealthScore    *float64 `json:"avg_health_score"`
	Completed         int      `json:"completed"`
	Errored           int      `json:"errored"`
	Abandoned         int      `json:"abandoned"`
	AvgFailureSignals float64  `json:"avg_failure_signals"`
}

// SignalsAgentRow holds signal data grouped by agent.
type SignalsAgentRow struct {
	Agent             string   `json:"agent"`
	SessionCount      int      `json:"session_count"`
	AvgHealthScore    *float64 `json:"avg_health_score"`
	CompletedRate     float64  `json:"completed_rate"`
	AvgFailureSignals float64  `json:"avg_failure_signals"`
}

// SignalsProjectRow holds signal data grouped by project.
type SignalsProjectRow struct {
	Project           string   `json:"project"`
	SessionCount      int      `json:"session_count"`
	AvgHealthScore    *float64 `json:"avg_health_score"`
	CompletedRate     float64  `json:"completed_rate"`
	AvgFailureSignals float64  `json:"avg_failure_signals"`
}

// SignalRow holds per-session signal data from the query.
// Exported so the PostgreSQL store can build the same rows
// from its own SELECT and feed them into AggregateSignals
// without duplicating the aggregation logic.
type SignalRow struct {
	ID                          string
	Agent                       string
	Project                     string
	Date                        string
	FirstMessage                *string
	IsAutomated                 bool
	HealthScore                 *int
	HealthGrade                 *string
	Outcome                     string
	OutcomeConfidence           string
	ToolFailureSignalCount      int
	ToolRetryCount              int
	EditChurnCount              int
	CompactionCount             int
	MidTaskCompactionCount      int
	ContextPressureMax          *float64
	QualitySignalVersion        int
	ShortPromptCount            int
	UnstructuredStart           bool
	MissingSuccessCriteriaCount int
	MissingVerificationCount    int
	DuplicatePromptCount        int
	NoCodeContextCount          int
	RunawayToolLoopCount        int
	FrustrationMarkerCount      int
}

// GetAnalyticsSignals returns aggregated session signal data.
//
// Signals are session-scoped: the counts come from persisted per-session
// columns computed at parse time (health, context pressure, compaction,
// quality markers) that describe the whole session, not one model's turns.
// When a model filter is active the session set is scoped to sessions that
// used the model, but the totals stay session-level aggregates and are not
// re-attributed per model -- per-model signal attribution is not well defined
// for most signals and is intentionally not computed here. The PostgreSQL and
// DuckDB stores mirror this.

// GetAnalyticsSignalSessions returns concrete examples for a
// signal within the current analytics filter. Candidates are ranked by the
// session-level signal counts (see GetAnalyticsSignals); under a model filter
// the drill-down evidence is model-scoped while the ranking stays session-level.

func SignalCandidates(
	rows []SignalRow,
	signal string,
	limit int,
) []SignalRow {
	candidates := make([]SignalRow, 0)
	for _, r := range rows {
		if signalValue(r, signal) > 0 {
			candidates = append(candidates, r)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		iv := signalValue(candidates[i], signal)
		jv := signalValue(candidates[j], signal)
		if iv != jv {
			return iv > jv
		}
		ib := isIncompleteOrLowQuality(candidates[i])
		jb := isIncompleteOrLowQuality(candidates[j])
		if ib != jb {
			return ib
		}
		if candidates[i].HealthScore != nil &&
			candidates[j].HealthScore != nil &&
			*candidates[i].HealthScore != *candidates[j].HealthScore {
			return *candidates[i].HealthScore <
				*candidates[j].HealthScore
		}
		return candidates[i].Date > candidates[j].Date
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func BuildSignalExamples(
	rows []SignalRow,
	messages map[string][]SignalMessage,
	signal string,
) []SignalSessionExample {
	examples := make([]SignalSessionExample, 0, len(rows))
	for _, r := range rows {
		excerpt, ordinal := signalExcerpt(
			signal, r, messages[r.ID],
		)
		examples = append(examples, SignalSessionExample{
			SessionID:      r.ID,
			Project:        r.Project,
			Agent:          r.Agent,
			Date:           r.Date,
			IsAutomated:    r.IsAutomated,
			Outcome:        r.Outcome,
			HealthScore:    r.HealthScore,
			HealthGrade:    r.HealthGrade,
			SignalTotal:    signalValue(r, signal),
			ReasonCode:     signalReason(signal),
			Excerpt:        truncateExcerpt(excerpt, 180),
			MessageOrdinal: ordinal,
			FailureSignals: r.ToolFailureSignalCount,
			Retries:        r.ToolRetryCount,
			EditChurn:      r.EditChurnCount,
		})
	}
	return examples
}

func signalExcerpt(
	signal string,
	r SignalRow,
	messages []SignalMessage,
) (string, *int) {
	switch signal {
	case "frustration_marker_count":
		if content, ordinal, ok := firstFrustrationPrompt(messages); ok {
			return content, ordinal
		}
	case "short_prompt_count":
		if content, ordinal, ok := firstShortPrompt(messages); ok {
			return content, ordinal
		}
	case "duplicate_prompt_count":
		if content, ordinal, ok := firstRepeatedPrompt(messages); ok {
			return content, ordinal
		}
	case "tool_failure_signals", "tool_retries", "edit_churn",
		"runaway_tool_loop_count":
		if content, ordinal, ok := firstToolUseMessage(messages); ok {
			return content, ordinal
		}
	case "outcome_errored", "outcome_abandoned", "outcome_completed",
		"sessions_with_compaction", "mid_task_compaction_count",
		"high_pressure_sessions":
		if content, ordinal, ok := lastSessionMessage(messages); ok {
			return content, ordinal
		}
	}
	if content, ordinal, ok := firstSubstantiveUserMessage(messages); ok {
		return content, ordinal
	}
	if r.FirstMessage != nil {
		return *r.FirstMessage, nil
	}
	return "", nil
}

func firstFrustrationPrompt(
	messages []SignalMessage,
) (string, *int, bool) {
	for _, m := range messages {
		if !isUserEvidenceMessage(m) {
			continue
		}
		if signals.IsFrustrationMarker(m.Content) {
			content, ordinal := messageEvidence(m)
			return content, ordinal, true
		}
	}
	return "", nil, false
}

func firstShortPrompt(
	messages []SignalMessage,
) (string, *int, bool) {
	firstUserOrdinal, ok := firstSubstantiveUserOrdinal(messages)
	if !ok {
		return "", nil, false
	}
	var previousAssistantTimestamp string
	hasPreviousAssistant := false
	userSinceLastAssistant := false
	for _, m := range messages {
		if m.IsSystem {
			continue
		}
		if m.Role == "assistant" {
			previousAssistantTimestamp = m.Timestamp
			hasPreviousAssistant = true
			userSinceLastAssistant = false
			continue
		}
		if !isUserEvidenceMessage(m) {
			continue
		}
		firstAfterAssistant := !userSinceLastAssistant
		normalized := normalizeEvidenceText(m.Content)
		if isControlEvidencePrompt(normalized) {
			continue
		}
		userSinceLastAssistant = true
		if len(normalized) >= 30 {
			continue
		}
		if m.Ordinal == firstUserOrdinal ||
			(firstAfterAssistant &&
				hasStaleEvidenceAssistantBefore(
					m.Timestamp,
					previousAssistantTimestamp,
					hasPreviousAssistant,
				)) {
			content, ordinal := messageEvidence(m)
			return content, ordinal, true
		}
	}
	return "", nil, false
}

func firstSubstantiveUserOrdinal(
	messages []SignalMessage,
) (int, bool) {
	for _, m := range messages {
		if !isUserEvidenceMessage(m) {
			continue
		}
		if isControlEvidencePrompt(normalizeEvidenceText(m.Content)) {
			continue
		}
		return m.Ordinal, true
	}
	return 0, false
}

func hasStaleEvidenceAssistantBefore(
	userTimestamp string,
	assistantTimestamp string,
	hasPreviousAssistant bool,
) bool {
	if !hasPreviousAssistant {
		return false
	}
	userTime, ok := parseEvidenceTime(userTimestamp)
	if !ok {
		return false
	}
	assistantTime, ok := parseEvidenceTime(assistantTimestamp)
	if !ok {
		return false
	}
	return userTime.Sub(assistantTime) > 30*time.Minute
}

func parseEvidenceTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
	} {
		t, err := time.Parse(layout, raw)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func firstRepeatedPrompt(
	messages []SignalMessage,
) (string, *int, bool) {
	type prompt struct {
		normalized string
		tokens     []string
	}
	seen := make([]prompt, 0, len(messages))
	for _, m := range messages {
		if !isUserEvidenceMessage(m) {
			continue
		}
		key := evidencePromptKey(m.Content)
		if key == "" {
			continue
		}
		tokens := evidencePromptTokens(key)
		if len(tokens) < 4 {
			continue
		}
		for _, prev := range seen {
			if key == prev.normalized ||
				evidenceJaccard(tokens, prev.tokens) >= 0.85 {
				content, ordinal := messageEvidence(m)
				return content, ordinal, true
			}
		}
		seen = append(seen, prompt{
			normalized: key,
			tokens:     tokens,
		})
	}
	return "", nil, false
}

func firstToolUseMessage(
	messages []SignalMessage,
) (string, *int, bool) {
	for _, m := range messages {
		if m.IsSystem || !m.HasToolUse {
			continue
		}
		content, ordinal := messageEvidence(m)
		return content, ordinal, true
	}
	return "", nil, false
}

func lastSessionMessage(
	messages []SignalMessage,
) (string, *int, bool) {
	for _, v := range slices.Backward(messages) {
		m := v
		if m.IsSystem {
			continue
		}
		if !isSubstantiveEvidence(m.Content) && !m.HasToolUse {
			continue
		}
		content, ordinal := messageEvidence(m)
		return content, ordinal, true
	}
	return "", nil, false
}

func firstSubstantiveUserMessage(
	messages []SignalMessage,
) (string, *int, bool) {
	for _, m := range messages {
		if !isUserEvidenceMessage(m) {
			continue
		}
		content, ordinal := messageEvidence(m)
		return content, ordinal, true
	}
	return "", nil, false
}

func isUserEvidenceMessage(m SignalMessage) bool {
	return m.Role == "user" &&
		!m.IsSystem &&
		isSubstantiveEvidence(m.Content)
}

func isSubstantiveEvidence(content string) bool {
	return normalizeEvidenceText(content) != ""
}

func evidencePromptKey(content string) string {
	key := normalizeEvidenceText(content)
	if len(key) < 20 || isControlEvidencePrompt(key) {
		return ""
	}
	return key
}

func evidencePromptTokens(normalized string) []string {
	return strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func evidenceJaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	seen := map[string]struct{}{}
	for _, token := range a {
		seen[token] = struct{}{}
	}
	intersections := 0
	union := len(seen)
	for _, token := range b {
		if _, ok := seen[token]; ok {
			intersections++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersections) / float64(union)
}

func isControlEvidencePrompt(normalized string) bool {
	switch normalized {
	case "yes", "y", "no", "n", "ok", "okay",
		"continue", "go ahead", "proceed",
		"do it", "done", "thanks", "thank you",
		"please continue", "keep going":
		return true
	default:
		return false
	}
}

func messageEvidence(m SignalMessage) (string, *int) {
	ordinal := m.Ordinal
	content := strings.TrimSpace(m.Content)
	if content == "" && m.HasToolUse {
		content = "Tool-use turn"
	}
	return content, &ordinal
}

func signalReason(signal string) string {
	switch signal {
	case "short_prompt_count":
		return "short-start-contextual"
	case "unstructured_start":
		return "unstructured-task-start"
	case "missing_success_criteria_count":
		return "missing-observable-acceptance"
	case "missing_verification_count":
		return "missing-targeted-verification-path"
	case "duplicate_prompt_count":
		return "possible-stuck-reask"
	case "no_code_context_count":
		return "code-task-without-context"
	case "runaway_tool_loop_count":
		return "repeated-failing-tool-cycle"
	case "frustration_marker_count":
		return "frustration-marker"
	case "outcome_errored":
		return "errored-outcome"
	case "outcome_abandoned":
		return "abandoned-outcome"
	case "outcome_completed":
		return "completed-outcome"
	case "tool_failure_signals":
		return "tool-failure-signal"
	case "tool_retries":
		return "tool-retry"
	case "edit_churn":
		return "edit-churn"
	case "sessions_with_compaction":
		return "context-compaction"
	case "mid_task_compaction_count":
		return "mid-task-compaction"
	case "high_pressure_sessions":
		return "high-context-pressure"
	default:
		return signal
	}
}

func normalizeEvidenceText(content string) string {
	lower := strings.ToLower(strings.TrimSpace(content))
	return spaceReplacer(lower)
}

func truncateExcerpt(s string, max int) string {
	s = strings.TrimSpace(spaceReplacer(s))
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func spaceReplacer(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// AggregateSignals builds the response from collected rows.
// Exported so the PostgreSQL store can reuse the same
// aggregation logic instead of re-implementing it.
func AggregateSignals(
	all []SignalRow,
) SignalsAnalyticsResponse {
	resp := SignalsAnalyticsResponse{
		GradeDistribution:             make(map[string]int),
		OutcomeDistribution:           make(map[string]int),
		OutcomeConfidenceDistribution: make(map[string]int),
		Calibration:                   make(map[string]SignalCalibration),
	}

	if len(all) == 0 {
		resp.Trend = []SignalsTrendBucket{}
		resp.ByAgent = []SignalsAgentRow{}
		resp.ByProject = []SignalsProjectRow{}
		return resp
	}

	type groupAccum struct {
		count            int
		healthScoreSum   int
		healthScoreCount int
		completed        int
		failureSignalSum int
	}

	totalCount := len(all)
	var healthScoreSum int
	var healthScoreCount int

	agentMap := make(map[string]*groupAccum)
	projectMap := make(map[string]*groupAccum)
	trendMap := make(map[string]*groupAccum)

	// Also track trend-specific outcome counts.
	type trendExtra struct {
		errored   int
		abandoned int
	}
	trendExtras := make(map[string]*trendExtra)

	for _, r := range all {
		// Scored vs unscored
		if r.HealthScore != nil {
			resp.ScoredSessions++
			healthScoreSum += *r.HealthScore
			healthScoreCount++
		} else {
			resp.UnscoredSessions++
		}

		// Grade distribution
		if r.HealthGrade != nil && *r.HealthGrade != "" {
			resp.GradeDistribution[*r.HealthGrade]++
		}

		// Outcome distribution
		if r.Outcome != "" {
			resp.OutcomeDistribution[r.Outcome]++
		}
		if r.OutcomeConfidence != "" {
			resp.OutcomeConfidenceDistribution[r.OutcomeConfidence]++
		}

		// Tool health
		resp.ToolHealth.TotalFailureSignals += r.ToolFailureSignalCount
		resp.ToolHealth.TotalRetries += r.ToolRetryCount
		resp.ToolHealth.TotalEditChurn += r.EditChurnCount
		if r.ToolFailureSignalCount > 0 {
			resp.ToolHealth.SessionsWithFailures++
		}

		// Context health
		if r.CompactionCount > 0 {
			resp.ContextHealth.SessionsWithCompaction++
		}
		resp.ContextHealth.AvgCompactionCount += float64(
			r.CompactionCount,
		)
		resp.ContextHealth.MidTaskCompactionCount +=
			r.MidTaskCompactionCount
		if r.MidTaskCompactionCount > 0 {
			resp.ContextHealth.SessionsWithMidTaskCompac++
		}
		if r.ContextPressureMax != nil {
			resp.ContextHealth.SessionsWithContextData++
			if *r.ContextPressureMax >= 0.8 {
				resp.ContextHealth.HighPressureSessions++
			}
		}

		accumulateQualityHealth(&resp.QualityHealth, r)

		// Accumulate by agent
		ga := agentMap[r.Agent]
		if ga == nil {
			ga = &groupAccum{}
			agentMap[r.Agent] = ga
		}
		ga.count++
		ga.failureSignalSum += r.ToolFailureSignalCount
		if r.HealthScore != nil {
			ga.healthScoreSum += *r.HealthScore
			ga.healthScoreCount++
		}
		if r.Outcome == "completed" {
			ga.completed++
		}

		// Accumulate by project
		gp := projectMap[r.Project]
		if gp == nil {
			gp = &groupAccum{}
			projectMap[r.Project] = gp
		}
		gp.count++
		gp.failureSignalSum += r.ToolFailureSignalCount
		if r.HealthScore != nil {
			gp.healthScoreSum += *r.HealthScore
			gp.healthScoreCount++
		}
		if r.Outcome == "completed" {
			gp.completed++
		}

		// Accumulate by date (trend)
		gt := trendMap[r.Date]
		if gt == nil {
			gt = &groupAccum{}
			trendMap[r.Date] = gt
		}
		gt.count++
		gt.failureSignalSum += r.ToolFailureSignalCount
		if r.HealthScore != nil {
			gt.healthScoreSum += *r.HealthScore
			gt.healthScoreCount++
		}
		if r.Outcome == "completed" {
			gt.completed++
		}
		te := trendExtras[r.Date]
		if te == nil {
			te = &trendExtra{}
			trendExtras[r.Date] = te
		}
		if r.Outcome == "errored" {
			te.errored++
		}
		if r.Outcome == "abandoned" {
			te.abandoned++
		}
	}

	// Average health score
	if healthScoreCount > 0 {
		avg := math.Round(
			float64(healthScoreSum)/
				float64(healthScoreCount)*10,
		) / 10
		resp.AvgHealthScore = &avg
	}

	// Tool health failure rate
	if totalCount > 0 {
		resp.ToolHealth.FailureRate = math.Round(
			float64(resp.ToolHealth.SessionsWithFailures)/
				float64(totalCount)*1000,
		) / 10
	}

	// Context health averages
	if totalCount > 0 {
		resp.ContextHealth.AvgCompactionCount = math.Round(
			resp.ContextHealth.AvgCompactionCount/
				float64(totalCount)*10,
		) / 10
	}
	if resp.ContextHealth.SessionsWithContextData > 0 {
		var pressureSum float64
		for _, r := range all {
			if r.ContextPressureMax != nil {
				pressureSum += *r.ContextPressureMax
			}
		}
		avg := math.Round(
			pressureSum/
				float64(
					resp.ContextHealth.SessionsWithContextData,
				)*1000,
		) / 1000
		resp.ContextHealth.AvgContextPressure = &avg
	}

	// Build trend (sorted by date)
	resp.Trend = make(
		[]SignalsTrendBucket, 0, len(trendMap),
	)
	for date, g := range trendMap {
		bucket := SignalsTrendBucket{
			Date:         date,
			SessionCount: g.count,
			Completed:    g.completed,
		}
		if te := trendExtras[date]; te != nil {
			bucket.Errored = te.errored
			bucket.Abandoned = te.abandoned
		}
		if g.healthScoreCount > 0 {
			avg := math.Round(
				float64(g.healthScoreSum)/
					float64(g.healthScoreCount)*10,
			) / 10
			bucket.AvgHealthScore = &avg
		}
		if g.count > 0 {
			bucket.AvgFailureSignals = math.Round(
				float64(g.failureSignalSum)/
					float64(g.count)*10,
			) / 10
		}
		resp.Trend = append(resp.Trend, bucket)
	}
	sort.Slice(resp.Trend, func(i, j int) bool {
		return resp.Trend[i].Date < resp.Trend[j].Date
	})

	// Build by-agent (sorted alphabetically)
	agentKeys := make([]string, 0, len(agentMap))
	for k := range agentMap {
		agentKeys = append(agentKeys, k)
	}
	sort.Strings(agentKeys)
	resp.ByAgent = make(
		[]SignalsAgentRow, 0, len(agentKeys),
	)
	for _, agent := range agentKeys {
		g, ok := agentMap[agent]
		if !ok || g == nil {
			continue
		}
		row := SignalsAgentRow{
			Agent:        agent,
			SessionCount: g.count,
		}
		if g.healthScoreCount > 0 {
			avg := math.Round(
				float64(g.healthScoreSum)/
					float64(g.healthScoreCount)*10,
			) / 10
			row.AvgHealthScore = &avg
		}
		if g.count > 0 {
			row.CompletedRate = math.Round(
				float64(g.completed)/
					float64(g.count)*1000,
			) / 10
			row.AvgFailureSignals = math.Round(
				float64(g.failureSignalSum)/
					float64(g.count)*10,
			) / 10
		}
		resp.ByAgent = append(resp.ByAgent, row)
	}

	// Build by-project (sorted by session count desc)
	resp.ByProject = make(
		[]SignalsProjectRow, 0, len(projectMap),
	)
	for project, g := range projectMap {
		row := SignalsProjectRow{
			Project:      project,
			SessionCount: g.count,
		}
		if g.healthScoreCount > 0 {
			avg := math.Round(
				float64(g.healthScoreSum)/
					float64(g.healthScoreCount)*10,
			) / 10
			row.AvgHealthScore = &avg
		}
		if g.count > 0 {
			row.CompletedRate = math.Round(
				float64(g.completed)/
					float64(g.count)*1000,
			) / 10
			row.AvgFailureSignals = math.Round(
				float64(g.failureSignalSum)/
					float64(g.count)*10,
			) / 10
		}
		resp.ByProject = append(resp.ByProject, row)
	}
	sort.Slice(resp.ByProject, func(i, j int) bool {
		if resp.ByProject[i].SessionCount !=
			resp.ByProject[j].SessionCount {
			return resp.ByProject[i].SessionCount >
				resp.ByProject[j].SessionCount
		}
		return resp.ByProject[i].Project <
			resp.ByProject[j].Project
	})
	resp.Calibration = buildSignalCalibrations(all)

	return resp
}

func accumulateQualityHealth(
	q *SignalsQualityHealth, r SignalRow,
) {
	if r.QualitySignalVersion <= 0 {
		return
	}
	q.ComputedSessions++
	q.Totals.ShortPromptCount += r.ShortPromptCount
	if r.ShortPromptCount > 0 {
		q.SessionsWithSignal.ShortPromptCount++
	}
	if r.UnstructuredStart {
		q.Totals.UnstructuredStart++
		q.SessionsWithSignal.UnstructuredStart++
	}
	q.Totals.MissingSuccessCriteriaCount +=
		r.MissingSuccessCriteriaCount
	if r.MissingSuccessCriteriaCount > 0 {
		q.SessionsWithSignal.MissingSuccessCriteriaCount++
	}
	q.Totals.MissingVerificationCount += r.MissingVerificationCount
	if r.MissingVerificationCount > 0 {
		q.SessionsWithSignal.MissingVerificationCount++
	}
	q.Totals.DuplicatePromptCount += r.DuplicatePromptCount
	if r.DuplicatePromptCount > 0 {
		q.SessionsWithSignal.DuplicatePromptCount++
	}
	q.Totals.NoCodeContextCount += r.NoCodeContextCount
	if r.NoCodeContextCount > 0 {
		q.SessionsWithSignal.NoCodeContextCount++
	}
	q.Totals.RunawayToolLoopCount += r.RunawayToolLoopCount
	if r.RunawayToolLoopCount > 0 {
		q.SessionsWithSignal.RunawayToolLoopCount++
	}
	q.Totals.FrustrationMarkerCount += r.FrustrationMarkerCount
	if r.FrustrationMarkerCount > 0 {
		q.SessionsWithSignal.FrustrationMarkerCount++
	}
}

func buildSignalCalibrations(
	rows []SignalRow,
) map[string]SignalCalibration {
	signals := []string{
		"tool_failure_signals",
		"tool_retries",
		"edit_churn",
		"sessions_with_compaction",
		"mid_task_compaction_count",
		"high_pressure_sessions",
		"short_prompt_count",
		"unstructured_start",
		"missing_success_criteria_count",
		"missing_verification_count",
		"duplicate_prompt_count",
		"no_code_context_count",
		"runaway_tool_loop_count",
		"frustration_marker_count",
	}
	out := make(map[string]SignalCalibration, len(signals))
	for _, signal := range signals {
		out[signal] = calibrateSignal(rows, signal)
	}
	return out
}

func calibrateSignal(rows []SignalRow, signal string) SignalCalibration {
	type side struct {
		count      int
		incomplete int
		scoreSum   int
		scoreCount int
	}
	var affected, baseline side
	for _, r := range rows {
		target := &baseline
		if signalValue(r, signal) > 0 {
			target = &affected
		}
		target.count++
		if isIncompleteOrLowQuality(r) {
			target.incomplete++
		}
		if r.HealthScore != nil {
			target.scoreSum += *r.HealthScore
			target.scoreCount++
		}
	}
	result := SignalCalibration{
		Signal:           signal,
		AffectedSessions: affected.count,
		BaselineSessions: baseline.count,
	}
	if affected.count > 0 {
		result.AffectedIncompleteRate = round1(
			float64(affected.incomplete) /
				float64(affected.count) * 100,
		)
	}
	if baseline.count > 0 {
		result.BaselineIncompleteRate = round1(
			float64(baseline.incomplete) /
				float64(baseline.count) * 100,
		)
	}
	if baseline.count > 0 &&
		result.BaselineIncompleteRate > 0 &&
		affected.count > 0 {
		lift := round1(
			result.AffectedIncompleteRate /
				result.BaselineIncompleteRate,
		)
		result.IncompleteLift = &lift
	}
	if affected.scoreCount > 0 && baseline.scoreCount > 0 {
		delta := round1(
			float64(affected.scoreSum)/
				float64(affected.scoreCount) -
				float64(baseline.scoreSum)/
					float64(baseline.scoreCount),
		)
		result.AvgScoreDelta = &delta
	}
	return result
}

func signalValue(r SignalRow, signal string) int {
	switch signal {
	case "outcome_errored":
		if r.Outcome == "errored" {
			return 1
		}
	case "outcome_abandoned":
		if r.Outcome == "abandoned" {
			return 1
		}
	case "outcome_completed":
		if r.Outcome == "completed" {
			return 1
		}
	case "tool_failure_signals":
		return r.ToolFailureSignalCount
	case "tool_retries":
		return r.ToolRetryCount
	case "edit_churn":
		return r.EditChurnCount
	case "sessions_with_compaction":
		if r.CompactionCount > 0 {
			return 1
		}
	case "mid_task_compaction_count":
		return r.MidTaskCompactionCount
	case "high_pressure_sessions":
		if r.ContextPressureMax != nil &&
			*r.ContextPressureMax >= 0.8 {
			return 1
		}
	case "short_prompt_count":
		return r.ShortPromptCount
	case "unstructured_start":
		if r.UnstructuredStart {
			return 1
		}
	case "missing_success_criteria_count":
		return r.MissingSuccessCriteriaCount
	case "missing_verification_count":
		return r.MissingVerificationCount
	case "duplicate_prompt_count":
		return r.DuplicatePromptCount
	case "no_code_context_count":
		return r.NoCodeContextCount
	case "runaway_tool_loop_count":
		return r.RunawayToolLoopCount
	case "frustration_marker_count":
		return r.FrustrationMarkerCount
	}
	return 0
}

func SignalValue(r SignalRow, signal string) int {
	return signalValue(r, signal)
}

func isIncompleteOrLowQuality(r SignalRow) bool {
	if r.Outcome == "errored" || r.Outcome == "abandoned" {
		return true
	}
	if r.HealthGrade == nil {
		return false
	}
	return *r.HealthGrade == "D" || *r.HealthGrade == "F"
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// --- Top Sessions ---

// TopSession holds summary info for a ranked session.
type TopSession struct {
	ID                string  `json:"id"`
	Project           string  `json:"project"`
	FirstMessage      *string `json:"first_message"`
	DisplayName       *string `json:"display_name,omitempty"`
	MessageCount      int     `json:"message_count"`
	OutputTokens      int     `json:"output_tokens"`
	DurationMin       float64 `json:"duration_min"`
	ActiveDurationMin float64 `json:"active_duration_min"`
	// StartedAt and EndedAt are included so the frontend can
	// derive a recency-based status tier — the StatusDot in the
	// Top Sessions column needs the same time window inputs as
	// the sidebar's session list.
	StartedAt         *string `json:"started_at,omitempty"`
	EndedAt           *string `json:"ended_at,omitempty"`
	TerminationStatus *string `json:"termination_status,omitempty"`
}

// TopSessionsResponse wraps the top sessions list.
type TopSessionsResponse struct {
	Metric   string       `json:"metric"`
	Sessions []TopSession `json:"sessions"`
}

// ActiveGapCapSec and ActiveGapCapMs bound how much a single
// inter-message gap can contribute to "active" time: a gap below the cap
// (5 minutes) counts in full -- model generation, tool execution, or a
// quick human turnaround -- while anything longer is treated as idle
// beyond the cap. Defined once and shared by the velocity "active
// minutes" metric and the Top Sessions "active duration" SQL so the two
// definitions cannot drift. timing_test.go asserts the two stay equal.
const (
	ActiveGapCapSec = 300.0
	ActiveGapCapMs  = 300_000
)

// GetAnalyticsTopSessions returns the top 10 sessions by the
// given metric ("messages", "duration", or "output_tokens")
// within the filter.

func rankTopSessions(sessions []TopSession, needsGoSort bool) []TopSession {
	if sessions == nil {
		return []TopSession{}
	}
	if needsGoSort && len(sessions) > 1 {
		sort.SliceStable(sessions, func(i, j int) bool {
			if sessions[i].ActiveDurationMin != sessions[j].ActiveDurationMin {
				return sessions[i].ActiveDurationMin >
					sessions[j].ActiveDurationMin
			}
			return sessions[i].ID < sessions[j].ID
		})
	}
	for i := range sessions {
		sessions[i].DurationMin = round1(sessions[i].DurationMin)
		sessions[i].ActiveDurationMin = round1(sessions[i].ActiveDurationMin)
	}
	return sessions
}
