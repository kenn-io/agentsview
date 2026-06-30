package postgres

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

// maxPGVars is the maximum bind variables per IN clause.
const maxPGVars = 500

// pgQueryChunked executes a callback for each chunk of IDs,
// splitting at maxPGVars to avoid excessive bind variables.
func pgQueryChunked(
	ids []string,
	fn func(chunk []string) error,
) error {
	for i := 0; i < len(ids); i += maxPGVars {
		end := min(i+maxPGVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// pgInPlaceholders returns a "(placeholders)" string for PG
// numbered parameters.
func pgInPlaceholders(
	ids []string, pb *paramBuilder,
) string {
	phs := make([]string, len(ids))
	for i, id := range ids {
		phs[i] = pb.add(id)
	}
	return "(" + strings.Join(phs, ",") + ")"
}

// analyticsUTCRange returns UTC time bounds padded by +/-14h
// to cover all possible timezone offsets. Empty From/To
// inputs (callers like the Store API can construct a zero
// AnalyticsFilter when "all time" is intended) collapse to
// effectively unbounded sentinel values so the resulting
// ::timestamptz cast is always valid -- the previous version
// concatenated empty + "T00:00:00Z" and produced literals
// like "T00:00:00Z" which PG rejected at runtime.
func analyticsUTCRange(
	f db.AnalyticsFilter,
) (string, string) {
	const (
		// Wide-open sentinels. PG TIMESTAMPTZ tolerates
		// these literals across every supported version.
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
	// Padding by ±14h could push the lower sentinel below
	// year 1 (which TIMESTAMPTZ does not accept); skip the
	// pad when we're already on a sentinel boundary.
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

// buildAnalyticsWhere builds a WHERE clause with PG
// placeholders. dateCol is the date expression.
func buildAnalyticsWhere(
	f db.AnalyticsFilter,
	dateCol string,
	pb *paramBuilder,
) string {
	return buildAnalyticsWhereWithDate(
		f, dateCol, pb, true, "id",
	)
}

// buildAnalyticsWhereWithoutDate returns common analytics
// predicates without adding session date bounds. Trends uses
// this because date, day, and hour filters are evaluated
// against message timestamps instead of session timestamps.
func buildAnalyticsWhereWithoutDate(
	f db.AnalyticsFilter,
	pb *paramBuilder,
) string {
	return buildAnalyticsWhereWithDate(
		f, "", pb, false, "id",
	)
}

func buildAnalyticsWhereWithDate(
	f db.AnalyticsFilter,
	dateCol string,
	pb *paramBuilder,
	includeDate bool,
	sessionIDExpr string,
) string {
	if sessionIDExpr == "" {
		sessionIDExpr = "id"
	}
	preds := []string{
		"message_count > 0",
		// Mirror the SQLite analytics filter: count subagents only on
		// opt-in sum/count surfaces; fork rows stay excluded always.
		f.RelationshipExclusionSQL(),
		"deleted_at IS NULL",
	}
	if includeDate {
		utcFrom, utcTo := analyticsUTCRange(f)
		preds = append(preds,
			dateCol+" >= "+pb.add(utcFrom)+"::timestamptz")
		preds = append(preds,
			dateCol+" <= "+pb.add(utcTo)+"::timestamptz")
	}
	if f.Machine != "" {
		preds = appendPGAnalyticsCSVFilter(
			preds, "machine", f.Machine, pb)
	}
	if f.Project != "" {
		preds = append(preds,
			"project = "+pb.add(f.Project))
	}
	if f.GitBranch != "" {
		preds = append(preds, db.BranchPairPredicate(
			"project", "git_branch", f.GitBranch,
			func(s string) string { return pb.add(s) }))
	}
	if f.Agent != "" {
		preds = appendPGAnalyticsCSVFilter(
			preds, "agent", f.Agent, pb)
	}
	if f.Model != "" {
		models := csvFilterValues(f.Model)
		if len(models) == 1 {
			preds = append(preds,
				"EXISTS (SELECT 1 FROM messages m WHERE "+
					"m.session_id = "+sessionIDExpr+
					" AND m.model = "+pb.add(models[0])+")")
		} else if len(models) > 1 {
			phs := make([]string, len(models))
			for i, model := range models {
				phs[i] = pb.add(model)
			}
			preds = append(preds,
				"EXISTS (SELECT 1 FROM messages m WHERE "+
					"m.session_id = "+sessionIDExpr+
					" AND m.model IN ("+
					strings.Join(phs, ",")+
					"))")
		}
	}
	if f.MinUserMessages > 0 {
		preds = append(preds,
			"user_message_count >= "+
				pb.add(f.MinUserMessages))
	}
	scope := normalizePGAutomatedScope(
		f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if scope != "human" {
			preds = append(preds,
				f.OneShotExclusionSQL(
					"(user_message_count > 1 OR is_automated = TRUE)"))
		} else {
			preds = append(preds,
				f.OneShotExclusionSQL("user_message_count > 1"))
		}
	}
	if pred := pgAutomatedScopePredicate(scope, "is_automated"); pred != "" {
		preds = append(preds, pred)
	}
	if f.ExcludeInteractive {
		preds = append(preds, "is_automated = TRUE")
	}
	if f.ActiveSince != "" {
		preds = append(preds,
			"COALESCE(ended_at, started_at, created_at)"+
				" >= "+pb.add(f.ActiveSince)+
				"::timestamptz")
	}
	if pred := pgTerminationPred(f.Termination, pb); pred != "" {
		preds = append(preds, pred)
	}
	return strings.Join(preds, " AND ")
}

func appendPGAnalyticsCSVFilter(
	preds []string,
	col string,
	raw string,
	pb *paramBuilder,
) []string {
	values := pgAnalyticsCSVValues(raw)
	if len(values) == 0 {
		return preds
	}
	if len(values) == 1 {
		return append(preds, col+" = "+pb.add(values[0]))
	}
	phs := make([]string, len(values))
	for i, value := range values {
		phs[i] = pb.add(value)
	}
	return append(preds,
		col+" IN ("+strings.Join(phs, ",")+")")
}

func pgAnalyticsCSVValues(raw string) []string {
	return csvFilterValues(raw)
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

func (s *Store) getAnalyticsFilteredMessageCounts(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string]int, error) {
	stats, err := s.getAnalyticsFilteredMessageStats(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(stats))
	for sessionID, stat := range stats {
		counts[sessionID] = stat.Messages
	}
	return counts, nil
}

func (s *Store) getAnalyticsModelScopedMessages(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string][]db.ScopedMessage, error) {
	scope, err := s.resolveAnalyticsMessageScope(ctx, sessionIDs, f, true)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return map[string][]db.ScopedMessage{}, nil
	}
	return scope.MessagesBySession(), nil
}

func (s *Store) getAnalyticsFilteredMessageStats(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string]db.MessageStats, error) {
	scope, err := s.resolveAnalyticsMessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return map[string]db.MessageStats{}, nil
	}
	return scope.StatsBySession(), nil
}

// localTime parses a UTC timestamp string and converts it to
// the given location.
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
// string (YYYY-MM-DD).
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

// medianInt returns the median of a sorted int slice.
func medianInt(sorted []int, n int) int {
	if n == 0 {
		return 0
	}
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// percentileFloat returns the value at the given percentile
// from a pre-sorted float64 slice.
func percentileFloat(
	sorted []float64, pct float64,
) float64 {
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

// analyticsLocation loads the timezone from the filter.
func analyticsLocation(
	f db.AnalyticsFilter,
) *time.Location {
	if f.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// matchesTimeFilter checks whether a local time matches the
// active hour and/or day-of-week filter.
func matchesTimeFilter(
	f db.AnalyticsFilter, t time.Time,
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

// pgDateCol is the date column expression for analytics.
const pgDateCol = "COALESCE(started_at, created_at)"

// pgDateColS is the date column with "s." table prefix.
const pgDateColS = "COALESCE(s.started_at, s.created_at)"

// filteredSessionIDs returns session IDs that have at least
// one message matching the hour/dow filter.
// filteredSessionIDs returns the session IDs that have at least one message
// matching the hour/dow filter. With a model filter active it pairs through the
// shared scope reducer (see filteredSessionIDsModel) so an empty-model user turn
// at the selected hour keeps its session, matching the model-scoped panels.
func (s *Store) filteredSessionIDs(
	ctx context.Context, f db.AnalyticsFilter,
) (map[string]bool, error) {
	if strings.TrimSpace(f.Model) != "" {
		return s.filteredSessionIDsModel(ctx, f)
	}
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(
		f, pgDateColS, pb, true, "s.id",
	)
	preds := []string{
		where,
		"m.timestamp IS NOT NULL",
	}

	query := `SELECT s.id,
		TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
			'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + strings.Join(preds, " AND ")

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
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
			continue
		}
		t, ok := localTime(msgTS, loc)
		if !ok {
			continue
		}
		if matchesTimeFilter(f, t) {
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

// filteredSessionIDsModel returns the sessions that have at least one
// model-scoped message matching the hour/dow filter. It runs the shared scope
// reducer (with the day/hour filter) instead of a direct m.model predicate, so
// an empty-model user turn paired with a selected-model assistant keeps its
// session when the user turn falls in the selected hour.
func (s *Store) filteredSessionIDsModel(
	ctx context.Context, f db.AnalyticsFilter,
) (map[string]bool, error) {
	sessionIDs, err := s.analyticsModelCandidateSessionIDs(ctx, f)
	if err != nil {
		return nil, err
	}
	scope, err := s.resolveAnalyticsMessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool)
	if scope != nil {
		for id := range scope.MessagesBySession() {
			ids[id] = true
		}
	}
	return ids, nil
}

// bucketDate truncates a date to the start of its bucket.
func bucketDate(date string, granularity string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	switch granularity {
	case "week":
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

func (s *Store) getModelScopedToolCallCounts(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string]int, error) {
	counts := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 || strings.TrimSpace(f.Model) == "" {
		return counts, nil
	}

	flt := messageScopeFilter(f)
	loc := analyticsLocation(f)
	if err := pgQueryChunked(sessionIDs, func(chunk []string) error {
		pb := &paramBuilder{}
		placeholders := pgInPlaceholders(chunk, pb)
		rows, err := s.pg.QueryContext(ctx, `
			SELECT tc.session_id, m.model,
				COALESCE(TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
					'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
				COUNT(*)
			FROM tool_calls tc
			JOIN messages m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal
			WHERE tc.session_id IN `+placeholders+`
			GROUP BY tc.session_id, m.model,
				COALESCE(TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
					'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')`,
			pb.args...,
		)
		if err != nil {
			return fmt.Errorf(
				"querying filtered analytics tool calls: %w",
				err,
			)
		}
		defer rows.Close()

		for rows.Next() {
			var sessionID, model, ts string
			var count int
			if err := rows.Scan(&sessionID, &model, &ts, &count); err != nil {
				return fmt.Errorf(
					"scanning filtered analytics tool calls: %w",
					err,
				)
			}
			if _, ok := flt.Models[model]; !ok {
				continue
			}
			parsed, has := localTime(ts, loc)
			if !flt.MatchesDayHour(parsed, has) {
				continue
			}
			counts[sessionID] += count
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}

	return counts, nil
}

func (s *Store) getAnalyticsActivityFilteredByModelTime(
	ctx context.Context,
	f db.AnalyticsFilter,
	granularity string,
) (db.ActivityResponse, error) {
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)
	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.ActivityResponse{}, err
		}
	}

	rows, err := s.pg.QueryContext(ctx, `SELECT id, `+pgDateCol+`, agent
		FROM sessions
		WHERE `+where, pb.args...)
	if err != nil {
		return db.ActivityResponse{},
			fmt.Errorf("querying analytics activity sessions: %w", err)
	}
	defer rows.Close()

	type sessionRow struct {
		id, date, agent string
	}
	sessions := make([]sessionRow, 0)
	sessionIDs := make([]string, 0)
	for rows.Next() {
		var id, agent string
		var ts *time.Time
		if err := rows.Scan(&id, &ts, &agent); err != nil {
			return db.ActivityResponse{},
				fmt.Errorf("scanning analytics activity session: %w", err)
		}
		date := localDate(scanDateCol(ts), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		sessions = append(sessions, sessionRow{id: id, date: date, agent: agent})
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return db.ActivityResponse{},
			fmt.Errorf("iterating analytics activity sessions: %w", err)
	}

	messageStats, err := s.getAnalyticsFilteredMessageStats(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	toolCounts, err := s.getModelScopedToolCallCounts(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.ActivityResponse{}, err
	}

	buckets := make(map[string]*db.ActivityEntry)
	for _, session := range sessions {
		bucket := bucketDate(session.date, granularity)
		entry := buckets[bucket]
		if entry == nil {
			entry = &db.ActivityEntry{
				Date:    bucket,
				ByAgent: make(map[string]int),
			}
			buckets[bucket] = entry
		}
		entry.Sessions++
		stat := messageStats[session.id]
		entry.Messages += stat.Messages
		entry.UserMessages += stat.UserMessages
		entry.AssistantMessages += stat.AssistantMessages
		entry.ThinkingMessages += stat.ThinkingMessages
		entry.ToolCalls += toolCounts[session.id]
		entry.ByAgent[session.agent] += stat.Messages
	}

	series := make([]db.ActivityEntry, 0, len(buckets))
	for _, entry := range buckets {
		series = append(series, *entry)
	}
	sort.Slice(series, func(i, j int) bool {
		return series[i].Date < series[j].Date
	})
	return db.ActivityResponse{
		Granularity: granularity,
		Series:      series,
	}, nil
}

// scanDateCol scans a TIMESTAMPTZ column and returns it as
// an ISO-8601 string for client-side date processing.
func scanDateCol(t *time.Time) string {
	if t == nil {
		return ""
	}
	return FormatISO8601(*t)
}

func (s *Store) getAnalyticsModelsForSessionIDs(
	ctx context.Context,
	sessionIDs []string,
) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	unique := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		unique = append(unique, sessionID)
	}

	modelSet := make(map[string]struct{})
	models := make([]string, 0)
	if err := pgQueryChunked(unique, func(chunk []string) error {
		pb := &paramBuilder{}
		placeholders := pgInPlaceholders(chunk, pb)
		rows, err := s.pg.QueryContext(ctx, `
			SELECT DISTINCT model
			FROM messages
			WHERE session_id IN `+placeholders+`
			AND COALESCE(model, '') <> ''
			ORDER BY model`,
			pb.args...,
		)
		if err != nil {
			return fmt.Errorf("querying analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return fmt.Errorf("scanning analytics model: %w", err)
			}
			if _, ok := modelSet[model]; ok {
				continue
			}
			modelSet[model] = struct{}{}
			models = append(models, model)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating analytics models: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(models)
	return models, nil
}

func (s *Store) getAnalyticsModelsForSessionIDsFiltered(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	unique := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		unique = append(unique, sessionID)
	}

	filterModels := csvFilterValues(f.Model)
	allowedModels := make(map[string]struct{}, len(filterModels))
	for _, model := range filterModels {
		allowedModels[model] = struct{}{}
	}
	loc := analyticsLocation(f)
	modelSet := make(map[string]struct{})
	models := make([]string, 0)
	if err := pgQueryChunked(unique, func(chunk []string) error {
		pb := &paramBuilder{}
		placeholders := pgInPlaceholders(chunk, pb)
		rows, err := s.pg.QueryContext(ctx, `
			SELECT model,
				COALESCE(TO_CHAR(timestamp AT TIME ZONE 'UTC',
					'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
			FROM messages
			WHERE session_id IN `+placeholders+`
			AND COALESCE(model, '') <> ''`,
			pb.args...,
		)
		if err != nil {
			return fmt.Errorf("querying filtered analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model, ts string
			if err := rows.Scan(&model, &ts); err != nil {
				return fmt.Errorf("scanning filtered analytics model: %w", err)
			}
			if len(allowedModels) > 0 {
				if _, ok := allowedModels[model]; !ok {
					continue
				}
			}
			if f.HasTimeFilter() {
				t, ok := localTime(ts, loc)
				if !ok || !matchesTimeFilter(f, t) {
					continue
				}
			}
			if _, ok := modelSet[model]; ok {
				continue
			}
			modelSet[model] = struct{}{}
			models = append(models, model)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating filtered analytics models: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(models)
	return models, nil
}

// --- Summary ---

// GetAnalyticsSummary returns aggregate statistics.
func (s *Store) GetAnalyticsSummary(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	// Sum/count aggregate: count subagent sessions (mirrors SQLite).
	f.IncludeSubagents = true
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.AnalyticsSummary{}, err
		}
	}

	query := `SELECT id, ` + pgDateCol +
		`, message_count, agent, project,
		total_output_tokens, has_total_output_tokens
		FROM sessions WHERE ` + where +
		` ORDER BY message_count ASC`

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.AnalyticsSummary{},
			fmt.Errorf(
				"querying analytics summary: %w", err,
			)
	}
	defer rows.Close()

	type sessionRow struct {
		id           string
		date         string
		messages     int
		agent        string
		project      string
		outputTokens int
		hasTokens    bool
	}

	var all []sessionRow
	for rows.Next() {
		var id string
		var ts *time.Time
		var mc int
		var agent, project string
		var outputTokens int
		var hasTokens bool
		if err := rows.Scan(
			&id, &ts, &mc, &agent, &project,
			&outputTokens, &hasTokens,
		); err != nil {
			return db.AnalyticsSummary{},
				fmt.Errorf(
					"scanning summary row: %w", err,
				)
		}
		date := localDate(scanDateCol(ts), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		all = append(all, sessionRow{
			id:           id,
			date:         date,
			messages:     mc,
			agent:        agent,
			project:      project,
			outputTokens: outputTokens,
			hasTokens:    hasTokens,
		})
	}
	if err := rows.Err(); err != nil {
		return db.AnalyticsSummary{},
			fmt.Errorf(
				"iterating summary rows: %w", err,
			)
	}

	var summary db.AnalyticsSummary
	summary.Agents = make(map[string]*db.AgentSummary)
	summary.Models = []string{}

	if len(all) == 0 {
		return summary, nil
	}

	if f.Model != "" {
		sessionIDs := make([]string, 0, len(all))
		for _, r := range all {
			sessionIDs = append(sessionIDs, r.id)
		}
		stats, err := s.getAnalyticsFilteredMessageStats(
			ctx, sessionIDs, f,
		)
		if err != nil {
			return db.AnalyticsSummary{}, err
		}
		for i := range all {
			stat := stats[all[i].id]
			all[i].messages = stat.Messages
			all[i].outputTokens = stat.OutputTokens
			all[i].hasTokens = stat.HasOutputTokens
		}
	}

	days := make(map[string]bool)
	projects := make(map[string]int)
	msgCounts := make([]int, 0, len(all))
	sessionIDs := make([]string, 0, len(all))

	for _, r := range all {
		summary.TotalSessions++
		summary.TotalMessages += r.messages
		if r.hasTokens {
			summary.TotalOutputTokens += r.outputTokens
			summary.TokenReportingSessions++
		}
		days[r.date] = true
		projects[r.project] += r.messages
		msgCounts = append(msgCounts, r.messages)
		sessionIDs = append(sessionIDs, r.id)

		if summary.Agents[r.agent] == nil {
			summary.Agents[r.agent] = &db.AgentSummary{}
		}
		summary.Agents[r.agent].Sessions++
		summary.Agents[r.agent].Messages += r.messages
	}

	var models []string
	if strings.TrimSpace(f.Model) != "" || f.HasTimeFilter() {
		models, err = s.getAnalyticsModelsForSessionIDsFiltered(
			ctx, sessionIDs, f,
		)
	} else {
		models, err = s.getAnalyticsModelsForSessionIDs(ctx, sessionIDs)
	}
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	summary.Models = models

	summary.ActiveProjects = len(projects)
	summary.ActiveDays = len(days)
	summary.AvgMessages = math.Round(
		float64(summary.TotalMessages)/
			float64(summary.TotalSessions)*10,
	) / 10

	sort.Ints(msgCounts)
	n := len(msgCounts)
	if n%2 == 0 {
		summary.MedianMessages =
			(msgCounts[n/2-1] + msgCounts[n/2]) / 2
	} else {
		summary.MedianMessages = msgCounts[n/2]
	}
	p90Idx := int(float64(n) * 0.9)
	if p90Idx >= n {
		p90Idx = n - 1
	}
	summary.P90Messages = msgCounts[p90Idx]

	maxMsgs := 0
	for name, count := range projects {
		if count > maxMsgs ||
			(count == maxMsgs && name < summary.MostActive) {
			maxMsgs = count
			summary.MostActive = name
		}
	}

	if summary.TotalMessages > 0 {
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
		summary.Concentration = math.Round(
			float64(topSum)/
				float64(summary.TotalMessages)*1000,
		) / 1000
	}

	return summary, nil
}

// --- Activity ---

// GetAnalyticsActivity returns session/message counts grouped
// by time bucket.
func (s *Store) GetAnalyticsActivity(
	ctx context.Context, f db.AnalyticsFilter,
	granularity string,
) (db.ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsActivityFilteredByModelTime(
			ctx, f, granularity,
		)
	}
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(
		f, pgDateColS, pb, true, "s.id",
	)
	preds := appendPGAnalyticsCSVFilter(
		[]string{where}, "m.model", f.Model, pb,
	)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.ActivityResponse{}, err
		}
	}

	query := `SELECT ` + pgDateColS + `, s.agent, s.id,
		m.role, m.has_thinking, COUNT(*)
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		WHERE ` + strings.Join(preds, " AND ") + `
		GROUP BY s.id, ` + pgDateColS +
		`, s.agent, m.role, m.has_thinking`

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.ActivityResponse{},
			fmt.Errorf(
				"querying analytics activity: %w", err,
			)
	}
	defer rows.Close()

	buckets := make(map[string]*db.ActivityEntry)
	sessionSeen := make(map[string]string)
	var sessionIDs []string

	for rows.Next() {
		var tsVal *time.Time
		var agent, sid string
		var role *string
		var hasThinking *bool
		var count int
		if err := rows.Scan(
			&tsVal, &agent, &sid, &role,
			&hasThinking, &count,
		); err != nil {
			return db.ActivityResponse{},
				fmt.Errorf(
					"scanning activity row: %w", err,
				)
		}

		date := localDate(scanDateCol(tsVal), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[sid] {
			continue
		}
		bucket := bucketDate(date, granularity)

		entry, ok := buckets[bucket]
		if !ok {
			entry = &db.ActivityEntry{
				Date:    bucket,
				ByAgent: make(map[string]int),
			}
			buckets[bucket] = entry
		}

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
		return db.ActivityResponse{},
			fmt.Errorf(
				"iterating activity rows: %w", err,
			)
	}

	if len(sessionIDs) > 0 {
		err = pgQueryChunked(sessionIDs,
			func(chunk []string) error {
				return s.mergeActivityToolCalls(
					ctx, chunk, sessionSeen, buckets, f.Model,
				)
			})
		if err != nil {
			return db.ActivityResponse{}, err
		}
	}

	series := make([]db.ActivityEntry, 0, len(buckets))
	for _, e := range buckets {
		series = append(series, *e)
	}
	sort.Slice(series, func(i, j int) bool {
		return series[i].Date < series[j].Date
	})

	return db.ActivityResponse{
		Granularity: granularity,
		Series:      series,
	}, nil
}

// mergeActivityToolCalls queries tool_calls for a chunk of
// session IDs and adds counts to the matching activity
// buckets.
func (s *Store) mergeActivityToolCalls(
	ctx context.Context,
	chunk []string,
	sessionBucket map[string]string,
	buckets map[string]*db.ActivityEntry,
	model string,
) error {
	pb := &paramBuilder{}
	ph := pgInPlaceholders(chunk, pb)
	preds := []string{"tc.session_id IN " + ph}
	preds = appendPGAnalyticsCSVFilter(
		preds, "m.model", model, pb,
	)
	q := `SELECT tc.session_id, COUNT(*)
		FROM tool_calls tc`
	if model != "" {
		q += `
		JOIN messages m
			ON m.session_id = tc.session_id
			AND m.ordinal = tc.message_ordinal`
	}
	q += `
		WHERE ` + strings.Join(preds, " AND ") + `
		GROUP BY tc.session_id`
	rows, err := s.pg.QueryContext(ctx, q, pb.args...)
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

// MaxHeatmapDays is the maximum number of day entries.
const MaxHeatmapDays = 366

// clampFrom returns from clamped so [from, to] spans at
// most MaxHeatmapDays.
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

// computeQuartileLevels computes thresholds from sorted
// values.
func computeQuartileLevels(
	sorted []int,
) db.HeatmapLevels {
	if len(sorted) == 0 {
		return db.HeatmapLevels{
			L1: 1, L2: 2, L3: 3, L4: 4,
		}
	}
	n := len(sorted)
	return db.HeatmapLevels{
		L1: sorted[0],
		L2: sorted[n/4],
		L3: sorted[n/2],
		L4: sorted[n*3/4],
	}
}

// assignLevel determines the heatmap level (0-4) for a value.
func assignLevel(value int, levels db.HeatmapLevels) int {
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

// buildDateEntries creates a HeatmapEntry for each day in
// [from, to].
func buildDateEntries(
	from, to string,
	values map[string]int,
	levels db.HeatmapLevels,
) []db.HeatmapEntry {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil
	}

	entries := []db.HeatmapEntry{}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		v := values[date]
		entries = append(entries, db.HeatmapEntry{
			Date:  date,
			Value: v,
			Level: assignLevel(v, levels),
		})
	}
	return entries
}

// GetAnalyticsHeatmap returns daily counts with intensity
// levels.
func (s *Store) GetAnalyticsHeatmap(
	ctx context.Context, f db.AnalyticsFilter,
	metric string,
) (db.HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
	}

	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.HeatmapResponse{}, err
		}
	}

	query := `SELECT id, ` + pgDateCol +
		`, message_count, total_output_tokens,
		has_total_output_tokens
		FROM sessions WHERE ` + where

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.HeatmapResponse{},
			fmt.Errorf(
				"querying analytics heatmap: %w", err,
			)
	}
	defer rows.Close()

	type heatmapRow struct {
		id           string
		date         string
		messages     int
		outputTokens int
		hasTokens    bool
	}

	var heatmapRows []heatmapRow
	dayCounts := make(map[string]int)
	daySessions := make(map[string]int)
	dayOutputTokens := make(map[string]int)

	for rows.Next() {
		var id string
		var ts *time.Time
		var mc, outputTokens int
		var hasTokens bool
		if err := rows.Scan(
			&id, &ts, &mc, &outputTokens, &hasTokens,
		); err != nil {
			return db.HeatmapResponse{},
				fmt.Errorf(
					"scanning heatmap row: %w", err,
				)
		}
		date := localDate(scanDateCol(ts), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		heatmapRows = append(heatmapRows, heatmapRow{
			id:           id,
			date:         date,
			messages:     mc,
			outputTokens: outputTokens,
			hasTokens:    hasTokens,
		})
	}
	if err := rows.Err(); err != nil {
		return db.HeatmapResponse{},
			fmt.Errorf(
				"iterating heatmap rows: %w", err,
			)
	}

	if f.Model != "" && (metric == "messages" || metric == "output_tokens") {
		ids := make([]string, 0, len(heatmapRows))
		for _, row := range heatmapRows {
			ids = append(ids, row.id)
		}
		stats, err := s.getAnalyticsFilteredMessageStats(ctx, ids, f)
		if err != nil {
			return db.HeatmapResponse{}, err
		}
		for i := range heatmapRows {
			stat := stats[heatmapRows[i].id]
			heatmapRows[i].messages = stat.Messages
			heatmapRows[i].outputTokens = stat.OutputTokens
			heatmapRows[i].hasTokens = stat.HasOutputTokens
		}
	}

	for _, row := range heatmapRows {
		dayCounts[row.date] += row.messages
		daySessions[row.date]++
		if row.hasTokens {
			dayOutputTokens[row.date] += row.outputTokens
		}
	}

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
		return db.HeatmapResponse{
			Metric:      metric,
			EntriesFrom: clampFrom(f.From, f.To),
		}, nil
	}

	entriesFrom := clampFrom(f.From, f.To)

	var values []int
	for date, v := range source {
		if v > 0 && date >= entriesFrom && date <= f.To {
			values = append(values, v)
		}
	}
	sort.Ints(values)

	levels := computeQuartileLevels(values)

	entries := buildDateEntries(
		entriesFrom, f.To, source, levels,
	)

	return db.HeatmapResponse{
		Metric:      metric,
		Entries:     entries,
		Levels:      levels,
		EntriesFrom: entriesFrom,
	}, nil
}

// --- Projects ---

// GetAnalyticsProjects returns per-project analytics.
func (s *Store) GetAnalyticsProjects(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ProjectsAnalyticsResponse, error) {
	// Per-project aggregate: count subagent sessions (mirrors SQLite).
	f.IncludeSubagents = true
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.ProjectsAnalyticsResponse{}, err
		}
	}

	query := `SELECT id, project, ` + pgDateCol + `,
		message_count, agent
		FROM sessions WHERE ` + where +
		` ORDER BY project, ` + pgDateCol

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.ProjectsAnalyticsResponse{},
			fmt.Errorf(
				"querying analytics projects: %w", err,
			)
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
	type projectRow struct {
		id       string
		project  string
		date     string
		messages int
		agent    string
	}
	var projectRows []projectRow

	for rows.Next() {
		var id, project, agent string
		var ts *time.Time
		var mc int
		if err := rows.Scan(
			&id, &project, &ts, &mc, &agent,
		); err != nil {
			return db.ProjectsAnalyticsResponse{},
				fmt.Errorf(
					"scanning project row: %w", err,
				)
		}
		date := localDate(scanDateCol(ts), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}

		projectRows = append(projectRows, projectRow{
			id:       id,
			project:  project,
			date:     date,
			messages: mc,
			agent:    agent,
		})
	}
	if err := rows.Err(); err != nil {
		return db.ProjectsAnalyticsResponse{},
			fmt.Errorf(
				"iterating project rows: %w", err,
			)
	}

	if f.Model != "" {
		ids := make([]string, 0, len(projectRows))
		for _, row := range projectRows {
			ids = append(ids, row.id)
		}
		counts, err := s.getAnalyticsFilteredMessageCounts(
			ctx, ids, f,
		)
		if err != nil {
			return db.ProjectsAnalyticsResponse{}, err
		}
		for i := range projectRows {
			projectRows[i].messages = counts[projectRows[i].id]
		}
	}

	for _, row := range projectRows {
		pd, ok := projectMap[row.project]
		if !ok {
			pd = &projectData{
				name:   row.project,
				agents: make(map[string]int),
				days:   make(map[string]int),
			}
			projectMap[row.project] = pd
			projectOrder = append(projectOrder, row.project)
		}

		pd.sessions++
		pd.messages += row.messages
		pd.counts = append(pd.counts, row.messages)
		pd.agents[row.agent]++
		pd.days[row.date] += row.messages

		if pd.first == "" || row.date < pd.first {
			pd.first = row.date
		}
		if row.date > pd.last {
			pd.last = row.date
		}
	}

	projects := make(
		[]db.ProjectAnalytics, 0, len(projectMap),
	)
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

		trend := 0.0
		if len(pd.days) > 0 {
			trend = math.Round(
				float64(pd.messages)/
					float64(len(pd.days))*10,
			) / 10
		}

		projects = append(projects, db.ProjectAnalytics{
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

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Messages > projects[j].Messages
	})

	return db.ProjectsAnalyticsResponse{
		Projects: projects,
	}, nil
}

// --- Hour-of-Week ---

// GetAnalyticsHourOfWeek returns message counts bucketed by
// day-of-week and hour-of-day.
func (s *Store) GetAnalyticsHourOfWeek(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsHourOfWeekFilteredByModel(ctx, f)
	}
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(
		f, pgDateColS, pb, true, "s.id",
	)
	preds := []string{where, "m.timestamp IS NOT NULL"}

	query := `SELECT ` + pgDateColS + `,
		TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
			'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + strings.Join(preds, " AND ")

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.HourOfWeekResponse{},
			fmt.Errorf(
				"querying hour-of-week: %w", err,
			)
	}
	defer rows.Close()

	var grid [7][24]int

	for rows.Next() {
		var sessTS *time.Time
		var msgTS string
		if err := rows.Scan(&sessTS, &msgTS); err != nil {
			return db.HourOfWeekResponse{},
				fmt.Errorf(
					"scanning hour-of-week row: %w",
					err,
				)
		}
		sessDate := localDate(scanDateCol(sessTS), loc)
		if !inDateRange(sessDate, f.From, f.To) {
			continue
		}
		t, ok := localTime(msgTS, loc)
		if !ok {
			continue
		}
		dow := (int(t.Weekday()) + 6) % 7
		grid[dow][t.Hour()]++
	}
	if err := rows.Err(); err != nil {
		return db.HourOfWeekResponse{},
			fmt.Errorf(
				"iterating hour-of-week rows: %w", err,
			)
	}

	return db.HourOfWeekResponseFromGrid(grid), nil
}

// getAnalyticsHourOfWeekFilteredByModel buckets model-scoped messages by
// day-of-week and hour. It pairs empty-model user turns with their
// selected-model assistant via the shared scope reducer, so those turns appear
// in the heatmap consistently with the summary, activity, velocity, and trends
// panels. The heatmap is the control that sets the day/hour filter, so it
// clears DayOfWeek/Hour before scoping to keep showing the full grid, matching
// the no-model path.
// analyticsModelCandidateSessionIDs returns the date-filtered, model-scoped
// session IDs that feed the shared message-scope reducer. The day/hour filter
// is intentionally not applied here: callers that need it let the reducer apply
// it (so paired empty-model user turns are kept), while the hour-of-week
// heatmap clears it to show the full grid.
func (s *Store) analyticsModelCandidateSessionIDs(
	ctx context.Context, f db.AnalyticsFilter,
) ([]string, error) {
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(
		f, pgDateCol, pb, true, "id",
	)

	rows, err := s.pg.QueryContext(ctx, `SELECT id, `+pgDateCol+`
		FROM sessions
		WHERE `+where, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying model candidate sessions: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		var ts *time.Time
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, fmt.Errorf("scanning model candidate session: %w", err)
		}
		if !inDateRange(localDate(scanDateCol(ts), loc), f.From, f.To) {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating model candidate sessions: %w", err)
	}
	return ids, nil
}

func (s *Store) getAnalyticsHourOfWeekFilteredByModel(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	sessionIDs, err := s.analyticsModelCandidateSessionIDs(ctx, f)
	if err != nil {
		return db.HourOfWeekResponse{}, err
	}

	scopeFilter := f
	scopeFilter.DayOfWeek = nil
	scopeFilter.Hour = nil
	scope, err := s.resolveAnalyticsMessageScope(
		ctx, sessionIDs, scopeFilter, false,
	)
	if err != nil {
		return db.HourOfWeekResponse{}, err
	}

	var grid [7][24]int
	if scope != nil {
		for _, msgs := range scope.MessagesBySession() {
			for _, m := range msgs {
				if !m.HasLocalTime {
					continue
				}
				dow := (int(m.LocalTime.Weekday()) + 6) % 7
				grid[dow][m.LocalTime.Hour()]++
			}
		}
	}

	return db.HourOfWeekResponseFromGrid(grid), nil
}

// --- Session Shape ---

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

// autonomyBucket returns the bucket label for an autonomy
// ratio.
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

// sortBuckets sorts distribution buckets by defined order.
func sortBuckets(
	buckets []db.DistributionBucket,
	order map[string]int,
) {
	sort.Slice(buckets, func(i, j int) bool {
		return order[buckets[i].Label] <
			order[buckets[j].Label]
	})
}

// mapToBuckets converts a label->count map to sorted buckets.
func mapToBuckets(
	m map[string]int, order map[string]int,
) []db.DistributionBucket {
	buckets := make(
		[]db.DistributionBucket, 0, len(m),
	)
	for label, count := range m {
		buckets = append(buckets, db.DistributionBucket{
			Label: label, Count: count,
		})
	}
	sortBuckets(buckets, order)
	return buckets
}

// GetAnalyticsSessionShape returns distribution histograms
// for session length, duration, and autonomy ratio.
func (s *Store) GetAnalyticsSessionShape(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SessionShapeResponse, error) {
	loc := analyticsLocation(f)
	modelFilter := strings.TrimSpace(f.Model) != ""
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
	}

	query := `SELECT ` + pgDateCol + `,
		EXTRACT(EPOCH FROM ended_at - started_at)
			AS duration_sec,
		message_count, id FROM sessions WHERE ` + where

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.SessionShapeResponse{},
			fmt.Errorf(
				"querying session shape: %w", err,
			)
	}
	defer rows.Close()

	lengthCounts := make(map[string]int)
	durationCounts := make(map[string]int)
	var sessionIDs []string
	totalCount := 0

	for rows.Next() {
		var tsVal *time.Time
		var durationSec *float64
		var mc int
		var id string
		if err := rows.Scan(
			&tsVal, &durationSec, &mc, &id,
		); err != nil {
			return db.SessionShapeResponse{},
				fmt.Errorf(
					"scanning session shape row: %w",
					err,
				)
		}
		date := localDate(scanDateCol(tsVal), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}

		totalCount++
		if !modelFilter {
			lengthCounts[lengthBucket(mc)]++
		}
		sessionIDs = append(sessionIDs, id)

		if durationSec != nil && *durationSec >= 0 {
			mins := *durationSec / 60.0
			durationCounts[durationBucket(mins)]++
		}
	}
	if err := rows.Err(); err != nil {
		return db.SessionShapeResponse{},
			fmt.Errorf(
				"iterating session shape rows: %w",
				err,
			)
	}

	autonomyCounts := make(map[string]int)
	if modelFilter && len(sessionIDs) > 0 {
		stats, err := s.getAnalyticsFilteredMessageStats(
			ctx, sessionIDs, f,
		)
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
		lengthCounts = make(map[string]int)
		seen := make(map[string]struct{}, len(sessionIDs))
		for _, sessionID := range sessionIDs {
			if _, ok := seen[sessionID]; ok {
				continue
			}
			seen[sessionID] = struct{}{}
			stat := stats[sessionID]
			lengthCounts[lengthBucket(stat.Messages)]++
			if stat.UserMessages > 0 {
				ratio := float64(stat.ToolUseMessages) /
					float64(stat.UserMessages)
				autonomyCounts[autonomyBucket(ratio)]++
			}
		}
	} else if len(sessionIDs) > 0 {
		err := pgQueryChunked(sessionIDs,
			func(chunk []string) error {
				return s.queryAutonomyChunk(
					ctx, chunk, autonomyCounts,
				)
			})
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
	}

	return db.SessionShapeResponse{
		Count: totalCount,
		LengthDistribution: mapToBuckets(
			lengthCounts, lengthOrder,
		),
		DurationDistribution: mapToBuckets(
			durationCounts, durationOrder,
		),
		AutonomyDistribution: mapToBuckets(
			autonomyCounts, autonomyOrder,
		),
	}, nil
}

// queryAutonomyChunk queries autonomy stats for a chunk of
// session IDs.
func (s *Store) queryAutonomyChunk(
	ctx context.Context,
	chunk []string,
	counts map[string]int,
) error {
	pb := &paramBuilder{}
	ph := pgInPlaceholders(chunk, pb)
	q := `SELECT session_id,
		SUM(CASE WHEN role='user' AND is_system=false
			THEN 1 ELSE 0 END),
		SUM(CASE WHEN role='assistant'
			AND has_tool_use=true THEN 1 ELSE 0 END)
		FROM messages
		WHERE session_id IN ` + ph + `
		GROUP BY session_id`

	rows, err := s.pg.QueryContext(ctx, q, pb.args...)
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
			return fmt.Errorf(
				"scanning autonomy row: %w", err,
			)
		}
		if userCount > 0 {
			ratio := float64(toolCount) /
				float64(userCount)
			counts[autonomyBucket(ratio)]++
		}
	}
	return rows.Err()
}

// --- Tools ---

// GetAnalyticsTools returns tool usage analytics.
func (s *Store) GetAnalyticsTools(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ToolsAnalyticsResponse, error) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithoutDate(f, pb)

	sessQ := `SELECT id, ` + pgDateCol + `, agent
		FROM sessions WHERE ` + where

	sessRows, err := s.pg.QueryContext(
		ctx, sessQ, pb.args...,
	)
	if err != nil {
		return db.ToolsAnalyticsResponse{},
			fmt.Errorf(
				"querying tool sessions: %w", err,
			)
	}
	defer sessRows.Close()

	type sessInfo struct {
		ts    string
		agent string
	}
	sessionMap := make(map[string]sessInfo)
	var sessionIDs []string

	for sessRows.Next() {
		var id, agent string
		var ts *time.Time
		if err := sessRows.Scan(
			&id, &ts, &agent,
		); err != nil {
			return db.ToolsAnalyticsResponse{},
				fmt.Errorf(
					"scanning tool session: %w", err,
				)
		}
		sessionMap[id] = sessInfo{
			ts: scanDateCol(ts), agent: agent,
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := sessRows.Err(); err != nil {
		return db.ToolsAnalyticsResponse{},
			fmt.Errorf(
				"iterating tool sessions: %w", err,
			)
	}

	resp := db.ToolsAnalyticsResponse{
		ByCategory: []db.ToolCategoryCount{},
		ByAgent:    []db.ToolAgentBreakdown{},
		Trend:      []db.ToolTrendEntry{},
	}

	if len(sessionIDs) == 0 {
		return resp, nil
	}

	type toolRow struct {
		sessionID string
		category  string
		count     int
		date      string
	}
	var toolRows []toolRow

	err = pgQueryChunked(sessionIDs,
		func(chunk []string) error {
			chunkPB := &paramBuilder{}
			ph := pgInPlaceholders(chunk, chunkPB)
			preds := []string{"tc.session_id IN " + ph}
			preds = appendPGAnalyticsCSVFilter(
				preds, "m.model", f.Model, chunkPB,
			)
			msgTSExpr := `COALESCE(TO_CHAR(m.timestamp AT TIME ZONE 'UTC', ` +
				`'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')`
			q := `SELECT tc.session_id, tc.category, COUNT(*),
				` + msgTSExpr + `
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.ordinal = tc.message_ordinal`
			q += `
				WHERE ` + strings.Join(preds, " AND ") + `
				GROUP BY tc.session_id, tc.category, ` + msgTSExpr
			rows, qErr := s.pg.QueryContext(
				ctx, q, chunkPB.args...,
			)
			if qErr != nil {
				return fmt.Errorf(
					"querying tool_calls: %w", qErr,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var sid, cat, ts string
				var count int
				if err := rows.Scan(
					&sid, &cat, &count, &ts,
				); err != nil {
					return fmt.Errorf(
						"scanning tool_call: %w", err,
					)
				}
				info, ok := sessionMap[sid]
				if !ok {
					continue
				}
				_, date, keep := f.ResolveSkillRowTime(
					ts, info.ts,
				)
				if !keep {
					continue
				}
				toolRows = append(toolRows, toolRow{
					sessionID: sid,
					category:  cat,
					count:     count,
					date:      date,
				})
			}
			return rows.Err()
		})
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}

	if len(toolRows) == 0 {
		return resp, nil
	}

	catCounts := make(map[string]int)
	agentCats := make(map[string]map[string]int)
	trendBuckets := make(map[string]map[string]int)

	for _, tr := range toolRows {
		info := sessionMap[tr.sessionID]
		catCounts[tr.category] += tr.count

		if agentCats[info.agent] == nil {
			agentCats[info.agent] = make(map[string]int)
		}
		agentCats[info.agent][tr.category] += tr.count

		week := bucketDate(tr.date, "week")
		if trendBuckets[week] == nil {
			trendBuckets[week] = make(map[string]int)
		}
		trendBuckets[week][tr.category] += tr.count
	}

	for _, count := range catCounts {
		resp.TotalCalls += count
	}

	resp.ByCategory = make(
		[]db.ToolCategoryCount, 0, len(catCounts),
	)
	for cat, count := range catCounts {
		pct := math.Round(
			float64(count)/
				float64(resp.TotalCalls)*1000,
		) / 10
		resp.ByCategory = append(resp.ByCategory,
			db.ToolCategoryCount{
				Category: cat, Count: count, Pct: pct,
			})
	}
	sort.Slice(resp.ByCategory, func(i, j int) bool {
		if resp.ByCategory[i].Count !=
			resp.ByCategory[j].Count {
			return resp.ByCategory[i].Count >
				resp.ByCategory[j].Count
		}
		return resp.ByCategory[i].Category <
			resp.ByCategory[j].Category
	})

	agentKeys := make([]string, 0, len(agentCats))
	for k := range agentCats {
		agentKeys = append(agentKeys, k)
	}
	sort.Strings(agentKeys)
	resp.ByAgent = make(
		[]db.ToolAgentBreakdown, 0, len(agentKeys),
	)
	for _, agent := range agentKeys {
		cats := agentCats[agent]
		total := 0
		for _, c := range cats {
			total += c
		}
		catList := make(
			[]db.ToolCategoryCount, 0, len(cats),
		)
		for cat, count := range cats {
			pct := math.Round(
				float64(count)/float64(total)*1000,
			) / 10
			catList = append(catList, db.ToolCategoryCount{
				Category: cat, Count: count, Pct: pct,
			})
		}
		sort.Slice(catList, func(i, j int) bool {
			if catList[i].Count != catList[j].Count {
				return catList[i].Count > catList[j].Count
			}
			return catList[i].Category <
				catList[j].Category
		})
		resp.ByAgent = append(resp.ByAgent,
			db.ToolAgentBreakdown{
				Agent:      agent,
				Total:      total,
				Categories: catList,
			})
	}

	resp.Trend = make(
		[]db.ToolTrendEntry, 0, len(trendBuckets),
	)
	for week, cats := range trendBuckets {
		resp.Trend = append(resp.Trend, db.ToolTrendEntry{
			Date: week, ByCat: cats,
		})
	}
	sort.Slice(resp.Trend, func(i, j int) bool {
		return resp.Trend[i].Date < resp.Trend[j].Date
	})

	return resp, nil
}

// GetAnalyticsSkills returns skill usage analytics.
func (s *Store) GetAnalyticsSkills(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SkillsAnalyticsResponse, error) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithoutDate(f, pb)

	sessQ := `SELECT id, ` + pgDateCol + `, agent, project
		FROM sessions WHERE ` + where

	sessRows, err := s.pg.QueryContext(ctx, sessQ, pb.args...)
	if err != nil {
		return db.SkillsAnalyticsResponse{},
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
		var id, agent, project string
		var ts *time.Time
		if err := sessRows.Scan(
			&id, &ts, &agent, &project,
		); err != nil {
			return db.SkillsAnalyticsResponse{},
				fmt.Errorf("scanning skill session: %w", err)
		}
		sessionMap[id] = sessInfo{
			ts:      scanDateCol(ts),
			agent:   agent,
			project: project,
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := sessRows.Err(); err != nil {
		return db.SkillsAnalyticsResponse{},
			fmt.Errorf("iterating skill sessions: %w", err)
	}
	if len(sessionIDs) == 0 {
		return db.BuildSkillsAnalytics(nil), nil
	}

	var skillRows []db.SkillAnalyticsRow
	err = pgQueryChunked(sessionIDs,
		func(chunk []string) error {
			chunkPB := &paramBuilder{}
			ph := pgInPlaceholders(chunk, chunkPB)
			preds := []string{
				"tc.session_id IN " + ph,
				"TRIM(COALESCE(tc.skill_name, '')) != ''",
			}
			preds = appendPGAnalyticsCSVFilter(
				preds, "m.model", f.Model, chunkPB,
			)
			q := `SELECT tc.session_id,
					TRIM(COALESCE(tc.skill_name, '')),
					COUNT(*),
					m.timestamp
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.ordinal = tc.message_ordinal
				WHERE ` + strings.Join(preds, " AND ") + `
				GROUP BY tc.session_id,
					TRIM(COALESCE(tc.skill_name, '')),
					m.timestamp`
			rows, qErr := s.pg.QueryContext(
				ctx, q, chunkPB.args...,
			)
			if qErr != nil {
				return fmt.Errorf(
					"querying skill tool_calls: %w", qErr,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var sid, skill string
				var count int
				var lastTS *time.Time
				if err := rows.Scan(
					&sid, &skill, &count, &lastTS,
				); err != nil {
					return fmt.Errorf(
						"scanning skill tool_call: %w", err,
					)
				}
				info := sessionMap[sid]
				usedTS, date, keep := f.ResolveSkillRowTime(
					scanDateCol(lastTS), info.ts,
				)
				if !keep {
					continue
				}
				skillRows = append(skillRows, db.SkillAnalyticsRow{
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
		return db.SkillsAnalyticsResponse{}, err
	}

	return db.BuildSkillsAnalytics(skillRows), nil
}

// --- Velocity ---

// velocityMsg holds per-message data needed for velocity.
type velocityMsg struct {
	role          string
	ts            time.Time
	valid         bool
	contentLength int
}

// queryVelocityMsgs fetches messages for a chunk of session
// IDs and appends them to sessionMsgs.
func (s *Store) queryVelocityMsgs(
	ctx context.Context,
	chunk []string,
	loc *time.Location,
	sessionMsgs map[string][]velocityMsg,
) error {
	pb := &paramBuilder{}
	ph := pgInPlaceholders(chunk, pb)
	q := `SELECT session_id, ordinal, role,
		timestamp,
		content_length
		FROM messages
		WHERE session_id IN ` + ph + `
		ORDER BY session_id, ordinal`

	rows, err := s.pg.QueryContext(ctx, q, pb.args...)
	if err != nil {
		return fmt.Errorf(
			"querying velocity messages: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var ordinal int
		var role string
		var ts *time.Time
		var cl int
		if err := rows.Scan(
			&sid, &ordinal, &role, &ts, &cl,
		); err != nil {
			return fmt.Errorf(
				"scanning velocity msg: %w", err,
			)
		}
		t := time.Time{}
		ok := false
		if ts != nil {
			t = ts.In(loc)
			ok = true
		}
		sessionMsgs[sid] = append(sessionMsgs[sid],
			velocityMsg{
				role: role, ts: t, valid: ok,
				contentLength: cl,
			})
	}
	return rows.Err()
}

func (s *Store) getAnalyticsVelocityMessages(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string][]velocityMsg, error) {
	sessionMsgs := make(map[string][]velocityMsg, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return sessionMsgs, nil
	}

	loc := analyticsLocation(f)
	if strings.TrimSpace(f.Model) == "" {
		err := pgQueryChunked(sessionIDs, func(chunk []string) error {
			return s.queryVelocityMsgs(ctx, chunk, loc, sessionMsgs)
		})
		return sessionMsgs, err
	}

	scope, err := s.resolveAnalyticsMessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	if scope != nil {
		for sid, timings := range scope.TimingBySession() {
			for _, t := range timings {
				sessionMsgs[sid] = append(sessionMsgs[sid], velocityMsg{
					role: t.Role, ts: t.Time, valid: t.Valid,
					contentLength: t.ContentLength,
				})
			}
		}
	}

	return sessionMsgs, nil
}

// complexityBucket returns the complexity label.
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

// velocityAccumulator collects raw values for a velocity
// group.
type velocityAccumulator struct {
	turnCycles     []float64
	firstResponses []float64
	totalMsgs      int
	totalChars     int
	totalToolCalls int
	activeMinutes  float64
	sessions       int
}

func (a *velocityAccumulator) computeOverview() db.VelocityOverview {
	sort.Float64s(a.turnCycles)
	sort.Float64s(a.firstResponses)

	var v db.VelocityOverview
	v.TurnCycleSec = db.Percentiles{
		P50: math.Round(
			percentileFloat(a.turnCycles, 0.5)*10) / 10,
		P90: math.Round(
			percentileFloat(a.turnCycles, 0.9)*10) / 10,
	}
	v.FirstResponseSec = db.Percentiles{
		P50: math.Round(
			percentileFloat(
				a.firstResponses, 0.5)*10) / 10,
		P90: math.Round(
			percentileFloat(
				a.firstResponses, 0.9)*10) / 10,
	}
	if a.activeMinutes > 0 {
		v.MsgsPerActiveMin = math.Round(
			float64(a.totalMsgs)/
				a.activeMinutes*10) / 10
		v.CharsPerActiveMin = math.Round(
			float64(a.totalChars)/
				a.activeMinutes*10) / 10
		v.ToolCallsPerActiveMin = math.Round(
			float64(a.totalToolCalls)/
				a.activeMinutes*10) / 10
	}
	return v
}

// GetAnalyticsVelocity computes turn cycle, first response,
// and throughput metrics.
func (s *Store) GetAnalyticsVelocity(
	ctx context.Context, f db.AnalyticsFilter,
) (db.VelocityResponse, error) {
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.VelocityResponse{}, err
		}
	}

	sessQuery := `SELECT id, ` + pgDateCol + `, agent,
		message_count FROM sessions WHERE ` + where

	sessRows, err := s.pg.QueryContext(
		ctx, sessQuery, pb.args...,
	)
	if err != nil {
		return db.VelocityResponse{},
			fmt.Errorf(
				"querying velocity sessions: %w", err,
			)
	}
	defer sessRows.Close()

	type sessInfo struct {
		agent string
		mc    int
	}
	sessionMap := make(map[string]sessInfo)
	var sessionIDs []string

	for sessRows.Next() {
		var id, agent string
		var ts *time.Time
		var mc int
		if err := sessRows.Scan(
			&id, &ts, &agent, &mc,
		); err != nil {
			return db.VelocityResponse{},
				fmt.Errorf(
					"scanning velocity session: %w",
					err,
				)
		}
		date := localDate(scanDateCol(ts), loc)
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
		return db.VelocityResponse{},
			fmt.Errorf(
				"iterating velocity sessions: %w", err,
			)
	}

	if len(sessionIDs) == 0 {
		return db.VelocityResponse{
			ByAgent:      []db.VelocityBreakdown{},
			ByComplexity: []db.VelocityBreakdown{},
		}, nil
	}
	if strings.TrimSpace(f.Model) != "" {
		stats, err := s.getAnalyticsFilteredMessageStats(
			ctx, sessionIDs, f,
		)
		if err != nil {
			return db.VelocityResponse{}, err
		}
		for _, sid := range sessionIDs {
			info := sessionMap[sid]
			info.mc = stats[sid].Messages
			sessionMap[sid] = info
		}
	}

	sessionMsgs, err := s.getAnalyticsVelocityMessages(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.VelocityResponse{}, err
	}

	var toolCountMap map[string]int
	if strings.TrimSpace(f.Model) != "" {
		toolCountMap, err = s.getModelScopedToolCallCounts(
			ctx, sessionIDs, f,
		)
		if err != nil {
			return db.VelocityResponse{}, err
		}
	} else {
		toolCountMap = make(map[string]int)
		err = pgQueryChunked(sessionIDs,
			func(chunk []string) error {
				chunkPB := &paramBuilder{}
				ph := pgInPlaceholders(chunk, chunkPB)
				q := `SELECT session_id, COUNT(*)
					FROM tool_calls
					WHERE session_id IN ` + ph + `
					GROUP BY session_id`
				rows, qErr := s.pg.QueryContext(
					ctx, q, chunkPB.args...,
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
					if err := rows.Scan(
						&sid, &count,
					); err != nil {
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
			return db.VelocityResponse{}, err
		}
	}

	overall := &velocityAccumulator{}
	byAgent := make(map[string]*velocityAccumulator)
	byComplexity := make(map[string]*velocityAccumulator)

	const maxCycleSec = 1800.0
	// Shared with the Top Sessions "active duration" SQL so the two
	// "active" definitions stay in lockstep.
	const maxGapSec = db.ActiveGapCapSec

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
			overall,
			byAgent[agentKey],
			byComplexity[compKey],
		}

		for _, a := range accums {
			a.sessions++
		}

		for i := 1; i < len(msgs); i++ {
			prev := msgs[i-1]
			cur := msgs[i]
			if !prev.valid || !cur.valid {
				continue
			}
			if prev.role == "user" &&
				cur.role == "assistant" {
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
			delta := firstAsst.ts.Sub(
				firstUser.ts,
			).Seconds()
			if delta < 0 {
				delta = 0
			}
			for _, a := range accums {
				a.firstResponses = append(
					a.firstResponses, delta,
				)
			}
		}

		activeSec := 0.0
		asstChars := 0
		for i, m := range msgs {
			if m.role == "assistant" {
				asstChars += m.contentLength
			}
			if i > 0 && msgs[i-1].valid && m.valid {
				gap := m.ts.Sub(
					msgs[i-1].ts,
				).Seconds()
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

	resp := db.VelocityResponse{
		Overall: overall.computeOverview(),
	}

	agentKeys := make([]string, 0, len(byAgent))
	for k := range byAgent {
		agentKeys = append(agentKeys, k)
	}
	sort.Strings(agentKeys)
	resp.ByAgent = make(
		[]db.VelocityBreakdown, 0, len(agentKeys),
	)
	for _, k := range agentKeys {
		a, ok := byAgent[k]
		if !ok || a == nil {
			continue
		}
		resp.ByAgent = append(resp.ByAgent,
			db.VelocityBreakdown{
				Label:    k,
				Sessions: a.sessions,
				Overview: a.computeOverview(),
			})
	}

	compOrder := map[string]int{
		"1-15": 0, "16-60": 1, "61+": 2,
	}
	compKeys := make([]string, 0, len(byComplexity))
	for k := range byComplexity {
		compKeys = append(compKeys, k)
	}
	sort.Slice(compKeys, func(i, j int) bool {
		return compOrder[compKeys[i]] <
			compOrder[compKeys[j]]
	})
	resp.ByComplexity = make(
		[]db.VelocityBreakdown, 0, len(compKeys),
	)
	for _, k := range compKeys {
		a, ok := byComplexity[k]
		if !ok || a == nil {
			continue
		}
		resp.ByComplexity = append(resp.ByComplexity,
			db.VelocityBreakdown{
				Label:    k,
				Sessions: a.sessions,
				Overview: a.computeOverview(),
			})
	}

	return resp, nil
}

// --- Top Sessions ---

// GetAnalyticsTopSessions returns the top 10 sessions by the
// given metric.
func (s *Store) GetAnalyticsTopSessions(
	ctx context.Context, f db.AnalyticsFilter,
	metric string,
) (db.TopSessionsResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
	}

	needsGoSort := metric == "duration"
	needsFilteredMessageSort := strings.TrimSpace(f.Model) != "" &&
		(metric == "messages" || metric == "output_tokens")
	orderExpr := "message_count DESC, id ASC"
	switch metric {
	case "output_tokens":
		if strings.TrimSpace(f.Model) == "" {
			where += " AND has_total_output_tokens = TRUE"
		}
		orderExpr = "total_output_tokens DESC, id ASC"
	case "duration":
		where += " AND started_at IS NOT NULL" +
			" AND ended_at IS NOT NULL" +
			" AND ended_at >= started_at"
	default:
		metric = "messages"
	}

	limitClause := " LIMIT 1000"
	if f.HasTimeFilter() || needsGoSort || needsFilteredMessageSort {
		limitClause = ""
	}
	activeDurationSelectExpr := fmt.Sprintf(`COALESCE(
		  (
			SELECT COALESCE(SUM(
				CASE
					WHEN inner2.delta_ms <= 0 THEN 0
					WHEN inner2.delta_ms > %[1]d THEN %[1]d
					ELSE inner2.delta_ms
				END), 0) / 60000.0
			FROM (
				SELECT CAST(
					round(EXTRACT(EPOCH FROM (
						LEAD(m2.timestamp) OVER (ORDER BY m2.ordinal)
						- m2.timestamp
					)) * 1000)
				AS BIGINT) AS delta_ms
				FROM messages m2
				WHERE m2.session_id = sessions.id
			) inner2
		), 0)`, db.ActiveGapCapMs)

	query := `SELECT id, ` + pgDateCol + `, project,
		first_message,
		COALESCE(display_name, session_name) AS display_name,
		message_count, total_output_tokens,
		COALESCE(EXTRACT(EPOCH FROM ended_at - started_at), 0)
			AS duration_sec,
		` + activeDurationSelectExpr + ` AS active_duration_min,
		started_at, ended_at,
		termination_status
		FROM sessions WHERE ` + where +
		` ORDER BY ` + orderExpr + limitClause

	rows, err := s.pg.QueryContext(
		ctx, query, pb.args...,
	)
	if err != nil {
		return db.TopSessionsResponse{},
			fmt.Errorf(
				"querying top sessions: %w", err,
			)
	}
	defer rows.Close()

	sessions := []db.TopSession{}
	for rows.Next() {
		var id, project string
		var ts *time.Time
		var startedAt, endedAt *time.Time
		var firstMsg, displayName, termStatus *string
		var mc, outputTokens int
		var durationSec, activeDurationMin float64
		if err := rows.Scan(
			&id, &ts, &project, &firstMsg,
			&displayName, &mc, &outputTokens,
			&durationSec, &activeDurationMin, &startedAt, &endedAt,
			&termStatus,
		); err != nil {
			return db.TopSessionsResponse{},
				fmt.Errorf(
					"scanning top session: %w", err,
				)
		}
		date := localDate(scanDateCol(ts), loc)
		if !inDateRange(date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[id] {
			continue
		}
		durMin := durationSec / 60.0
		var startedStr, endedStr *string
		if startedAt != nil {
			s := FormatISO8601(*startedAt)
			startedStr = &s
		}
		if endedAt != nil {
			s := FormatISO8601(*endedAt)
			endedStr = &s
		}
		sessions = append(sessions, db.TopSession{
			ID:                id,
			Project:           project,
			FirstMessage:      firstMsg,
			DisplayName:       displayName,
			MessageCount:      mc,
			OutputTokens:      outputTokens,
			DurationMin:       durMin,
			ActiveDurationMin: activeDurationMin,
			StartedAt:         startedStr,
			EndedAt:           endedStr,
			TerminationStatus: termStatus,
		})
	}
	if err := rows.Err(); err != nil {
		return db.TopSessionsResponse{},
			fmt.Errorf(
				"iterating top sessions: %w", err,
			)
	}

	if needsFilteredMessageSort {
		sessionIDs := make([]string, 0, len(sessions))
		for _, session := range sessions {
			sessionIDs = append(sessionIDs, session.ID)
		}
		stats, err := s.getAnalyticsFilteredMessageStats(ctx, sessionIDs, f)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
		filtered := sessions[:0]
		for i := range sessions {
			stat := stats[sessions[i].ID]
			sessions[i].MessageCount = stat.Messages
			sessions[i].OutputTokens = stat.OutputTokens
			if metric == "output_tokens" && !stat.HasOutputTokens {
				continue
			}
			filtered = append(filtered, sessions[i])
		}
		sessions = filtered
		sort.SliceStable(sessions, func(i, j int) bool {
			if metric == "output_tokens" {
				if sessions[i].OutputTokens != sessions[j].OutputTokens {
					return sessions[i].OutputTokens >
						sessions[j].OutputTokens
				}
			} else if sessions[i].MessageCount != sessions[j].MessageCount {
				return sessions[i].MessageCount >
					sessions[j].MessageCount
			}
			return sessions[i].ID < sessions[j].ID
		})
	}

	sessions = rankTopSessions(sessions, needsGoSort)

	return db.TopSessionsResponse{
		Metric:   metric,
		Sessions: sessions,
	}, nil
}

// GetAnalyticsSignals returns aggregated session signal data.
// Mirrors the SQLite implementation: select per-session signal
// columns, apply analytics filters, then hand the rows to the
// shared db.AggregateSignals so the response shape stays
// identical across stores. Signals stay session-scoped under a
// model filter (totals are session-level aggregates over sessions
// that used the model, not re-attributed per model); see the
// SQLite GetAnalyticsSignals for the rationale.
func (s *Store) GetAnalyticsSignals(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SignalsAnalyticsResponse, error) {
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)

	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return db.SignalsAnalyticsResponse{}, err
		}
	}

	query := `SELECT id, agent, project, first_message, is_automated,
		` + pgDateCol + `,
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

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.SignalsAnalyticsResponse{}, fmt.Errorf(
			"querying analytics signals: %w", err,
		)
	}
	defer rows.Close()

	var all []db.SignalRow
	for rows.Next() {
		var (
			r  db.SignalRow
			ts *time.Time
		)
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
			return db.SignalsAnalyticsResponse{}, fmt.Errorf(
				"scanning signals row: %w", err,
			)
		}
		r.Date = localDate(scanDateCol(ts), loc)
		if !inDateRange(r.Date, f.From, f.To) {
			continue
		}
		if timeIDs != nil && !timeIDs[r.ID] {
			continue
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return db.SignalsAnalyticsResponse{}, fmt.Errorf(
			"iterating signals rows: %w", err,
		)
	}
	if err := s.populateFrustrationMarkers(ctx, all); err != nil {
		return db.SignalsAnalyticsResponse{}, err
	}

	return db.AggregateSignals(all), nil
}

func (s *Store) GetAnalyticsSignalSessions(
	ctx context.Context,
	f db.AnalyticsFilter,
	signal string,
	limit int,
) (db.SignalSessionsResponse, error) {
	if !db.IsSupportedAnalyticsSignal(signal) {
		return db.SignalSessionsResponse{},
			db.ErrUnsupportedAnalyticsSignal
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	rows, err := s.signalRows(ctx, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	if err := s.populateFrustrationMarkers(ctx, rows); err != nil {
		return db.SignalSessionsResponse{}, err
	}
	candidates := db.SignalCandidates(rows, signal, limit)
	messages, err := s.signalMessages(ctx, candidates, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	return db.SignalSessionsResponse{
		Signal:   signal,
		Sessions: db.BuildSignalExamples(candidates, messages, signal),
	}, nil
}

func (s *Store) signalRows(
	ctx context.Context,
	f db.AnalyticsFilter,
) ([]db.SignalRow, error) {
	loc := analyticsLocation(f)
	pb := &paramBuilder{}
	where := buildAnalyticsWhere(f, pgDateCol, pb)
	var timeIDs map[string]bool
	if f.HasTimeFilter() {
		var err error
		timeIDs, err = s.filteredSessionIDs(ctx, f)
		if err != nil {
			return nil, err
		}
	}
	query := `SELECT id, agent, project, first_message, is_automated,
		` + pgDateCol + `,
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
	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying analytics signal rows: %w", err,
		)
	}
	defer rows.Close()
	var all []db.SignalRow
	for rows.Next() {
		r, err := scanPGSignalRow(rows, loc)
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

func scanPGSignalRow(
	rows interface{ Scan(dest ...any) error },
	loc *time.Location,
) (db.SignalRow, error) {
	var (
		r  db.SignalRow
		ts *time.Time
	)
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
		return db.SignalRow{}, fmt.Errorf(
			"scanning signal row: %w", err,
		)
	}
	r.Date = localDate(scanDateCol(ts), loc)
	return r, nil
}

func (s *Store) signalMessages(
	ctx context.Context,
	rows []db.SignalRow,
	f db.AnalyticsFilter,
) (map[string][]db.SignalMessage, error) {
	out := make(map[string][]db.SignalMessage, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if strings.TrimSpace(f.Model) != "" {
		rowsBySession, err := s.getAnalyticsModelScopedMessages(ctx, ids, f)
		if err != nil {
			return nil, err
		}
		for sessionID, scopedRows := range rowsBySession {
			for _, row := range scopedRows {
				out[sessionID] = append(out[sessionID], db.SignalMessage{
					SessionID:  row.SessionID,
					Ordinal:    row.Ordinal,
					Role:       row.Role,
					Content:    row.Content,
					Timestamp:  row.Timestamp,
					IsSystem:   row.IsSystem,
					HasToolUse: row.HasToolUse,
				})
			}
		}
		return out, nil
	}
	filterModels := csvFilterValues(f.Model)
	err := pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		placeholders := make([]string, 0, len(chunk))
		for _, id := range chunk {
			placeholders = append(placeholders, pb.add(id))
		}
		q := `SELECT session_id, ordinal, role, content,
					COALESCE(to_char(timestamp AT TIME ZONE 'UTC',
						'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''),
					is_system, has_tool_use
				FROM messages
				WHERE session_id IN (` + strings.Join(placeholders, ",") + `)`
		if len(filterModels) == 1 {
			q += ` AND model = ` + pb.add(filterModels[0])
		} else if len(filterModels) > 1 {
			modelPlaceholders := make([]string, 0, len(filterModels))
			for _, model := range filterModels {
				modelPlaceholders = append(
					modelPlaceholders,
					pb.add(model),
				)
			}
			q += ` AND model IN (` + strings.Join(modelPlaceholders, ",") + `)`
		}
		q += `
				ORDER BY session_id, ordinal`
		msgRows, err := s.pg.QueryContext(ctx, q, pb.args...)
		if err != nil {
			return fmt.Errorf(
				"querying signal messages: %w", err,
			)
		}
		defer msgRows.Close()
		for msgRows.Next() {
			var m db.SignalMessage
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

func (s *Store) populateFrustrationMarkers(
	ctx context.Context,
	rows []db.SignalRow,
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
	return pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		placeholders := make([]string, 0, len(chunk))
		for _, id := range chunk {
			placeholders = append(placeholders, pb.add(id))
		}
		q := `SELECT session_id, ordinal, content, is_system
			FROM messages
			WHERE role = 'user'
			  AND session_id IN (` + strings.Join(placeholders, ",") + `)`
		msgRows, err := s.pg.QueryContext(ctx, q, pb.args...)
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

// rankTopSessions sorts sessions by active duration (if
// needsGoSort), truncates to top 10, and rounds duration fields.
func rankTopSessions(
	sessions []db.TopSession, needsGoSort bool,
) []db.TopSession {
	if sessions == nil {
		return []db.TopSession{}
	}
	if needsGoSort && len(sessions) > 1 {
		sort.SliceStable(sessions, func(i, j int) bool {
			if sessions[i].ActiveDurationMin !=
				sessions[j].ActiveDurationMin {
				return sessions[i].ActiveDurationMin >
					sessions[j].ActiveDurationMin
			}
			return sessions[i].ID < sessions[j].ID
		})
	}
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	for i := range sessions {
		sessions[i].DurationMin = math.Round(
			sessions[i].DurationMin*10) / 10
		sessions[i].ActiveDurationMin = math.Round(
			sessions[i].ActiveDurationMin*10) / 10
	}
	return sessions
}
