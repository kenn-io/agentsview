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
	From             string // ISO date YYYY-MM-DD, inclusive
	To               string // ISO date YYYY-MM-DD, inclusive
	Machine          string // optional machine filter
	Project          string // optional project filter
	Agent            string // optional agent filter
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
	// skew them. Fork rows stay excluded regardless because their tokens
	// overlap a root.
	IncludeSubagents bool
}

// RelationshipExclusionSQL returns the relationship_type predicate for
// analytics aggregation. The default excludes subagent and fork rows
// (matching the session list). When IncludeSubagents is set, subagent
// rows are counted while fork rows stay excluded to avoid
// double-counting tokens that overlap their root session. Exported so
// the PostgreSQL and DuckDB analytics builders apply the same rule.
func (f AnalyticsFilter) RelationshipExclusionSQL() string {
	return RelationshipExclusionSQL(f.IncludeSubagents, "")
}

// RelationshipExclusionSQL is the single source of truth for the
// relationship_type analytics predicate, shared by the analytics
// builders (AnalyticsFilter) and the stats pipeline (StatsFilter).
// colPrefix qualifies the column for callers that alias the sessions
// table (e.g. "s."); pass "" for an unqualified column. fork rows are
// always excluded; subagents are excluded unless includeSubagents.
func RelationshipExclusionSQL(includeSubagents bool, colPrefix string) string {
	col := colPrefix + "relationship_type"
	if includeSubagents {
		return col + " NOT IN ('fork')"
	}
	return col + " NOT IN ('subagent', 'fork')"
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
	return f.buildWhereWithDate(dateCol, true)
}

// buildWhereWithoutDate returns common analytics predicates
// without adding session date bounds. Callers that evaluate
// date windows against message timestamps should use this to
// avoid pre-filtering by the parent session timestamp.
func (f AnalyticsFilter) buildWhereWithoutDate() (string, []any) {
	return f.buildWhereWithDate("", false)
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
) (string, []any) {
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

func (f AnalyticsFilter) canUseSQLiteTimeSQL() bool {
	_, ok := f.sqliteTimeModifier()
	return ok
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

func sqliteDateExpr(dateCol string, modifier string) string {
	if modifier == "" {
		return "strftime('%Y-%m-%d', " + dateCol + ")"
	}
	return "strftime('%Y-%m-%d', " + dateCol + ", '" + modifier + "')"
}

func sqliteAnalyticsWhereSQL(
	f AnalyticsFilter,
	dateCol string,
	sessionIDExpr string,
	includeTime bool,
) (string, []any) {
	where, args := f.buildWhere(dateCol)
	modifier, _ := f.sqliteTimeModifier()
	dateExpr := sqliteDateExpr(dateCol, modifier)
	if f.From != "" {
		where += " AND " + dateExpr + " >= ?"
		args = append(args, f.From)
	}
	if f.To != "" {
		where += " AND " + dateExpr + " <= ?"
		args = append(args, f.To)
	}
	if includeTime && f.HasTimeFilter() {
		preds := []string{
			"m.session_id = " + sessionIDExpr,
			"m.timestamp != ''",
		}
		if f.DayOfWeek != nil {
			dowExpr := "strftime('%w', m.timestamp)"
			if modifier != "" {
				dowExpr = "strftime('%w', m.timestamp, '" + modifier + "')"
			}
			preds = append(preds,
				"((CAST("+dowExpr+" AS INTEGER) + 6) % 7) = ?")
			args = append(args, *f.DayOfWeek)
		}
		if f.Hour != nil {
			hourExpr := "strftime('%H', m.timestamp)"
			if modifier != "" {
				hourExpr = "strftime('%H', m.timestamp, '" + modifier + "')"
			}
			preds = append(preds,
				"CAST("+hourExpr+" AS INTEGER) = ?")
			args = append(args, *f.Hour)
		}
		where += " AND EXISTS (SELECT 1 FROM messages m WHERE " +
			strings.Join(preds, " AND ") + ")"
	}
	return where, args
}

// filteredSessionIDs returns the set of session IDs that have
// at least one message matching the hour/dow filter. Used by
// session-level queries to restrict results when time filters
// are active.
func (db *DB) filteredSessionIDs(
	ctx context.Context, f AnalyticsFilter,
) (map[string]bool, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(s.started_at, ''), s.created_at)"
	where, args := f.buildWhere(dateCol)

	query := `SELECT s.id, m.timestamp
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + where + ` AND m.timestamp != ''`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying filtered session IDs: %w", err,
		)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var sid, msgTS string
		if err := rows.Scan(&sid, &msgTS); err != nil {
			return nil, fmt.Errorf(
				"scanning filtered session ID: %w", err,
			)
		}
		if ids[sid] {
			continue // already matched
		}
		t, ok := localTime(msgTS, loc)
		if !ok {
			continue
		}
		if f.matchesTimeFilter(t) {
			ids[sid] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating filtered session IDs: %w", err,
		)
	}
	return ids, nil
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
func (db *DB) GetAnalyticsSummary(
	ctx context.Context, f AnalyticsFilter,
) (AnalyticsSummary, error) {
	// The summary is a token/session aggregate, so subagent sessions
	// (including workflow subagents) are counted here.
	f.IncludeSubagents = true
	if !f.canUseSQLiteTimeSQL() {
		return db.getAnalyticsSummaryGo(ctx, f)
	}
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := sqliteAnalyticsWhereSQL(f, dateCol, "sessions.id", true)
	modifier, _ := f.sqliteTimeModifier()
	dateExpr := sqliteDateExpr(dateCol, modifier)

	query := `
		WITH filtered AS (
			SELECT id, project, agent, message_count,
				total_output_tokens, has_total_output_tokens,
				` + dateExpr + ` AS local_date
			FROM sessions
			WHERE ` + where + `
		),
		ranked AS (
			SELECT message_count,
				ROW_NUMBER() OVER (ORDER BY message_count ASC) AS rn,
				COUNT(*) OVER () AS n
			FROM filtered
		),
		project_totals AS (
			SELECT project, SUM(message_count) AS messages
			FROM filtered
			GROUP BY project
		)
		SELECT
			COUNT(*) AS total_sessions,
			COALESCE(SUM(message_count), 0) AS total_messages,
			COALESCE(SUM(CASE WHEN has_total_output_tokens
				THEN total_output_tokens ELSE 0 END), 0) AS total_output_tokens,
			COALESCE(SUM(CASE WHEN has_total_output_tokens
				THEN 1 ELSE 0 END), 0) AS token_reporting_sessions,
			COUNT(DISTINCT project) AS active_projects,
			COUNT(DISTINCT local_date) AS active_days,
			COALESCE(ROUND(AVG(message_count), 1), 0) AS avg_messages,
			COALESCE((
				SELECT CAST(AVG(message_count) AS INTEGER)
				FROM ranked
				WHERE rn IN (
					CAST(((n + 1) / 2) AS INTEGER),
					CAST(((n + 2) / 2) AS INTEGER)
				)
			), 0) AS median_messages,
			COALESCE((
				SELECT message_count
				FROM ranked
				WHERE rn = MIN(CAST(n * 0.9 AS INTEGER) + 1, n)
				LIMIT 1
			), 0) AS p90_messages,
			COALESCE((
				SELECT project
				FROM project_totals
				ORDER BY messages DESC, project ASC
				LIMIT 1
			), '') AS most_active,
			COALESCE(ROUND((
				SELECT SUM(messages)
				FROM (
					SELECT messages
					FROM project_totals
					ORDER BY messages DESC
					LIMIT 3
				)
			) * 1.0 / NULLIF(SUM(message_count), 0), 3), 0) AS concentration
		FROM filtered`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("querying analytics summary: %w", err)
	}
	s := AnalyticsSummary{Agents: make(map[string]*AgentSummary)}
	if !rows.Next() {
		rows.Close()
		return s, nil
	}
	if err := rows.Scan(
		&s.TotalSessions,
		&s.TotalMessages,
		&s.TotalOutputTokens,
		&s.TokenReportingSessions,
		&s.ActiveProjects,
		&s.ActiveDays,
		&s.AvgMessages,
		&s.MedianMessages,
		&s.P90Messages,
		&s.MostActive,
		&s.Concentration,
	); err != nil {
		rows.Close()
		return AnalyticsSummary{},
			fmt.Errorf("scanning summary row: %w", err)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return AnalyticsSummary{},
			fmt.Errorf("iterating summary rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("closing summary rows: %w", err)
	}

	agentRows, err := db.getReader().QueryContext(ctx, `
		WITH filtered AS (
			SELECT agent, message_count
			FROM sessions
			WHERE `+where+`
		)
		SELECT agent, COUNT(*), COALESCE(SUM(message_count), 0)
		FROM filtered
		GROUP BY agent`,
		args...,
	)
	if err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("querying analytics summary agents: %w", err)
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var agent string
		var summary AgentSummary
		if err := agentRows.Scan(
			&agent, &summary.Sessions, &summary.Messages,
		); err != nil {
			return AnalyticsSummary{},
				fmt.Errorf("scanning summary agent: %w", err)
		}
		s.Agents[agent] = &summary
	}
	if err := agentRows.Err(); err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("iterating summary agents: %w", err)
	}
	return s, nil
}

func (db *DB) getAnalyticsSummaryGo(
	ctx context.Context, f AnalyticsFilter,
) (AnalyticsSummary, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return AnalyticsSummary{}, err
		}
	}

	// Fetch sessions with their message counts and agents
	query := `SELECT id, ` + dateCol +
		`, message_count, agent, project,
		total_output_tokens, has_total_output_tokens
		FROM sessions WHERE ` + where +
		` ORDER BY message_count ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("querying analytics summary: %w", err)
	}
	defer rows.Close()

	type sessionRow struct {
		date         string
		messages     int
		agent        string
		project      string
		outputTokens int
		hasTokens    bool
	}

	var all []sessionRow
	for rows.Next() {
		var id, ts string
		var mc int
		var agent, project string
		var outputTokens int
		var hasTokens bool
		if err := rows.Scan(
			&id, &ts, &mc, &agent, &project,
			&outputTokens, &hasTokens,
		); err != nil {
			return AnalyticsSummary{},
				fmt.Errorf("scanning summary row: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		all = append(all, sessionRow{
			date:         date,
			messages:     mc,
			agent:        agent,
			project:      project,
			outputTokens: outputTokens,
			hasTokens:    hasTokens,
		})
	}
	if err := rows.Err(); err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("iterating summary rows: %w", err)
	}

	var s AnalyticsSummary
	s.Agents = make(map[string]*AgentSummary)

	if len(all) == 0 {
		return s, nil
	}

	days := make(map[string]bool)
	projects := make(map[string]int) // project -> message count
	msgCounts := make([]int, 0, len(all))

	for _, r := range all {
		s.TotalSessions++
		s.TotalMessages += r.messages
		if r.hasTokens {
			s.TotalOutputTokens += r.outputTokens
			s.TokenReportingSessions++
		}
		days[r.date] = true
		projects[r.project] += r.messages
		msgCounts = append(msgCounts, r.messages)

		if s.Agents[r.agent] == nil {
			s.Agents[r.agent] = &AgentSummary{}
		}
		s.Agents[r.agent].Sessions++
		s.Agents[r.agent].Messages += r.messages
	}

	s.ActiveProjects = len(projects)
	s.ActiveDays = len(days)
	s.AvgMessages = math.Round(
		float64(s.TotalMessages)/float64(s.TotalSessions)*10,
	) / 10

	sort.Ints(msgCounts)
	n := len(msgCounts)
	if n%2 == 0 {
		s.MedianMessages = (msgCounts[n/2-1] + msgCounts[n/2]) / 2
	} else {
		s.MedianMessages = msgCounts[n/2]
	}
	p90Idx := int(float64(n) * 0.9)
	if p90Idx >= n {
		p90Idx = n - 1
	}
	s.P90Messages = msgCounts[p90Idx]

	// Most active project by message count (deterministic tie-break)
	maxMsgs := 0
	for name, count := range projects {
		if count > maxMsgs || (count == maxMsgs && name < s.MostActive) {
			maxMsgs = count
			s.MostActive = name
		}
	}

	// Concentration: fraction of messages in top 3 projects
	if s.TotalMessages > 0 {
		counts := make([]int, 0, len(projects))
		for _, c := range projects {
			counts = append(counts, c)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(counts)))
		top := min(3, len(counts))
		topSum := 0
		for _, c := range counts[:top] {
			topSum += c
		}
		s.Concentration = math.Round(
			float64(topSum)/float64(s.TotalMessages)*1000,
		) / 1000
	}

	return s, nil
}

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
func (db *DB) GetAnalyticsActivity(
	ctx context.Context, f AnalyticsFilter,
	granularity string,
) (ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	loc := f.location()
	dateCol := "COALESCE(NULLIF(s.started_at, ''), s.created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return ActivityResponse{}, err
		}
	}

	query := `SELECT ` + dateCol + `, s.agent, s.id,
		m.role, m.has_thinking, m.is_system, COUNT(*)
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		WHERE ` + where + `
		GROUP BY s.id, m.role, m.has_thinking, m.is_system`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return ActivityResponse{},
			fmt.Errorf("querying analytics activity: %w", err)
	}
	defer rows.Close()

	buckets := make(map[string]*ActivityEntry)
	sessionSeen := make(map[string]string) // session_id -> bucket
	var sessionIDs []string

	for rows.Next() {
		var ts, agent, sid string
		var role *string
		var hasThinking, isSystem *bool
		var count int
		if err := rows.Scan(
			&ts, &agent, &sid, &role,
			&hasThinking, &isSystem, &count,
		); err != nil {
			return ActivityResponse{},
				fmt.Errorf("scanning activity row: %w", err)
		}

		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[sid] {
			continue
		}
		bucket := bucketDate(date, granularity)

		entry, ok := buckets[bucket]
		if !ok {
			entry = &ActivityEntry{
				Date:    bucket,
				ByAgent: make(map[string]int),
			}
			buckets[bucket] = entry
		}

		// Count this session once per bucket
		if _, seen := sessionSeen[sid]; !seen {
			sessionSeen[sid] = bucket
			sessionIDs = append(sessionIDs, sid)
			entry.Sessions++
		}

		sys := isSystem != nil && *isSystem
		if role != nil {
			entry.Messages += count
			entry.ByAgent[agent] += count
			switch *role {
			case "user":
				if !sys {
					entry.UserMessages += count
				}
			case "assistant":
				entry.AssistantMessages += count
			}
			if hasThinking != nil && *hasThinking {
				entry.ThinkingMessages += count
			}
		}
	}
	if err := rows.Err(); err != nil {
		return ActivityResponse{},
			fmt.Errorf("iterating activity rows: %w", err)
	}

	// Merge tool_call counts per session into buckets.
	if len(sessionIDs) > 0 {
		err = queryChunked(sessionIDs,
			func(chunk []string) error {
				return db.mergeActivityToolCalls(
					ctx, chunk, sessionSeen, buckets,
				)
			})
		if err != nil {
			return ActivityResponse{}, err
		}
	}

	// Sort by date
	series := make([]ActivityEntry, 0, len(buckets))
	for _, e := range buckets {
		series = append(series, *e)
	}
	sort.Slice(series, func(i, j int) bool {
		return series[i].Date < series[j].Date
	})

	return ActivityResponse{
		Granularity: granularity,
		Series:      series,
	}, nil
}

// mergeActivityToolCalls queries tool_calls for a chunk of
// session IDs and adds counts to the matching activity buckets.
func (db *DB) mergeActivityToolCalls(
	ctx context.Context,
	chunk []string,
	sessionBucket map[string]string,
	buckets map[string]*ActivityEntry,
) error {
	ph, args := inPlaceholders(chunk)
	q := `SELECT session_id, COUNT(*)
		FROM tool_calls
		WHERE session_id IN ` + ph + `
		GROUP BY session_id`
	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf(
			"querying activity tool_calls: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var count int
		if err := rows.Scan(&sid, &count); err != nil {
			return fmt.Errorf(
				"scanning activity tool_call: %w", err,
			)
		}
		bucket := sessionBucket[sid]
		if entry, ok := buckets[bucket]; ok {
			entry.ToolCalls += count
		}
	}
	return rows.Err()
}

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
func (db *DB) GetAnalyticsHeatmap(
	ctx context.Context, f AnalyticsFilter,
	metric string,
) (HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
	}

	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return HeatmapResponse{}, err
		}
	}

	query := `SELECT id, ` + dateCol +
		`, message_count, total_output_tokens,
		has_total_output_tokens
		FROM sessions WHERE ` + where

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return HeatmapResponse{},
			fmt.Errorf("querying analytics heatmap: %w", err)
	}
	defer rows.Close()

	dayCounts := make(map[string]int) // date -> count
	daySessions := make(map[string]int)
	dayOutputTokens := make(map[string]int)

	for rows.Next() {
		var id, ts string
		var mc, outputTokens int
		var hasTokens bool
		if err := rows.Scan(
			&id, &ts, &mc, &outputTokens, &hasTokens,
		); err != nil {
			return HeatmapResponse{},
				fmt.Errorf("scanning heatmap row: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		dayCounts[date] += mc
		daySessions[date]++
		if hasTokens {
			dayOutputTokens[date] += outputTokens
		}
	}
	if err := rows.Err(); err != nil {
		return HeatmapResponse{},
			fmt.Errorf("iterating heatmap rows: %w", err)
	}

	// Choose which map to use based on metric
	source := dayCounts
	switch metric {
	case "sessions":
		source = daySessions
	case "output_tokens":
		source = dayOutputTokens
	}

	// For output_tokens, an empty source means no sessions
	// reported token coverage. Return an empty heatmap so the
	// UI can show "no data" instead of a misleading zero grid.
	if metric == "output_tokens" && len(source) == 0 {
		return HeatmapResponse{
			Metric:      metric,
			EntriesFrom: clampFrom(f.From, f.To),
		}, nil
	}

	// Determine effective date range (clamped to MaxHeatmapDays)
	entriesFrom := clampFrom(f.From, f.To)

	// Collect non-zero values from the displayed range only,
	// so outliers outside the window don't skew intensity.
	var values []int
	for date, v := range source {
		if v > 0 && date >= entriesFrom && date <= f.To {
			values = append(values, v)
		}
	}
	sort.Ints(values)

	levels := computeQuartileLevels(values)

	// Build entries for each day in the clamped range
	entries := buildDateEntries(
		entriesFrom, f.To, source, levels,
	)

	return HeatmapResponse{
		Metric:      metric,
		Entries:     entries,
		Levels:      levels,
		EntriesFrom: entriesFrom,
	}, nil
}

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
func (db *DB) GetAnalyticsProjects(
	ctx context.Context, f AnalyticsFilter,
) (ProjectsAnalyticsResponse, error) {
	// Per-project session/token breakdown is an aggregate, so subagent
	// sessions (including workflow subagents) are counted here.
	f.IncludeSubagents = true
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return ProjectsAnalyticsResponse{}, err
		}
	}

	query := `SELECT id, project, ` + dateCol + `,
		message_count, agent
		FROM sessions WHERE ` + where +
		` ORDER BY project, ` + dateCol

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return ProjectsAnalyticsResponse{},
			fmt.Errorf("querying analytics projects: %w", err)
	}
	defer rows.Close()

	type projectData struct {
		name     string
		sessions int
		messages int
		first    string
		last     string
		counts   []int
		agents   map[string]int
		days     map[string]int
	}

	projectMap := make(map[string]*projectData)
	var projectOrder []string

	for rows.Next() {
		var id, project, ts, agent string
		var mc int
		if err := rows.Scan(
			&id, &project, &ts, &mc, &agent,
		); err != nil {
			return ProjectsAnalyticsResponse{},
				fmt.Errorf("scanning project row: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}

		pd, ok := projectMap[project]
		if !ok {
			pd = &projectData{
				name:   project,
				agents: make(map[string]int),
				days:   make(map[string]int),
			}
			projectMap[project] = pd
			projectOrder = append(projectOrder, project)
		}

		pd.sessions++
		pd.messages += mc
		pd.counts = append(pd.counts, mc)
		pd.agents[agent]++
		pd.days[date] += mc

		if pd.first == "" || date < pd.first {
			pd.first = date
		}
		if date > pd.last {
			pd.last = date
		}
	}
	if err := rows.Err(); err != nil {
		return ProjectsAnalyticsResponse{},
			fmt.Errorf("iterating project rows: %w", err)
	}

	projects := make([]ProjectAnalytics, 0, len(projectMap))
	for _, name := range projectOrder {
		pd, ok := projectMap[name]
		if !ok || pd == nil {
			continue
		}
		sort.Ints(pd.counts)
		n := len(pd.counts)

		avg := 0.0
		if n > 0 {
			avg = math.Round(
				float64(pd.messages)/float64(n)*10,
			) / 10
		}

		// Daily trend: messages per active day
		trend := 0.0
		if len(pd.days) > 0 {
			trend = math.Round(
				float64(pd.messages)/float64(len(pd.days))*10,
			) / 10
		}

		projects = append(projects, ProjectAnalytics{
			Name:           pd.name,
			Sessions:       pd.sessions,
			Messages:       pd.messages,
			FirstSession:   pd.first,
			LastSession:    pd.last,
			AvgMessages:    avg,
			MedianMessages: medianInt(pd.counts, n),
			Agents:         pd.agents,
			DailyTrend:     trend,
		})
	}

	// Sort by message count descending
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Messages > projects[j].Messages
	})

	return ProjectsAnalyticsResponse{Projects: projects}, nil
}

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
func (db *DB) GetAnalyticsHourOfWeek(
	ctx context.Context, f AnalyticsFilter,
) (HourOfWeekResponse, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(s.started_at, ''), s.created_at)"
	where, args := f.buildWhere(dateCol)

	query := `SELECT ` + dateCol + `, m.timestamp
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + where + ` AND m.timestamp != ''`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return HourOfWeekResponse{},
			fmt.Errorf("querying hour-of-week: %w", err)
	}
	defer rows.Close()

	var grid [7][24]int

	for rows.Next() {
		var sessTS, msgTS string
		if err := rows.Scan(&sessTS, &msgTS); err != nil {
			return HourOfWeekResponse{},
				fmt.Errorf("scanning hour-of-week row: %w", err)
		}
		sessDate := localDate(sessTS, loc)
		if !inDateRange(sessDate, f.From, f.To) {
			continue
		}
		t, ok := localTime(msgTS, loc)
		if !ok {
			continue
		}
		// Go Sunday=0, convert to ISO Monday=0
		dow := (int(t.Weekday()) + 6) % 7
		grid[dow][t.Hour()]++
	}
	if err := rows.Err(); err != nil {
		return HourOfWeekResponse{},
			fmt.Errorf("iterating hour-of-week rows: %w", err)
	}

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

	return HourOfWeekResponse{Cells: cells}, nil
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
func (db *DB) GetAnalyticsSessionShape(
	ctx context.Context, f AnalyticsFilter,
) (SessionShapeResponse, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return SessionShapeResponse{}, err
		}
	}

	query := `SELECT ` + dateCol + `, started_at, ended_at,
		message_count, id FROM sessions WHERE ` + where

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return SessionShapeResponse{},
			fmt.Errorf("querying session shape: %w", err)
	}
	defer rows.Close()

	lengthCounts := make(map[string]int)
	durationCounts := make(map[string]int)
	var sessionIDs []string
	totalCount := 0

	for rows.Next() {
		var ts string
		var startedAt, endedAt *string
		var mc int
		var id string
		if err := rows.Scan(
			&ts, &startedAt, &endedAt, &mc, &id,
		); err != nil {
			return SessionShapeResponse{},
				fmt.Errorf("scanning session shape row: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}

		totalCount++
		lengthCounts[lengthBucket(mc)]++
		sessionIDs = append(sessionIDs, id)

		if startedAt != nil && endedAt != nil &&
			*startedAt != "" && *endedAt != "" {
			tStart, okS := localTime(*startedAt, loc)
			tEnd, okE := localTime(*endedAt, loc)
			if okS && okE {
				mins := tEnd.Sub(tStart).Minutes()
				if mins >= 0 {
					durationCounts[durationBucket(mins)]++
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return SessionShapeResponse{},
			fmt.Errorf("iterating session shape rows: %w", err)
	}

	// Query autonomy data for filtered sessions
	autonomyCounts := make(map[string]int)
	if len(sessionIDs) > 0 {
		err := queryChunked(sessionIDs,
			func(chunk []string) error {
				return db.queryAutonomyChunk(
					ctx, chunk, autonomyCounts,
				)
			})
		if err != nil {
			return SessionShapeResponse{}, err
		}
	}

	return SessionShapeResponse{
		Count:                totalCount,
		LengthDistribution:   mapToBuckets(lengthCounts, lengthOrder),
		DurationDistribution: mapToBuckets(durationCounts, durationOrder),
		AutonomyDistribution: mapToBuckets(autonomyCounts, autonomyOrder),
	}, nil
}

// queryAutonomyChunk queries autonomy stats for a chunk of
// session IDs and accumulates results into counts.
func (db *DB) queryAutonomyChunk(
	ctx context.Context,
	chunk []string,
	counts map[string]int,
) error {
	ph, args := inPlaceholders(chunk)
	q := `SELECT session_id,
		SUM(CASE WHEN role='user' AND is_system=0
			THEN 1 ELSE 0 END),
		SUM(CASE WHEN role='assistant'
			AND has_tool_use=1 THEN 1 ELSE 0 END)
		FROM messages
		WHERE session_id IN ` + ph + `
		GROUP BY session_id`

	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying autonomy: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var userCount, toolCount int
		if err := rows.Scan(
			&sid, &userCount, &toolCount,
		); err != nil {
			return fmt.Errorf("scanning autonomy row: %w", err)
		}
		if userCount > 0 {
			ratio := float64(toolCount) / float64(userCount)
			counts[autonomyBucket(ratio)]++
		}
	}
	return rows.Err()
}

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
	Trend      []ToolTrendEntry     `json:"trend"`
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

// BuildSkillsAnalytics folds backend-neutral skill rows into the public
// response shape. Skill names are trimmed and empty names are ignored.
func BuildSkillsAnalytics(rows []SkillAnalyticsRow) SkillsAnalyticsResponse {
	resp := SkillsAnalyticsResponse{
		BySkill: []SkillUsage{},
		Trend:   []SkillTrendEntry{},
	}
	if len(rows) == 0 {
		return resp
	}

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
			week := bucketDate(row.Date, "week")
			if trendBuckets[week] == nil {
				trendBuckets[week] = map[string]int{}
			}
			trendBuckets[week][name] += row.Count
		}
	}

	resp.DistinctSkills = len(bySkill)
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

	for week, skills := range trendBuckets {
		resp.Trend = append(resp.Trend, SkillTrendEntry{
			Date: week, BySkill: skills,
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

func analyticsToolsQuery(placeholders string) string {
	return `SELECT session_id, category, COUNT(*)
		FROM tool_calls
		WHERE session_id IN ` + placeholders + `
		GROUP BY session_id, category`
}

func analyticsSkillsQuery(placeholders string) string {
	return `SELECT tc.session_id, TRIM(tc.skill_name), COUNT(*),
			COALESCE(m.timestamp, '')
		FROM tool_calls tc
		LEFT JOIN messages m
			ON m.session_id = tc.session_id AND m.id = tc.message_id
		WHERE tc.session_id IN ` + placeholders + `
			AND TRIM(COALESCE(tc.skill_name, '')) != ''
		GROUP BY tc.session_id, TRIM(tc.skill_name),
			COALESCE(m.timestamp, '')`
}

// GetAnalyticsTools returns tool usage analytics aggregated
// from the tool_calls table.
func (db *DB) GetAnalyticsTools(
	ctx context.Context, f AnalyticsFilter,
) (ToolsAnalyticsResponse, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return ToolsAnalyticsResponse{}, err
		}
	}

	// Fetch filtered session IDs and their metadata.
	sessQ := `SELECT id, ` + dateCol + `, agent
		FROM sessions WHERE ` + where

	sessRows, err := db.getReader().QueryContext(ctx, sessQ, args...)
	if err != nil {
		return ToolsAnalyticsResponse{},
			fmt.Errorf("querying tool sessions: %w", err)
	}
	defer sessRows.Close()

	type sessInfo struct {
		date  string
		agent string
	}
	sessionMap := make(map[string]sessInfo)
	var sessionIDs []string

	for sessRows.Next() {
		var id, ts, agent string
		if err := sessRows.Scan(&id, &ts, &agent); err != nil {
			return ToolsAnalyticsResponse{},
				fmt.Errorf("scanning tool session: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		sessionMap[id] = sessInfo{date: date, agent: agent}
		sessionIDs = append(sessionIDs, id)
	}
	if err := sessRows.Err(); err != nil {
		return ToolsAnalyticsResponse{},
			fmt.Errorf("iterating tool sessions: %w", err)
	}

	resp := ToolsAnalyticsResponse{
		ByCategory: []ToolCategoryCount{},
		ByAgent:    []ToolAgentBreakdown{},
		Trend:      []ToolTrendEntry{},
	}

	if len(sessionIDs) == 0 {
		return resp, nil
	}

	// Query tool_calls for filtered sessions (chunked).
	type toolRow struct {
		sessionID string
		category  string
		count     int
	}
	var toolRows []toolRow

	err = queryChunked(sessionIDs,
		func(chunk []string) error {
			ph, chunkArgs := inPlaceholders(chunk)
			q := analyticsToolsQuery(ph)
			rows, qErr := db.getReader().QueryContext(
				ctx, q, chunkArgs...,
			)
			if qErr != nil {
				return fmt.Errorf(
					"querying tool_calls: %w", qErr,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var sid, cat string
				var count int
				if err := rows.Scan(
					&sid, &cat, &count,
				); err != nil {
					return fmt.Errorf(
						"scanning tool_call: %w", err,
					)
				}
				toolRows = append(toolRows, toolRow{
					sessionID: sid,
					category:  cat,
					count:     count,
				})
			}
			return rows.Err()
		})
	if err != nil {
		return ToolsAnalyticsResponse{}, err
	}

	if len(toolRows) == 0 {
		return resp, nil
	}

	// Aggregate in Go.
	catCounts := make(map[string]int)
	agentCats := make(map[string]map[string]int)    // agent → cat → count
	trendBuckets := make(map[string]map[string]int) // week → cat → count

	for _, tr := range toolRows {
		info := sessionMap[tr.sessionID]
		catCounts[tr.category] += tr.count

		if agentCats[info.agent] == nil {
			agentCats[info.agent] = make(map[string]int)
		}
		agentCats[info.agent][tr.category] += tr.count

		week := bucketDate(info.date, "week")
		if trendBuckets[week] == nil {
			trendBuckets[week] = make(map[string]int)
		}
		trendBuckets[week][tr.category] += tr.count
	}

	for _, count := range catCounts {
		resp.TotalCalls += count
	}

	// Build ByCategory sorted by count desc.
	resp.ByCategory = make(
		[]ToolCategoryCount, 0, len(catCounts),
	)
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

	// Build ByAgent sorted alphabetically.
	agentKeys := make([]string, 0, len(agentCats))
	for k := range agentCats {
		agentKeys = append(agentKeys, k)
	}
	sort.Strings(agentKeys)
	resp.ByAgent = make(
		[]ToolAgentBreakdown, 0, len(agentKeys),
	)
	for _, agent := range agentKeys {
		cats := agentCats[agent]
		total := 0
		for _, c := range cats {
			total += c
		}
		catList := make(
			[]ToolCategoryCount, 0, len(cats),
		)
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

	// Build Trend sorted by date.
	resp.Trend = make(
		[]ToolTrendEntry, 0, len(trendBuckets),
	)
	for week, cats := range trendBuckets {
		resp.Trend = append(resp.Trend, ToolTrendEntry{
			Date: week, ByCat: cats,
		})
	}
	sort.Slice(resp.Trend, func(i, j int) bool {
		return resp.Trend[i].Date < resp.Trend[j].Date
	})

	return resp, nil
}

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
// from non-empty tool_calls.skill_name values.
func (db *DB) GetAnalyticsSkills(
	ctx context.Context, f AnalyticsFilter,
) (SkillsAnalyticsResponse, error) {
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhereWithoutDate()

	sessQ := `SELECT id, ` + dateCol + `, agent, project
		FROM sessions WHERE ` + where

	sessRows, err := db.getReader().QueryContext(ctx, sessQ, args...)
	if err != nil {
		return SkillsAnalyticsResponse{},
			fmt.Errorf("querying skill sessions: %w", err)
	}
	defer sessRows.Close()

	type sessInfo struct {
		ts      string
		agent   string
		project string
	}
	sessionMap := make(map[string]sessInfo)
	var sessionIDs []string

	for sessRows.Next() {
		var id, ts, agent, project string
		if err := sessRows.Scan(
			&id, &ts, &agent, &project,
		); err != nil {
			return SkillsAnalyticsResponse{},
				fmt.Errorf("scanning skill session: %w", err)
		}
		sessionMap[id] = sessInfo{
			ts:      ts,
			agent:   agent,
			project: project,
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := sessRows.Err(); err != nil {
		return SkillsAnalyticsResponse{},
			fmt.Errorf("iterating skill sessions: %w", err)
	}
	if len(sessionIDs) == 0 {
		return BuildSkillsAnalytics(nil), nil
	}

	var skillRows []SkillAnalyticsRow
	err = queryChunked(sessionIDs,
		func(chunk []string) error {
			ph, chunkArgs := inPlaceholders(chunk)
			q := analyticsSkillsQuery(ph)
			rows, qErr := db.getReader().QueryContext(
				ctx, q, chunkArgs...,
			)
			if qErr != nil {
				return fmt.Errorf(
					"querying skill tool_calls: %w", qErr,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var sid, skill, lastTS string
				var count int
				if err := rows.Scan(
					&sid, &skill, &count, &lastTS,
				); err != nil {
					return fmt.Errorf(
						"scanning skill tool_call: %w", err,
					)
				}
				info := sessionMap[sid]
				usedTS, date, keep := f.ResolveSkillRowTime(
					lastTS, info.ts,
				)
				if !keep {
					continue
				}
				skillRows = append(skillRows, SkillAnalyticsRow{
					SessionID:  sid,
					SkillName:  skill,
					Agent:      info.agent,
					Project:    info.project,
					Date:       date,
					LastUsedAt: usedTS,
					Count:      count,
				})
			}
			return rows.Err()
		})
	if err != nil {
		return SkillsAnalyticsResponse{}, err
	}

	return BuildSkillsAnalytics(skillRows), nil
}

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
	const maxGapSec = 300.0

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
func (db *DB) GetAnalyticsVelocity(
	ctx context.Context, f AnalyticsFilter,
) (VelocityResponse, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return VelocityResponse{}, err
		}
	}

	// Phase 1: Get filtered session metadata
	sessQuery := `SELECT id, ` + dateCol + `, agent,
		message_count FROM sessions WHERE ` + where

	sessRows, err := db.getReader().QueryContext(
		ctx, sessQuery, args...,
	)
	if err != nil {
		return VelocityResponse{},
			fmt.Errorf("querying velocity sessions: %w", err)
	}
	defer sessRows.Close()

	type sessInfo struct {
		agent string
		mc    int
	}
	sessionMap := make(map[string]sessInfo)
	var sessionIDs []string

	for sessRows.Next() {
		var id, ts, agent string
		var mc int
		if err := sessRows.Scan(
			&id, &ts, &agent, &mc,
		); err != nil {
			return VelocityResponse{},
				fmt.Errorf("scanning velocity session: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		sessionMap[id] = sessInfo{agent: agent, mc: mc}
		sessionIDs = append(sessionIDs, id)
	}
	if err := sessRows.Err(); err != nil {
		return VelocityResponse{},
			fmt.Errorf("iterating velocity sessions: %w", err)
	}

	if len(sessionIDs) == 0 {
		return VelocityResponse{
			ByAgent:      []VelocityBreakdown{},
			ByComplexity: []VelocityBreakdown{},
		}, nil
	}

	// Phase 2: Fetch messages for filtered sessions (chunked)
	sessionMsgs := make(map[string][]velocityMsg)
	err = queryChunked(sessionIDs,
		func(chunk []string) error {
			return db.queryVelocityMsgs(
				ctx, chunk, loc, sessionMsgs,
			)
		})
	if err != nil {
		return VelocityResponse{}, err
	}

	// Phase 2b: Fetch tool call counts per session (chunked)
	toolCountMap := make(map[string]int)
	err = queryChunked(sessionIDs,
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
					"querying velocity tool_calls: %w",
					qErr,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var sid string
				var count int
				if err := rows.Scan(&sid, &count); err != nil {
					return fmt.Errorf(
						"scanning velocity tool_call: %w",
						err,
					)
				}
				toolCountMap[sid] = count
			}
			return rows.Err()
		})
	if err != nil {
		return VelocityResponse{}, err
	}

	// Process per-session metrics
	overall := &velocityAccumulator{}
	byAgent := make(map[string]*velocityAccumulator)
	byComplexity := make(map[string]*velocityAccumulator)

	for _, sid := range sessionIDs {
		info := sessionMap[sid]
		msgs := sessionMsgs[sid]
		if len(msgs) < 2 {
			continue
		}

		agentKey := info.agent
		compKey := complexityBucket(info.mc)

		if byAgent[agentKey] == nil {
			byAgent[agentKey] = &velocityAccumulator{}
		}
		if byComplexity[compKey] == nil {
			byComplexity[compKey] = &velocityAccumulator{}
		}

		processSessionVelocity(
			[]*velocityAccumulator{
				overall, byAgent[agentKey], byComplexity[compKey],
			},
			msgs, toolCountMap[sid],
		)
	}

	resp := VelocityResponse{
		Overall: overall.computeOverview(),
	}

	// Build by-agent breakdowns
	agentKeys := make([]string, 0, len(byAgent))
	for k := range byAgent {
		agentKeys = append(agentKeys, k)
	}
	sort.Strings(agentKeys)
	resp.ByAgent = make([]VelocityBreakdown, 0, len(agentKeys))
	for _, k := range agentKeys {
		a, ok := byAgent[k]
		if !ok || a == nil {
			continue
		}
		resp.ByAgent = append(resp.ByAgent, VelocityBreakdown{
			Label:    k,
			Sessions: a.sessions,
			Overview: a.computeOverview(),
		})
	}

	// Build by-complexity breakdowns
	compOrder := map[string]int{
		"1-15": 0, "16-60": 1, "61+": 2,
	}
	compKeys := make([]string, 0, len(byComplexity))
	for k := range byComplexity {
		compKeys = append(compKeys, k)
	}
	sort.Slice(compKeys, func(i, j int) bool {
		return compOrder[compKeys[i]] < compOrder[compKeys[j]]
	})
	resp.ByComplexity = make(
		[]VelocityBreakdown, 0, len(compKeys),
	)
	for _, k := range compKeys {
		a, ok := byComplexity[k]
		if !ok || a == nil {
			continue
		}
		resp.ByComplexity = append(resp.ByComplexity,
			VelocityBreakdown{
				Label:    k,
				Sessions: a.sessions,
				Overview: a.computeOverview(),
			})
	}

	return resp, nil
}

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
func (db *DB) GetAnalyticsSignals(
	ctx context.Context, f AnalyticsFilter,
) (SignalsAnalyticsResponse, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return SignalsAnalyticsResponse{}, err
		}
	}

	query := `SELECT id, agent, project, first_message, is_automated,
		` + dateCol + `,
		health_score, health_grade, outcome,
		outcome_confidence,
		tool_failure_signal_count, tool_retry_count,
		edit_churn_count, compaction_count,
		mid_task_compaction_count,
		context_pressure_max,
		quality_signal_version,
		short_prompt_count, unstructured_start,
		missing_success_criteria_count,
		missing_verification_count, duplicate_prompt_count,
		no_code_context_count, runaway_tool_loop_count
		FROM sessions WHERE ` + where

	rows, err := db.getReader().QueryContext(
		ctx, query, args...,
	)
	if err != nil {
		return SignalsAnalyticsResponse{},
			fmt.Errorf(
				"querying analytics signals: %w", err,
			)
	}
	defer rows.Close()

	var all []SignalRow
	for rows.Next() {
		var r SignalRow
		var ts string
		if err := rows.Scan(
			&r.ID, &r.Agent, &r.Project,
			&r.FirstMessage, &r.IsAutomated, &ts,
			&r.HealthScore, &r.HealthGrade,
			&r.Outcome, &r.OutcomeConfidence,
			&r.ToolFailureSignalCount,
			&r.ToolRetryCount, &r.EditChurnCount,
			&r.CompactionCount, &r.MidTaskCompactionCount,
			&r.ContextPressureMax,
			&r.QualitySignalVersion,
			&r.ShortPromptCount, &r.UnstructuredStart,
			&r.MissingSuccessCriteriaCount,
			&r.MissingVerificationCount,
			&r.DuplicatePromptCount,
			&r.NoCodeContextCount, &r.RunawayToolLoopCount,
		); err != nil {
			return SignalsAnalyticsResponse{},
				fmt.Errorf(
					"scanning signals row: %w", err,
				)
		}
		r.Date = localDate(ts, loc)
		if !inDateRange(r.Date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[r.ID] {
			continue
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return SignalsAnalyticsResponse{},
			fmt.Errorf(
				"iterating signals rows: %w", err,
			)
	}
	if err := db.populateFrustrationMarkers(ctx, all); err != nil {
		return SignalsAnalyticsResponse{}, err
	}

	return AggregateSignals(all), nil
}

// GetAnalyticsSignalSessions returns concrete examples for a
// signal within the current analytics filter.
func (db *DB) GetAnalyticsSignalSessions(
	ctx context.Context,
	f AnalyticsFilter,
	signal string,
	limit int,
) (SignalSessionsResponse, error) {
	if !IsSupportedAnalyticsSignal(signal) {
		return SignalSessionsResponse{}, ErrUnsupportedAnalyticsSignal
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	rows, err := db.signalRows(ctx, f)
	if err != nil {
		return SignalSessionsResponse{}, err
	}
	if err := db.populateFrustrationMarkers(ctx, rows); err != nil {
		return SignalSessionsResponse{}, err
	}
	candidates := SignalCandidates(rows, signal, limit)
	messages, err := db.signalMessages(ctx, candidates)
	if err != nil {
		return SignalSessionsResponse{}, err
	}
	return SignalSessionsResponse{
		Signal:   signal,
		Sessions: BuildSignalExamples(candidates, messages, signal),
	}, nil
}

func (db *DB) signalRows(
	ctx context.Context,
	f AnalyticsFilter,
) ([]SignalRow, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return nil, err
		}
	}

	query := `SELECT id, agent, project, first_message, is_automated,
		` + dateCol + `,
		health_score, health_grade, outcome,
		outcome_confidence,
		tool_failure_signal_count, tool_retry_count,
		edit_churn_count, compaction_count,
		mid_task_compaction_count,
		context_pressure_max,
		quality_signal_version,
		short_prompt_count, unstructured_start,
		missing_success_criteria_count,
		missing_verification_count, duplicate_prompt_count,
		no_code_context_count, runaway_tool_loop_count
		FROM sessions WHERE ` + where

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying analytics signal rows: %w", err,
		)
	}
	defer rows.Close()
	var all []SignalRow
	for rows.Next() {
		r, err := scanSignalRow(rows, loc)
		if err != nil {
			return nil, err
		}
		if !inDateRange(r.Date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[r.ID] {
			continue
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating analytics signal rows: %w", err,
		)
	}
	return all, nil
}

func (db *DB) populateFrustrationMarkers(
	ctx context.Context,
	rows []SignalRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	idx := make(map[string]int, len(rows))
	ids := make([]string, 0, len(rows))
	for i := range rows {
		idx[rows[i].ID] = i
		ids = append(ids, rows[i].ID)
	}
	return queryChunked(ids, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		q := `SELECT session_id, ordinal, content, is_system
			FROM messages
			WHERE role = 'user' AND session_id IN ` + ph
		msgRows, err := db.getReader().QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf(
				"querying frustration markers: %w", err,
			)
		}
		defer msgRows.Close()
		for msgRows.Next() {
			var sessionID, content string
			var ordinal int
			var isSystem bool
			if err := msgRows.Scan(
				&sessionID, &ordinal, &content, &isSystem,
			); err != nil {
				return fmt.Errorf(
					"scanning frustration marker: %w", err,
				)
			}
			i, ok := idx[sessionID]
			if !ok || isSystem {
				continue
			}
			if signals.IsFrustrationMarker(content) {
				rows[i].FrustrationMarkerCount++
			}
		}
		if err := msgRows.Err(); err != nil {
			return fmt.Errorf(
				"iterating frustration markers: %w", err,
			)
		}
		return nil
	})
}

func scanSignalRow(rs rowScanner, loc *time.Location) (SignalRow, error) {
	var r SignalRow
	var ts string
	if err := rs.Scan(
		&r.ID, &r.Agent, &r.Project,
		&r.FirstMessage, &r.IsAutomated, &ts,
		&r.HealthScore, &r.HealthGrade,
		&r.Outcome, &r.OutcomeConfidence,
		&r.ToolFailureSignalCount,
		&r.ToolRetryCount, &r.EditChurnCount,
		&r.CompactionCount, &r.MidTaskCompactionCount,
		&r.ContextPressureMax,
		&r.QualitySignalVersion,
		&r.ShortPromptCount, &r.UnstructuredStart,
		&r.MissingSuccessCriteriaCount,
		&r.MissingVerificationCount,
		&r.DuplicatePromptCount,
		&r.NoCodeContextCount, &r.RunawayToolLoopCount,
	); err != nil {
		return SignalRow{}, fmt.Errorf(
			"scanning signal row: %w", err,
		)
	}
	r.Date = localDate(ts, loc)
	return r, nil
}

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

func (db *DB) signalMessages(
	ctx context.Context,
	rows []SignalRow,
) (map[string][]SignalMessage, error) {
	out := make(map[string][]SignalMessage, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	err := queryChunked(ids, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		q := `SELECT session_id, ordinal, role, content,
					COALESCE(timestamp, ''), is_system, has_tool_use
				FROM messages
				WHERE session_id IN ` + ph + `
				ORDER BY session_id, ordinal`
		msgRows, err := db.getReader().QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf(
				"querying signal messages: %w", err,
			)
		}
		defer msgRows.Close()
		for msgRows.Next() {
			var m SignalMessage
			if err := msgRows.Scan(
				&m.SessionID, &m.Ordinal, &m.Role,
				&m.Content, &m.Timestamp,
				&m.IsSystem, &m.HasToolUse,
			); err != nil {
				return fmt.Errorf(
					"scanning signal message: %w", err,
				)
			}
			out[m.SessionID] = append(out[m.SessionID], m)
		}
		if err := msgRows.Err(); err != nil {
			return fmt.Errorf(
				"iterating signal messages: %w", err,
			)
		}
		return nil
	})
	return out, err
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
	ID           string  `json:"id"`
	Project      string  `json:"project"`
	FirstMessage *string `json:"first_message"`
	DisplayName  *string `json:"display_name,omitempty"`
	MessageCount int     `json:"message_count"`
	OutputTokens int     `json:"output_tokens"`
	DurationMin  float64 `json:"duration_min"`
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

// GetAnalyticsTopSessions returns the top 10 sessions by the
// given metric ("messages", "duration", or "output_tokens")
// within the filter.
func (db *DB) GetAnalyticsTopSessions(
	ctx context.Context, f AnalyticsFilter, metric string,
) (TopSessionsResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	if !f.canUseSQLiteTimeSQL() {
		return db.getAnalyticsTopSessionsGo(ctx, f, metric)
	}
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := sqliteAnalyticsWhereSQL(f, dateCol, "sessions.id", true)

	durationExpr := `ROUND((julianday(ended_at) -
		julianday(started_at)) * 1440, 1)`
	durationSelectExpr := "COALESCE(" + durationExpr + ", 0)"
	var orderExpr string
	switch metric {
	case "output_tokens":
		where += " AND has_total_output_tokens = TRUE"
		orderExpr = "total_output_tokens DESC, id ASC"
	case "duration":
		orderExpr = durationExpr + " DESC, id ASC"
		where += " AND NULLIF(started_at, '') IS NOT NULL" +
			" AND NULLIF(ended_at, '') IS NOT NULL" +
			" AND julianday(ended_at) >= julianday(started_at)"
	default:
		metric = "messages"
		orderExpr = "message_count DESC, id ASC"
	}

	query := `SELECT id, project, first_message,
		COALESCE(display_name, session_name) AS display_name,
		message_count, total_output_tokens, ` + durationSelectExpr + `,
		started_at, ended_at, termination_status
		FROM sessions WHERE ` + where +
		` ORDER BY ` + orderExpr + ` LIMIT 10`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return TopSessionsResponse{},
			fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	resp := TopSessionsResponse{Metric: metric}
	for rows.Next() {
		var row TopSession
		if err := rows.Scan(
			&row.ID, &row.Project, &row.FirstMessage,
			&row.DisplayName, &row.MessageCount,
			&row.OutputTokens, &row.DurationMin,
			&row.StartedAt, &row.EndedAt,
			&row.TerminationStatus,
		); err != nil {
			return TopSessionsResponse{},
				fmt.Errorf("scanning top session: %w", err)
		}
		resp.Sessions = append(resp.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return TopSessionsResponse{},
			fmt.Errorf("iterating top sessions: %w", err)
	}
	return resp, nil
}

func (db *DB) getAnalyticsTopSessionsGo(
	ctx context.Context, f AnalyticsFilter, metric string,
) (TopSessionsResponse, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = db.filteredSessionIDs(ctx, f)
		if err != nil {
			return TopSessionsResponse{}, err
		}
	}

	var orderExpr string
	switch metric {
	case "output_tokens":
		where += " AND has_total_output_tokens = TRUE"
		orderExpr = "total_output_tokens DESC, id ASC"
	case "duration":
		orderExpr = `(julianday(ended_at) -
			julianday(started_at)) * 1440 DESC, id ASC`
		where += " AND started_at IS NOT NULL" +
			" AND ended_at IS NOT NULL"
	default:
		metric = "messages"
		orderExpr = "message_count DESC, id ASC"
	}

	query := `SELECT id, ` + dateCol + `, project,
		first_message,
		COALESCE(display_name, session_name) AS display_name,
		message_count, total_output_tokens,
		started_at, ended_at, termination_status
		FROM sessions WHERE ` + where +
		` ORDER BY ` + orderExpr + ` LIMIT 200`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return TopSessionsResponse{},
			fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	var sessions []TopSession
	for rows.Next() {
		var id, ts, project string
		var firstMsg, displayName, startedAt, endedAt *string
		var termStatus *string
		var mc, outputTokens int
		if err := rows.Scan(
			&id, &ts, &project, &firstMsg,
			&displayName, &mc, &outputTokens,
			&startedAt, &endedAt, &termStatus,
		); err != nil {
			return TopSessionsResponse{},
				fmt.Errorf("scanning top session: %w", err)
		}
		date := localDate(ts, loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		durMin := 0.0
		if startedAt != nil && endedAt != nil {
			tS, okS := localTime(*startedAt, loc)
			tE, okE := localTime(*endedAt, loc)
			if okS && okE {
				durMin = math.Round(
					tE.Sub(tS).Minutes()*10) / 10
			}
		}
		sessions = append(sessions, TopSession{
			ID:                id,
			Project:           project,
			FirstMessage:      firstMsg,
			DisplayName:       displayName,
			MessageCount:      mc,
			OutputTokens:      outputTokens,
			DurationMin:       durMin,
			StartedAt:         startedAt,
			EndedAt:           endedAt,
			TerminationStatus: termStatus,
		})
	}
	if err := rows.Err(); err != nil {
		return TopSessionsResponse{},
			fmt.Errorf("iterating top sessions: %w", err)
	}

	if sessions == nil {
		sessions = []TopSession{}
	}
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}

	return TopSessionsResponse{
		Metric:   metric,
		Sessions: sessions,
	}, nil
}
