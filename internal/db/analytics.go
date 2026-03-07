package db

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// maxSQLVars is the maximum bind variables per IN clause to stay
// within SQLite's default SQLITE_MAX_VARIABLE_NUMBER (999).
const maxSQLVars = 500

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
	for i := 0; i < len(ids); i += maxSQLVars {
		end := min(i+maxSQLVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// AnalyticsFilter is the shared filter for all analytics queries.
type AnalyticsFilter struct {
	From            string // ISO date YYYY-MM-DD, inclusive
	To              string // ISO date YYYY-MM-DD, inclusive
	Machine         string // optional machine filter
	Project         string // optional project filter
	Agent           string // optional agent filter
	Timezone        string // IANA timezone for day bucketing
	DayOfWeek       *int   // nil = all, 0=Mon, 6=Sun (ISO)
	Hour            *int   // nil = all, 0-23
	MinUserMessages int    // user_message_count >= N
	ActiveSince     string // ISO timestamp cutoff
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
// to leverage the started_at index.
func (f AnalyticsFilter) utcRange() (string, string) {
	from := f.From + "T00:00:00Z"
	to := f.To + "T23:59:59Z"

	tFrom, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return from, to
	}
	tTo, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return from, to
	}

	// Pad by max UTC offset (±14h)
	paddedFrom := tFrom.Add(-14 * time.Hour).Format(time.RFC3339)
	paddedTo := tTo.Add(14 * time.Hour).Format(time.RFC3339)
	return paddedFrom, paddedTo
}

// buildWhere returns a WHERE clause and args for common
// analytics filters.
func (f AnalyticsFilter) buildWhere(
	dateCol string,
) (string, []any) {
	preds := []string{
		"message_count > 0",
		"relationship_type NOT IN ('subagent', 'fork')",
		"deleted_at IS NULL",
	}
	var args []any

	utcFrom, utcTo := f.utcRange()
	preds = append(preds, dateCol+" >= ?")
	args = append(args, utcFrom)
	preds = append(preds, dateCol+" <= ?")
	args = append(args, utcTo)

	if f.Machine != "" {
		preds = append(preds, "machine = ?")
		args = append(args, f.Machine)
	}

	if f.Project != "" {
		preds = append(preds, "project = ?")
		args = append(args, f.Project)
	}

	if f.Agent != "" {
		preds = append(preds, "agent = ?")
		args = append(args, f.Agent)
	}

	if f.MinUserMessages > 0 {
		preds = append(preds, "user_message_count >= ?")
		args = append(args, f.MinUserMessages)
	}

	if f.ActiveSince != "" {
		preds = append(preds,
			"COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at) >= ?")
		args = append(args, f.ActiveSince)
	}

	return strings.Join(preds, " AND "), args
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
func inDateRange(date, from, to string) bool {
	return date >= from && date <= to
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
	TotalSessions  int                      `json:"total_sessions"`
	TotalMessages  int                      `json:"total_messages"`
	ActiveProjects int                      `json:"active_projects"`
	ActiveDays     int                      `json:"active_days"`
	AvgMessages    float64                  `json:"avg_messages"`
	MedianMessages int                      `json:"median_messages"`
	P90Messages    int                      `json:"p90_messages"`
	MostActive     string                   `json:"most_active_project"`
	Concentration  float64                  `json:"concentration"`
	Agents         map[string]*AgentSummary `json:"agents"`
}

// GetAnalyticsSummary returns aggregate statistics.
func (db *DB) GetAnalyticsSummary(
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
		`, message_count, agent, project
		FROM sessions WHERE ` + where +
		` ORDER BY message_count ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return AnalyticsSummary{},
			fmt.Errorf("querying analytics summary: %w", err)
	}
	defer rows.Close()

	type sessionRow struct {
		date     string
		messages int
		agent    string
		project  string
	}

	var all []sessionRow
	for rows.Next() {
		var id, ts string
		var mc int
		var agent, project string
		if err := rows.Scan(
			&id, &ts, &mc, &agent, &project,
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
			date: date, messages: mc,
			agent: agent, project: project,
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
		m.role, m.has_thinking, COUNT(*)
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		WHERE ` + where + `
		GROUP BY s.id, m.role, m.has_thinking`

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
		var hasThinking *bool
		var count int
		if err := rows.Scan(
			&ts, &agent, &sid, &role,
			&hasThinking, &count,
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

		if role != nil {
			entry.Messages += count
			entry.ByAgent[agent] += count
			switch *role {
			case "user":
				entry.UserMessages += count
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

	query := `SELECT id, ` + dateCol + `, message_count
		FROM sessions WHERE ` + where

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return HeatmapResponse{},
			fmt.Errorf("querying analytics heatmap: %w", err)
	}
	defer rows.Close()

	dayCounts := make(map[string]int) // date -> count
	daySessions := make(map[string]int)

	for rows.Next() {
		var id, ts string
		var mc int
		if err := rows.Scan(&id, &ts, &mc); err != nil {
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
	}
	if err := rows.Err(); err != nil {
		return HeatmapResponse{},
			fmt.Errorf("iterating heatmap rows: %w", err)
	}

	// Choose which map to use based on metric
	source := dayCounts
	if metric == "sessions" {
		source = daySessions
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
		pd := projectMap[name]
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
		SUM(CASE WHEN role='user' THEN 1 ELSE 0 END),
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
	}
	var toolRows []toolRow

	err = queryChunked(sessionIDs,
		func(chunk []string) error {
			ph, chunkArgs := inPlaceholders(chunk)
			q := `SELECT session_id, category
				FROM tool_calls
				WHERE session_id IN ` + ph
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
				if err := rows.Scan(&sid, &cat); err != nil {
					return fmt.Errorf(
						"scanning tool_call: %w", err,
					)
				}
				toolRows = append(toolRows, toolRow{
					sessionID: sid, category: cat,
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
		catCounts[tr.category]++

		if agentCats[info.agent] == nil {
			agentCats[info.agent] = make(map[string]int)
		}
		agentCats[info.agent][tr.category]++

		week := bucketDate(info.date, "week")
		if trendBuckets[week] == nil {
			trendBuckets[week] = make(map[string]int)
		}
		trendBuckets[week][tr.category]++
	}

	resp.TotalCalls = len(toolRows)

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
	q := `SELECT session_id, ordinal, role,
		timestamp, content_length
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

	const maxCycleSec = 1800.0
	const maxGapSec = 300.0

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

		accums := []*velocityAccumulator{
			overall, byAgent[agentKey], byComplexity[compKey],
		}

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
						a.turnCycles = append(
							a.turnCycles, delta,
						)
					}
				}
			}
		}

		// First response: first user → first assistant after it
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
				if msgs[i].role == "assistant" &&
					msgs[i].valid {
					firstAsst = &msgs[i]
					break
				}
			}
		}
		if firstUser != nil && firstAsst != nil {
			delta := firstAsst.ts.Sub(firstUser.ts).Seconds()
			// Clamp negative deltas to 0: ordinal order is
			// authoritative, so a negative delta means clock
			// skew, not a missing response.
			if delta < 0 {
				delta = 0
			}
			for _, a := range accums {
				a.firstResponses = append(
					a.firstResponses, delta,
				)
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
			tc := toolCountMap[sid]
			for _, a := range accums {
				a.totalMsgs += len(msgs)
				a.totalChars += asstChars
				a.totalToolCalls += tc
				a.activeMinutes += activeMins
			}
		}
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
		a := byAgent[k]
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
		a := byComplexity[k]
		resp.ByComplexity = append(resp.ByComplexity,
			VelocityBreakdown{
				Label:    k,
				Sessions: a.sessions,
				Overview: a.computeOverview(),
			})
	}

	return resp, nil
}

// --- Top Sessions ---

// TopSession holds summary info for a ranked session.
type TopSession struct {
	ID           string  `json:"id"`
	Project      string  `json:"project"`
	FirstMessage *string `json:"first_message"`
	MessageCount int     `json:"message_count"`
	DurationMin  float64 `json:"duration_min"`
}

// TopSessionsResponse wraps the top sessions list.
type TopSessionsResponse struct {
	Metric   string       `json:"metric"`
	Sessions []TopSession `json:"sessions"`
}

// GetAnalyticsTopSessions returns the top 10 sessions by the
// given metric ("messages" or "duration") within the filter.
func (db *DB) GetAnalyticsTopSessions(
	ctx context.Context, f AnalyticsFilter, metric string,
) (TopSessionsResponse, error) {
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
			return TopSessionsResponse{}, err
		}
	}

	var orderExpr string
	switch metric {
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
		first_message, message_count,
		started_at, ended_at
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
		var firstMsg, startedAt, endedAt *string
		var mc int
		if err := rows.Scan(
			&id, &ts, &project, &firstMsg,
			&mc, &startedAt, &endedAt,
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
			ID:           id,
			Project:      project,
			FirstMessage: firstMsg,
			MessageCount: mc,
			DurationMin:  durMin,
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
