// internal/db/session_stats.go
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/wesm/agentsview/internal/db/git"
)

// StatsFilter mirrors the service-layer StatsFilter but lives in db
// because db functions take typed filters without cross-package deps.
type StatsFilter struct {
	Since           string
	Until           string
	Agent           string
	IncludeProjects []string
	ExcludeProjects []string
	Timezone        string
	GHToken         string
}

// GetSessionStats computes the v1 session-stats JSON response.
// Sections are populated in order so each step can reuse the per-session
// rows (and derived sessionIDs) loaded once by loadSessionsInWindow.
func (db *DB) GetSessionStats(
	ctx context.Context, f StatsFilter,
) (*SessionStats, error) {
	tz, err := resolveTimezone(f.Timezone)
	if err != nil {
		return nil, fmt.Errorf("resolving timezone: %w", err)
	}
	from, to, days, err := windowBounds(f, time.Now())
	if err != nil {
		return nil, fmt.Errorf("resolving window: %w", err)
	}

	rows, err := db.loadSessionsInWindow(ctx, f, from, to)
	if err != nil {
		return nil, err
	}

	stats := &SessionStats{
		SchemaVersion: 1,
		Window: StatsWindow{
			Since: from.UTC().Format(time.RFC3339),
			Until: to.UTC().Format(time.RFC3339),
			Days:  days,
		},
		Filters: StatsFilters{
			Agent:            orDefault(f.Agent, "all"),
			ProjectsIncluded: f.IncludeProjects,
			ProjectsExcluded: nonNilSlice(f.ExcludeProjects),
			Timezone:         tz.String(),
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	computeTotalsAndArchetypes(stats, rows)
	computeDistributions(stats, rows)

	sessionIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		sessionIDs = append(sessionIDs, r.id)
	}
	accum, err := populateVelocityAccumulator(ctx, db, sessionIDs, tz)
	if err != nil {
		return nil, fmt.Errorf("populating velocity accumulator: %w", err)
	}
	computeVelocity(stats, accum)

	if err := db.computeToolAndModelMix(
		ctx, stats, sessionIDs,
	); err != nil {
		return nil, fmt.Errorf(
			"computing tool/model mix: %w", err,
		)
	}

	computeAgentPortfolio(stats, rows)

	if err := db.computeCacheEconomics(ctx, stats, rows); err != nil {
		return nil, fmt.Errorf(
			"computing cache economics: %w", err,
		)
	}

	if err := db.computeTemporal(
		ctx, stats, f, from, to, sessionIDs,
	); err != nil {
		return nil, fmt.Errorf("computing temporal: %w", err)
	}

	computeOutcomes(stats, rows)

	if err := db.computeAdoption(ctx, stats, rows); err != nil {
		return nil, fmt.Errorf("computing adoption: %w", err)
	}

	if err := db.computeOutcomeStats(ctx, stats, f, from, to, rows); err != nil {
		return nil, fmt.Errorf("computing outcome stats: %w", err)
	}

	return stats, nil
}

// computeToolAndModelMix fills stats.ToolMix and stats.ModelMix from
// tool_calls and messages attached to sessionIDs. The session-level
// window and agent/project filters are already applied in
// loadSessionsInWindow — restricting to sessionIDs inherits those
// predicates without re-running the WHERE clause.
//
// Both mix maps are always non-nil so the JSON output keeps stable
// keys when the window contains no sessions.
func (db *DB) computeToolAndModelMix(
	ctx context.Context, stats *SessionStats, sessionIDs []string,
) error {
	stats.ToolMix.ByCategory = map[string]int{}
	stats.ModelMix.ByTokens = map[string]int64{}
	if len(sessionIDs) == 0 {
		return nil
	}

	if err := queryChunked(sessionIDs,
		func(chunk []string) error {
			return db.accumulateToolMix(ctx, stats, chunk)
		}); err != nil {
		return err
	}

	return queryChunked(sessionIDs,
		func(chunk []string) error {
			return db.accumulateModelMix(ctx, stats, chunk)
		})
}

// accumulateToolMix folds one chunk of session IDs into
// stats.ToolMix. Each row in tool_calls increments the matching
// category bucket and the total counter; empty-string categories are
// silently grouped under "" so the total stays consistent with
// GetAnalyticsTools.
func (db *DB) accumulateToolMix(
	ctx context.Context, stats *SessionStats, sessionIDs []string,
) error {
	ph, args := inPlaceholders(sessionIDs)
	q := `SELECT category, COUNT(*)
		FROM tool_calls
		WHERE session_id IN ` + ph + `
		GROUP BY category`
	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying tool_calls mix: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var category string
		var count int
		if err := rows.Scan(&category, &count); err != nil {
			return fmt.Errorf("scanning tool_calls mix: %w", err)
		}
		stats.ToolMix.ByCategory[category] += count
		stats.ToolMix.TotalCalls += count
	}
	return rows.Err()
}

// accumulateModelMix folds one chunk of session IDs into
// stats.ModelMix. Token contribution is messages.output_tokens summed
// per model — the per-message cost column, matching the spec's
// "model_mix.by_tokens reflects total output tokens per model".
//
// Eligibility mirrors usageMessageEligibility (internal/db/usage.go):
// rows without parsed token_usage and rows tagged as "<synthetic>" are
// excluded so model_mix never disagrees with the dollar/usage views.
// Messages with zero output_tokens are also dropped since they cannot
// move the by_tokens distribution.
func (db *DB) accumulateModelMix(
	ctx context.Context, stats *SessionStats, sessionIDs []string,
) error {
	ph, args := inPlaceholders(sessionIDs)
	q := `SELECT model, COALESCE(SUM(output_tokens), 0)
		FROM messages
		WHERE session_id IN ` + ph + `
			AND token_usage != ''
			AND model != ''
			AND model != '<synthetic>'
		GROUP BY model`
	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying model mix: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var total int64
		if err := rows.Scan(&model, &total); err != nil {
			return fmt.Errorf("scanning model mix: %w", err)
		}
		if total == 0 {
			continue
		}
		stats.ModelMix.ByTokens[model] += total
	}
	return rows.Err()
}

// computeVelocity fills SessionStats.Velocity from an already-populated
// accumulator. The mean fields are computed over the same turnCycles
// and firstResponses samples as the percentiles, so the two move
// together — no extra filtering, no hidden sample drift.
func computeVelocity(s *SessionStats, accum *velocityAccumulator) {
	ov := accum.computeOverview()
	s.Velocity.TurnCycleSeconds = StatsPercentiles{
		P50:  ov.TurnCycleSec.P50,
		P90:  ov.TurnCycleSec.P90,
		Mean: accum.turnCycleMean(),
	}
	s.Velocity.FirstResponseSeconds = StatsPercentiles{
		P50:  ov.FirstResponseSec.P50,
		P90:  ov.FirstResponseSec.P90,
		Mean: accum.firstResponseMean(),
	}
	if accum.activeMinutes > 0 {
		s.Velocity.MessagesPerActiveHour =
			float64(accum.totalMsgs) / (accum.activeMinutes / 60.0)
	}
}

// resolveTimezone loads an IANA zone name, defaulting to UTC when
// empty. Unknown zones are an error — silently falling back would
// hide typos in user input.
func resolveTimezone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf(
			"loading timezone %q: %w", name, err,
		)
	}
	return loc, nil
}

// windowBounds resolves Since/Until into absolute time bounds.
// Supported inputs: "Nd" (days), "Nh" (hours), or "YYYY-MM-DD".
// Until defaults to now; Since defaults to 28 days before Until.
// Returned days is the calendar-style span in whole days, rounded
// up when Since is a non-integer-day duration (e.g. "48h" → 2).
func windowBounds(
	f StatsFilter, now time.Time,
) (from, to time.Time, days int, err error) {
	to = now
	if f.Until != "" {
		to, err = parseWindowPoint(f.Until, now)
		if err != nil {
			return time.Time{}, time.Time{}, 0,
				fmt.Errorf("parsing until %q: %w", f.Until, err)
		}
	}

	from = to.Add(-28 * 24 * time.Hour)
	if f.Since != "" {
		// Durations anchor relative to Until; dates stand alone.
		if d, ok := parseDurationShort(f.Since); ok {
			from = to.Add(-d)
		} else {
			from, err = parseWindowPoint(f.Since, now)
			if err != nil {
				return time.Time{}, time.Time{}, 0,
					fmt.Errorf(
						"parsing since %q: %w",
						f.Since, err,
					)
			}
		}
	}

	if !from.Before(to) {
		return time.Time{}, time.Time{}, 0, fmt.Errorf(
			"window since (%s) must precede until (%s)",
			from.Format(time.RFC3339),
			to.Format(time.RFC3339),
		)
	}

	span := to.Sub(from)
	days = int(span / (24 * time.Hour))
	if span%(24*time.Hour) != 0 {
		days++
	}
	return from, to, days, nil
}

// parseWindowPoint accepts either a duration-relative-to-now form
// ("28d", "12h") or an absolute YYYY-MM-DD date (interpreted as
// the start of that UTC day). Used by Since and Until.
func parseWindowPoint(s string, now time.Time) (time.Time, error) {
	if d, ok := parseDurationShort(s); ok {
		return now.Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf(
		"expected Nd, Nh, or YYYY-MM-DD, got %q", s,
	)
}

// parseDurationShort recognises the compact "Nd" / "Nh" forms the
// stats CLI advertises. Returns ok=false when s is not a compact
// duration so callers can try the date path.
func parseDurationShort(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	num, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || num <= 0 {
		return 0, false
	}
	switch unit {
	case 'd':
		return time.Duration(num) * 24 * time.Hour, true
	case 'h':
		return time.Duration(num) * time.Hour, true
	default:
		return 0, false
	}
}

// sessionStatsRow is the compact per-session projection used by all
// stats sections. Only the columns this task reads are populated;
// later tasks extend the struct (and loadSessionsInWindow's SELECT)
// in place rather than duplicating the scan.
type sessionStatsRow struct {
	id                   string
	agent                string
	project              string
	startedAt            time.Time
	endedAt              sql.NullTime
	messageCount         int
	userMessageCount     int
	totalOutputTokens    int64
	hasTotalOutputTokens bool
	peakContextTokens    int64
	hasPeakContext       bool
	totalToolCalls       int
	assistantTurns       int
	// Outcome-section fields. Populated from the sessions table via
	// loadSessionsInWindow; consumed by computeOutcomes. Empty strings
	// for outcome/healthGrade denote "no signal recorded yet".
	outcome         string
	healthGrade     string
	toolRetryCount  int
	compactionCount int
	editChurnCount  int
	// cwd is the working directory recorded on the session. Consumed by
	// computeOutcomeStats to resolve enclosing git repositories; empty
	// string indicates the session had no recorded cwd and is skipped.
	cwd string
}

// loadSessionsInWindow returns the rows the stats pipeline needs.
// Matches the analytics.go convention: exclude subagent/fork rows
// and soft-deleted rows, require non-empty message_count, and bound
// by started_at within [from, to).
func (db *DB) loadSessionsInWindow(
	ctx context.Context, f StatsFilter, from, to time.Time,
) ([]sessionStatsRow, error) {
	// Use the same COALESCE(NULLIF(started_at, ''), created_at)
	// expression as the rest of the analytics code so sessions whose
	// started_at is missing (parser couldn't infer a start time) are
	// still attributed to the window via their created_at fallback.
	preds := []string{
		"message_count > 0",
		"relationship_type NOT IN ('subagent', 'fork')",
		"deleted_at IS NULL",
		"COALESCE(NULLIF(started_at, ''), created_at) >= ?",
		"COALESCE(NULLIF(started_at, ''), created_at) < ?",
	}
	args := []any{
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	}

	if f.Agent != "" {
		agents := strings.Split(f.Agent, ",")
		if len(agents) == 1 {
			preds = append(preds, "agent = ?")
			args = append(args, agents[0])
		} else {
			ph := make([]string, len(agents))
			for i, a := range agents {
				ph[i] = "?"
				args = append(args, a)
			}
			preds = append(preds,
				"agent IN ("+strings.Join(ph, ",")+")")
		}
	}

	if len(f.IncludeProjects) > 0 {
		ph, inArgs := inPlaceholders(f.IncludeProjects)
		preds = append(preds, "project IN "+ph)
		args = append(args, inArgs...)
	}
	if len(f.ExcludeProjects) > 0 {
		ph, inArgs := inPlaceholders(f.ExcludeProjects)
		preds = append(preds, "project NOT IN "+ph)
		args = append(args, inArgs...)
	}

	// The tool-call / assistant-turn subqueries keep the per-session
	// projection self-contained: one row per session, no separate
	// merge step. Correlated subqueries are cheap here because
	// idx_tool_calls_session and idx_messages_session_role already
	// narrow the scan to the session's rows.
	// Project the started_at the rest of the pipeline reads (with
	// the created_at fallback baked in) so downstream code never has
	// to revisit the COALESCE. assistant_turns excludes system rows
	// (Claude compact-boundary summaries, etc.) so they don't inflate
	// the denominator of the tools-per-turn distribution.
	// has_total_output_tokens is projected so agent_portfolio's
	// by_tokens accumulator can guard against zeroed-out token rows.
	query := `SELECT s.id, s.agent, s.project,
		COALESCE(NULLIF(s.started_at, ''), s.created_at) AS effective_started_at,
		s.ended_at,
		s.message_count, s.user_message_count,
		s.total_output_tokens, s.has_total_output_tokens,
		s.peak_context_tokens, s.has_peak_context_tokens,
		COALESCE((SELECT COUNT(*) FROM tool_calls tc
			WHERE tc.session_id = s.id), 0) AS total_tool_calls,
		COALESCE((SELECT COUNT(*) FROM messages m
			WHERE m.session_id = s.id
				AND m.role = 'assistant'
				AND m.is_system = 0),
			0) AS assistant_turns,
		s.outcome, COALESCE(s.health_grade, ''),
		s.tool_retry_count, s.compaction_count, s.edit_churn_count,
		COALESCE(s.cwd, '')
		FROM sessions s WHERE ` + strings.Join(preds, " AND ")

	sqlRows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying sessions for stats window: %w", err,
		)
	}
	defer sqlRows.Close()

	var out []sessionStatsRow
	for sqlRows.Next() {
		var r sessionStatsRow
		var startedAt string
		var endedAt sql.NullString
		var hasTotalTokens, hasPeak int
		if err := sqlRows.Scan(
			&r.id, &r.agent, &r.project,
			&startedAt, &endedAt,
			&r.messageCount, &r.userMessageCount,
			&r.totalOutputTokens, &hasTotalTokens,
			&r.peakContextTokens, &hasPeak,
			&r.totalToolCalls, &r.assistantTurns,
			&r.outcome, &r.healthGrade,
			&r.toolRetryCount, &r.compactionCount, &r.editChurnCount,
			&r.cwd,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning session stats row: %w", err,
			)
		}
		t, err := parseTimestamp(startedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"session %s: parsing started_at %q: %w",
				r.id, startedAt, err,
			)
		}
		r.startedAt = t
		if endedAt.Valid && endedAt.String != "" {
			et, err := parseTimestamp(endedAt.String)
			if err != nil {
				return nil, fmt.Errorf(
					"session %s: parsing ended_at %q: %w",
					r.id, endedAt.String, err,
				)
			}
			r.endedAt = sql.NullTime{Time: et, Valid: true}
		}
		r.hasTotalOutputTokens = hasTotalTokens == 1
		r.hasPeakContext = hasPeak == 1
		out = append(out, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating session stats rows: %w", err,
		)
	}
	return out, nil
}

// parseTimestamp accepts RFC3339 and RFC3339Nano — the two forms
// the session table writes via timeutil.Format / Ptr.
func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// archetypeLabel classifies a session by its user_message_count per
// the session-analytics v1 spec. Boundaries are inclusive on both
// sides of each band.
func archetypeLabel(userMsgs int) string {
	switch {
	case userMsgs <= 1:
		return "automation"
	case userMsgs <= 5:
		return "quick"
	case userMsgs <= 15:
		return "standard"
	case userMsgs <= 50:
		return "deep"
	default:
		return "marathon"
	}
}

// computeTotalsAndArchetypes fills SessionStats.Totals and
// .Archetypes in a single pass over rows.
func computeTotalsAndArchetypes(
	s *SessionStats, rows []sessionStatsRow,
) {
	archMax := map[string]int{}
	humanMax := map[string]int{}
	for _, r := range rows {
		s.Totals.SessionsAll++
		s.Totals.MessagesTotal += r.messageCount
		s.Totals.UserMessagesTotal += r.userMessageCount

		label := archetypeLabel(r.userMessageCount)
		switch label {
		case "automation":
			s.Archetypes.Automation++
			s.Totals.SessionsAutomation++
		case "quick":
			s.Archetypes.Quick++
			s.Totals.SessionsHuman++
			humanMax[label]++
		case "standard":
			s.Archetypes.Standard++
			s.Totals.SessionsHuman++
			humanMax[label]++
		case "deep":
			s.Archetypes.Deep++
			s.Totals.SessionsHuman++
			humanMax[label]++
		case "marathon":
			s.Archetypes.Marathon++
			s.Totals.SessionsHuman++
			humanMax[label]++
		}
		archMax[label]++
	}
	s.Archetypes.Primary = pickMaxLabel(archMax, []string{
		"automation", "marathon", "deep", "standard", "quick",
	})
	s.Archetypes.PrimaryHuman = pickMaxLabel(humanMax, []string{
		"marathon", "deep", "standard", "quick",
	})
}

// pickMaxLabel returns the key with the strictly highest count.
// Ties are broken by iterating priority in order — the earlier
// priority entry wins. Returns "" when counts is empty or every
// candidate count is zero, so empty windows do not fabricate a
// "primary" label.
func pickMaxLabel(counts map[string]int, priority []string) string {
	best := ""
	bestN := 0
	for _, k := range priority {
		if counts[k] > bestN {
			best = k
			bestN = counts[k]
		}
	}
	return best
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// scopedAccumulator collects values for one scope of one metric: a
// bucket slice plus the running sum/n needed for the arithmetic mean.
// Kept as a plain struct so computeDistributions can wire up one pair
// per metric without bespoke variables per scope.
type scopedAccumulator struct {
	buckets []DistributionBucketV1
	edges   []float64
	sum     float64
	n       int
}

func newAccumulator(edges []float64) scopedAccumulator {
	return scopedAccumulator{
		buckets: buildEmptyBuckets(edges),
		edges:   edges,
	}
}

func (a *scopedAccumulator) add(v float64) {
	addBucket(a.buckets, a.edges, v)
	a.sum += v
	a.n++
}

func (a *scopedAccumulator) finalize() ScopedDistribution {
	return ScopedDistribution{
		Buckets: a.buckets,
		Mean:    safeMean(a.sum, a.n),
	}
}

// computeDistributions populates the four scope-aware histograms on
// SessionStats. Scope rules:
//
//   - ScopeAll includes every row in the window.
//   - ScopeHuman requires userMessageCount >= 2 (mirrors the archetype
//     boundary between automation and quick).
//
// Per-metric filters excluded from both scopes:
//
//   - DurationMinutes: only rows with endedAt set (r.endedAt.Valid);
//     sessions without an end timestamp have no meaningful duration.
//   - ToolsPerTurn: only rows with assistantTurns > 0; a zero-turn
//     session has no meaningful turn rate and would otherwise bias
//     bucket 0 toward the zero ratio.
//
// PeakContextTokens is Claude-only: rows from other agents and rows
// without hasPeakContext data are excluded from every bucket; the
// Claude-specific null rows are tallied separately in NullCount.
func computeDistributions(s *SessionStats, rows []sessionStatsRow) {
	durAll := newAccumulator(durationMinutesEdges)
	durHuman := newAccumulator(durationMinutesEdges)
	umAll := newAccumulator(userMessagesEdgesAll)
	umHuman := newAccumulator(userMessagesEdgesHuman)
	pcAll := newAccumulator(peakContextEdges)
	pcHuman := newAccumulator(peakContextEdges)
	tptAll := newAccumulator(toolsPerTurnEdges)
	tptHuman := newAccumulator(toolsPerTurnEdges)
	var pcNull int

	for _, r := range rows {
		human := r.userMessageCount >= 2
		if r.endedAt.Valid {
			dur := r.endedAt.Time.Sub(r.startedAt).Minutes()
			// Drop clock-skewed / malformed sessions whose ended_at
			// precedes started_at: negative durations would distort
			// the mean and have no matching bucket. assignBucket
			// already drops them from the histogram, so excluding
			// them here keeps the mean and bucket totals consistent.
			if dur >= 0 {
				durAll.add(dur)
				if human {
					durHuman.add(dur)
				}
			}
		}
		umv := float64(r.userMessageCount)
		umAll.add(umv)
		if human {
			umHuman.add(umv)
		}
		if r.agent == "claude" {
			if r.hasPeakContext {
				pv := float64(r.peakContextTokens)
				pcAll.add(pv)
				if human {
					pcHuman.add(pv)
				}
			} else {
				pcNull++
			}
		}
		if r.assistantTurns > 0 {
			tpt := float64(r.totalToolCalls) / float64(r.assistantTurns)
			tptAll.add(tpt)
			if human {
				tptHuman.add(tpt)
			}
		}
	}

	s.Distributions.DurationMinutes = ScopedDistributionPair{
		ScopeAll:   durAll.finalize(),
		ScopeHuman: durHuman.finalize(),
	}
	s.Distributions.UserMessages = ScopedDistributionPair{
		ScopeAll:   umAll.finalize(),
		ScopeHuman: umHuman.finalize(),
	}
	s.Distributions.PeakContextTokens = PeakContextDistribution{
		ScopeAll:   pcAll.finalize(),
		ScopeHuman: pcHuman.finalize(),
		NullCount:  pcNull,
		ClaudeOnly: true,
	}
	s.Distributions.ToolsPerTurn = ScopedDistributionPair{
		ScopeAll:   tptAll.finalize(),
		ScopeHuman: tptHuman.finalize(),
	}
}

// addBucket places v into the bucket matching edges and increments
// its count. Values outside the edge range are silently dropped; the
// v1 edge lists all end in +Inf so this is unreachable in practice.
func addBucket(buckets []DistributionBucketV1, edges []float64, v float64) {
	idx := assignBucket(edges, v)
	if idx < 0 || idx >= len(buckets) {
		return
	}
	buckets[idx].Count++
}

// safeMean returns sum/n or 0 when n is zero. Keeps the JSON mean
// field numeric (never NaN) when a scope has no contributing rows.
func safeMean(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// computeAgentPortfolio fills SessionStats.AgentPortfolio by folding
// per-session counts and output tokens into one bucket per agent.
// Maps are always non-nil so the JSON output keeps stable {} values
// when the window contains no sessions.
//
// Sessions with an empty agent name are skipped to match the rest of
// the analytics code (sessions.go's "agent != ”" filter on the agents
// list). They would otherwise emit an empty-string JSON key and bias
// pickPrimaryAgent's lexicographic tiebreaker toward "".
//
// Token totals only include sessions whose has_total_output_tokens
// flag is set. Without that guard, agents whose token coverage is
// missing (default 0) would be indistinguishable from agents that
// truly produced no output tokens.
func computeAgentPortfolio(s *SessionStats, rows []sessionStatsRow) {
	bySessions := map[string]int{}
	byMessages := map[string]int{}
	byTokens := map[string]int64{}
	for _, r := range rows {
		if r.agent == "" {
			continue
		}
		bySessions[r.agent]++
		byMessages[r.agent] += r.messageCount
		if r.hasTotalOutputTokens {
			byTokens[r.agent] += r.totalOutputTokens
		}
	}
	s.AgentPortfolio.BySessions = bySessions
	s.AgentPortfolio.ByMessages = byMessages
	s.AgentPortfolio.ByTokens = byTokens
	s.AgentPortfolio.Primary = pickPrimaryAgent(bySessions)
}

// pickPrimaryAgent returns the agent with the highest session count.
// Ties are broken by choosing the lexicographically smallest agent
// name — a stable rule so downstream tools that golden-compare the
// JSON output see deterministic values regardless of Go's randomised
// map iteration order. Returns "" for an empty map.
func pickPrimaryAgent(bySessions map[string]int) string {
	best := ""
	bestN := -1
	for agent, n := range bySessions {
		if n > bestN || (n == bestN && agent < best) {
			best = agent
			bestN = n
		}
	}
	return best
}

// sessionCacheTotals accumulates the denominator tokens (input +
// cache_read + cache_creation) that drive the per-session ratio, plus
// the dollar figures for one Claude session. Output tokens don't feed
// the ratio and are baked directly into dollars* as they're parsed,
// so they're intentionally not kept on the struct.
type sessionCacheTotals struct {
	inputTok     int64
	cacheCreateT int64
	cacheReadT   int64
	dollarsSpent float64
	dollarsNoCac float64 // cost if the workload had never cached
}

// computeCacheEconomics populates stats.CacheEconomics for Claude
// sessions in the window. The field is a nullable pointer — it is
// left nil whenever rows contains no agent="claude" session so the
// JSON output stays absent for non-Claude workloads (see spec:
// "Section 6 hidden if cache_economics absent").
//
// Overall hit ratio is the weighted mean of cache_read over
// (input + cache_read + cache_creation), weighted by each session's
// denominator (equivalently: sum(cache_read)/sum(denominator) across
// sessions with a nonzero denominator). The spec's aggregator rule
// for merging cache_hit_ratio across machines is a weighted mean
// over the same denominator, so computing the single-machine number
// the same way keeps merge semantics stable.
//
// dollars_spent prices every eligible Claude message using the
// model_pricing table. dollars_saved_vs_uncached reprices cache_read
// tokens at the input rate and zeroes cache_creation (the
// counterfactual where the workload never cached), then subtracts
// dollars_spent. A missing pricing row zeroes out that model's
// contribution — the same graceful-degrade behaviour as GetDailyUsage.
func (db *DB) computeCacheEconomics(
	ctx context.Context, stats *SessionStats,
	rows []sessionStatsRow,
) error {
	claudeIDs := collectClaudeSessionIDs(rows)
	if len(claudeIDs) == 0 {
		return nil
	}

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return fmt.Errorf("loading pricing: %w", err)
	}

	perSession := make(map[string]*sessionCacheTotals, len(claudeIDs))
	if err := queryChunked(claudeIDs,
		func(chunk []string) error {
			return db.accumulateCacheTotals(
				ctx, chunk, pricing, perSession,
			)
		}); err != nil {
		return err
	}

	ce := &StatsCacheEconomics{
		ClaudeOnly: true,
		CacheHitRatio: CacheHitRatioDistribution{
			Buckets: buildCacheHitRatioBuckets(),
		},
	}
	var (
		cacheReadSum   int64
		denominatorSum int64
		dollarsSpent   float64
		dollarsNoCache float64
	)
	// Iterate in session-id order so floating-point sums stay
	// deterministic across runs; Go's map iteration order is
	// randomised and (a+b)+c != a+(b+c) in IEEE 754.
	keys := make([]string, 0, len(perSession))
	for k := range perSession {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		totals, ok := perSession[k]
		if !ok || totals == nil {
			continue
		}
		denom := totals.inputTok + totals.cacheReadT +
			totals.cacheCreateT
		dollarsSpent += totals.dollarsSpent
		dollarsNoCache += totals.dollarsNoCac
		if denom <= 0 {
			continue
		}
		cacheReadSum += totals.cacheReadT
		denominatorSum += denom
		ratio := float64(totals.cacheReadT) / float64(denom)
		addBucket(ce.CacheHitRatio.Buckets,
			cacheHitRatioEdges, ratio)
	}
	if denominatorSum > 0 {
		ce.CacheHitRatio.Overall =
			float64(cacheReadSum) / float64(denominatorSum)
	}
	ce.DollarsSpent = dollarsSpent
	// Negative savings are a legitimate outcome for write-heavy
	// workloads where cache_creation premiums outweigh cache_read
	// discounts. The existing usage views (internal/db/usage.go,
	// frontend/src/lib/utils/usageSavings.ts) surface that "costlier
	// than uncached" state directly, so do not clamp it away here —
	// hiding it would mask real cache-efficiency regressions.
	ce.DollarsSavedVsUncached = dollarsNoCache - dollarsSpent

	stats.CacheEconomics = ce
	return nil
}

// collectClaudeSessionIDs filters sessionStatsRow to the Claude-agent
// subset used by the cache_economics query. Kept as a helper so the
// caller reads as "build the list, run the query".
func collectClaudeSessionIDs(rows []sessionStatsRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.agent == "claude" {
			out = append(out, r.id)
		}
	}
	return out
}

// accumulateCacheTotals folds one chunk of Claude session IDs into
// perSession. Messages with empty token_usage or empty model are
// skipped — they match usageMessageEligibility's filter and keep the
// dollar numbers consistent with GetDailyUsage.
func (db *DB) accumulateCacheTotals(
	ctx context.Context, sessionIDs []string,
	pricing map[string]modelRates,
	perSession map[string]*sessionCacheTotals,
) error {
	ph, args := inPlaceholders(sessionIDs)
	// ORDER BY (session_id, ordinal) so floating-point sums are
	// reproducible across runs: SQLite is free to return rows in any
	// physical order otherwise, and (a+b)+c != a+(b+c) in IEEE 754.
	// The cross-session fold in computeCacheEconomics already sorts
	// session IDs; the per-message order completes the determinism
	// chain so golden tests stay byte-stable.
	q := `SELECT session_id, model, token_usage
		FROM messages
		WHERE session_id IN ` + ph + `
			AND token_usage != ''
			AND model != ''
			AND model != '<synthetic>'
		ORDER BY session_id, ordinal`
	sqlRows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying cache tokens: %w", err)
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
		var sessionID, model, tokenJSON string
		if err := sqlRows.Scan(
			&sessionID, &model, &tokenJSON,
		); err != nil {
			return fmt.Errorf("scanning cache tokens: %w", err)
		}
		addMessageToCacheTotals(
			perSession, sessionID, model, tokenJSON, pricing,
		)
	}
	return sqlRows.Err()
}

// addMessageToCacheTotals parses one message's token_usage JSON and
// folds its contribution into perSession. Split out of
// accumulateCacheTotals so the row loop stays a thin scan+dispatch.
func addMessageToCacheTotals(
	perSession map[string]*sessionCacheTotals,
	sessionID, model, tokenJSON string,
	pricing map[string]modelRates,
) {
	usage := gjson.Parse(tokenJSON)
	inputTok := usage.Get("input_tokens").Int()
	outputTok := usage.Get("output_tokens").Int()
	cacheCrTok := usage.Get("cache_creation_input_tokens").Int()
	cacheRdTok := usage.Get("cache_read_input_tokens").Int()

	totals, ok := perSession[sessionID]
	if !ok {
		totals = &sessionCacheTotals{}
		perSession[sessionID] = totals
	}
	totals.inputTok += inputTok
	totals.cacheCreateT += cacheCrTok
	totals.cacheReadT += cacheRdTok

	rates := pricing[model]
	totals.dollarsSpent += (float64(inputTok)*rates.input +
		float64(outputTok)*rates.output +
		float64(cacheCrTok)*rates.cacheCreation +
		float64(cacheRdTok)*rates.cacheRead) / 1_000_000
	// Uncached counterfactual: cache_creation tokens would still
	// have been sent as ordinary input (so they are billed at the
	// input rate, not dropped), and cache_read tokens are re-billed
	// at the input rate too. This matches the rest of the codebase
	// (see internal/db/usage.go and the savings calculation in
	// frontend/src/lib/utils/usageSavings.ts).
	totals.dollarsNoCac += (float64(inputTok)*rates.input +
		float64(outputTok)*rates.output +
		float64(cacheCrTok)*rates.input +
		float64(cacheRdTok)*rates.input) / 1_000_000
}

// computeTemporal fills stats.Temporal.HourlyUTC and ReporterTimezone.
//
// HourlyUTC groups user messages (role='user') by their UTC calendar
// hour. Each entry reports the count of user messages in that hour and
// the number of distinct sessions with at least one user message in
// that hour. Hours with zero activity are omitted (sparse output).
//
// Window + agent + project filters apply transitively via sessionIDs —
// the caller already filtered sessions via loadSessionsInWindow, so
// restricting to session_id IN (...) inherits those predicates. An
// empty sessionIDs slice short-circuits to an empty entry list without
// touching the database.
//
// Entries are sorted by TS ascending. The slice is always non-nil so
// the JSON output emits "hourly_utc": [] rather than null.
//
// ReporterTimezone reflects f.Timezone when set (honouring the CLI
// --timezone flag), the TZ env var when present, or time.Local's name
// otherwise. This is a best-effort IANA name; tooling that needs a
// strict tzdata lookup should pass --timezone explicitly.
func (db *DB) computeTemporal(
	ctx context.Context, stats *SessionStats, f StatsFilter,
	from, to time.Time, sessionIDs []string,
) error {
	stats.Temporal.HourlyUTC = []TemporalHourlyUTCEntry{}
	stats.Temporal.ReporterTimezone = reporterTimezone(f)

	if len(sessionIDs) == 0 {
		return nil
	}

	perHour := map[string]*TemporalHourlyUTCEntry{}
	if err := queryChunked(sessionIDs,
		func(chunk []string) error {
			return db.accumulateHourlyUTC(
				ctx, chunk, from, to, perHour,
			)
		}); err != nil {
		return err
	}

	hours := make([]string, 0, len(perHour))
	for h := range perHour {
		hours = append(hours, h)
	}
	sort.Strings(hours)

	out := make([]TemporalHourlyUTCEntry, 0, len(hours))
	for _, h := range hours {
		entry, ok := perHour[h]
		if !ok || entry == nil {
			continue
		}
		out = append(out, *entry)
	}
	stats.Temporal.HourlyUTC = out
	return nil
}

// accumulateHourlyUTC folds one chunk of session IDs into perHour.
// Messages without a timestamp are skipped — strftime returns NULL for
// empty strings, and we ignore the resulting row rather than bucketing
// it into the epoch.
//
// from/to bound the message timestamps so that long-running sessions
// don't drag pre-window or post-window activity into hourly_utc. The
// session window already restricted us to in-window sessions; this
// extra predicate keeps a session's stray messages from leaking out
// of [from, to).
//
// Sessions-per-hour is a distinct count: a session sending many
// messages in one hour counts once, but the same session appearing in
// two hours contributes to both. queryChunked slices sessionIDs into
// disjoint chunks, so a per-chunk seen-set is enough — no session ID
// crosses chunk boundaries.
func (db *DB) accumulateHourlyUTC(
	ctx context.Context, sessionIDs []string,
	from, to time.Time,
	perHour map[string]*TemporalHourlyUTCEntry,
) error {
	ph, args := inPlaceholders(sessionIDs)
	args = append(args,
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	)
	q := `SELECT
			strftime('%Y-%m-%dT%H:00:00Z', m.timestamp) AS utc_hour,
			m.session_id
		FROM messages m
		WHERE m.session_id IN ` + ph + `
			AND m.role = 'user'
			AND m.timestamp IS NOT NULL
			AND m.timestamp != ''
			AND m.timestamp >= ?
			AND m.timestamp < ?`
	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying temporal hourly_utc: %w", err)
	}
	defer rows.Close()
	seen := map[string]map[string]struct{}{}
	for rows.Next() {
		var hour sql.NullString
		var sessionID string
		if err := rows.Scan(&hour, &sessionID); err != nil {
			return fmt.Errorf("scanning hourly_utc: %w", err)
		}
		if !hour.Valid || hour.String == "" {
			continue
		}
		entry, ok := perHour[hour.String]
		if !ok {
			entry = &TemporalHourlyUTCEntry{TS: hour.String}
			perHour[hour.String] = entry
		}
		entry.UserMessages++
		hourSeen, ok := seen[hour.String]
		if !ok {
			hourSeen = map[string]struct{}{}
			seen[hour.String] = hourSeen
		}
		if _, dup := hourSeen[sessionID]; !dup {
			hourSeen[sessionID] = struct{}{}
			entry.Sessions++
		}
	}
	return rows.Err()
}

// reporterTimezone picks the best-effort IANA name to record in
// SessionStats.Temporal.ReporterTimezone. Precedence:
//
//  1. f.Timezone when non-empty — echoes the --timezone flag.
//  2. TZ environment variable — what most Unix tools respect.
//  3. time.Local.String() — may be "Local" on systems without /etc/localtime.
//
// This function is intentionally simple: it does not attempt tzdata
// lookups or validate the result. Consumers that need a strict zone
// pass --timezone explicitly and get the validated name back.
func reporterTimezone(f StatsFilter) string {
	if f.Timezone != "" {
		return f.Timezone
	}
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	return time.Local.String()
}

// computeOutcomes populates stats.Outcomes from the Claude-agent subset
// of rows. The pointer stays nil when the window contains no Claude
// sessions so the JSON output stays absent for pure non-Claude
// workloads (matching the cache_economics convention: omitempty + nil).
//
// The JSON contract exposes success/failure/unknown buckets, but
// agentsview's sessions.outcome column uses a different vocabulary
// ("completed" / "abandoned" / "errored" / "unknown" — see
// internal/signals/outcome.go). The switch below maps the stored
// vocabulary onto the contract. Unknown counts the schema default
// "unknown" plus any legacy empty string or future additions.
// GradeDistribution is always allocated as a non-nil map so the JSON
// emits "grade_distribution": {} rather than null when no session has
// a grade yet; empty health_grade values are skipped so the map never
// carries a "" key.
//
// ToolRetryRate is guarded against division by zero — without that
// guard a window with retries but no (counted) tool calls would divide
// by zero (NaN), which JSON cannot encode. CompactionsPerSession and
// AvgEditChurn do not need a guard because the early return above
// guarantees len(claudeRows) > 0.
func computeOutcomes(s *SessionStats, rows []sessionStatsRow) {
	var claudeRows []sessionStatsRow
	for _, r := range rows {
		if r.agent == "claude" {
			claudeRows = append(claudeRows, r)
		}
	}
	if len(claudeRows) == 0 {
		return
	}
	out := &StatsOutcomes{
		ClaudeOnly:        true,
		GradeDistribution: map[string]int{},
	}
	totalTools := 0
	totalRetries := 0
	totalCompactions := 0
	totalChurn := 0
	for _, r := range claudeRows {
		// Map agentsview's outcome vocabulary (see
		// internal/signals/outcome.go) onto the JSON contract's
		// success/failure/unknown buckets. "completed" is the only
		// positive outcome; "abandoned" and "errored" both indicate
		// the session did not reach a clean finish.
		switch r.outcome {
		case "completed":
			out.Success++
		case "abandoned", "errored":
			out.Failure++
		default:
			// Covers "unknown", empty, and any future additions.
			out.Unknown++
		}
		if r.healthGrade != "" {
			out.GradeDistribution[r.healthGrade]++
		}
		totalTools += r.totalToolCalls
		totalRetries += r.toolRetryCount
		totalCompactions += r.compactionCount
		totalChurn += r.editChurnCount
	}
	if totalTools > 0 {
		out.ToolRetryRate = float64(totalRetries) /
			float64(totalTools)
	}
	// len(claudeRows) > 0 is guaranteed by the early return above.
	out.CompactionsPerSession = float64(totalCompactions) /
		float64(len(claudeRows))
	out.AvgEditChurn = float64(totalChurn) /
		float64(len(claudeRows))
	s.Outcomes = out
}

// computeAdoption populates stats.Adoption for Claude sessions in the
// window. The field is a nullable pointer — it stays nil whenever the
// window contains zero agent="claude" sessions so the JSON output stays
// absent for pure non-Claude workloads (matching the cache_economics
// and outcomes convention: omitempty + nil).
//
// Metrics are derived from the tool_calls table, restricted to the
// already-filtered Claude session IDs so window/project predicates flow
// through transitively:
//
//   - PlanModeRate: distinct Claude sessions with at least one row where
//     tool_name = "ExitPlanMode", divided by total Claude sessions.
//     Always in [0, 1].
//   - SubagentsPerSession: total tool_calls rows with tool_name in
//     ("Task", "Agent"), divided by total Claude sessions. Can exceed 1
//     (it is a mean). Both names refer to the same subagent dispatch
//     primitive — Claude Code records it as "Task" historically and as
//     "Agent" in newer transcripts; counting both keeps the metric
//     stable across the rename.
//   - DistinctSkills: count of distinct non-empty skill_name values
//     recorded on rows with tool_name = "Skill". The schema already
//     normalises skill_name as a dedicated column (see schema.sql), so
//     no JSON parsing is required.
func (db *DB) computeAdoption(
	ctx context.Context, stats *SessionStats, rows []sessionStatsRow,
) error {
	claudeIDs := collectClaudeSessionIDs(rows)
	if len(claudeIDs) == 0 {
		return nil
	}
	planModeSessions := map[string]struct{}{}
	skillNames := map[string]struct{}{}
	var totalSubagents int
	if err := queryChunked(claudeIDs,
		func(chunk []string) error {
			return db.accumulateAdoption(
				ctx, chunk,
				planModeSessions, skillNames, &totalSubagents,
			)
		}); err != nil {
		return err
	}
	n := float64(len(claudeIDs))
	stats.Adoption = &StatsAdoption{
		ClaudeOnly:          true,
		PlanModeRate:        float64(len(planModeSessions)) / n,
		SubagentsPerSession: float64(totalSubagents) / n,
		DistinctSkills:      len(skillNames),
	}
	return nil
}

// accumulateAdoption folds one chunk of Claude session IDs into the
// three per-window accumulators. One pass over tool_calls scans only
// the three tool_name values the adoption metrics need; a
// single-column skill_name projection keeps the result set narrow.
func (db *DB) accumulateAdoption(
	ctx context.Context, sessionIDs []string,
	planModeSessions map[string]struct{},
	skillNames map[string]struct{},
	totalSubagents *int,
) error {
	ph, args := inPlaceholders(sessionIDs)
	q := `SELECT session_id, tool_name, COALESCE(skill_name, '')
		FROM tool_calls
		WHERE session_id IN ` + ph + `
			AND tool_name IN ('ExitPlanMode', 'Task', 'Agent', 'Skill')`
	rows, err := db.getReader().QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying adoption tool_calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID, toolName, skillName string
		if err := rows.Scan(&sessionID, &toolName, &skillName); err != nil {
			return fmt.Errorf("scanning adoption tool_calls: %w", err)
		}
		switch toolName {
		case "ExitPlanMode":
			planModeSessions[sessionID] = struct{}{}
		case "Task", "Agent":
			*totalSubagents++
		case "Skill":
			if skillName != "" {
				skillNames[skillName] = struct{}{}
			}
		}
	}
	return rows.Err()
}

// computeOutcomeStats populates stats.OutcomeStats by discovering the git
// repositories enclosing session cwds in the window and aggregating
// author-filtered commit activity across them. Output stays nil when no
// session in the window has a recognisable cwd — a signal that the caller
// has no git-derived outcome data, not a legitimate zero.
//
// Each repo is processed independently: a failure from one (bad path,
// missing git, unreadable config) is logged via the error path but does
// not abort the aggregation — per-repo errors are swallowed so a single
// broken checkout can't erase every other repo's numbers. Repos with no
// resolvable author email are skipped; without an author filter the log
// aggregation would attribute every other contributor's commits to the
// local user.
//
// PR counts are only populated when f.GHToken is set. When gh is
// configured, PRsOpened and PRsMerged accumulate across every repo that
// successfully returned a PRResult; gh failures (unauthenticated,
// network) are swallowed the same way log failures are. When the token
// is empty, both pointers stay nil so the JSON output distinguishes
// "gh not configured" from "configured, zero PRs".
//
// from/to are the absolute window bounds already resolved by
// windowBounds. They are formatted as RFC3339 UTC before being handed to
// `git log --since/--until` (git accepts RFC3339) and to
// `gh pr list --search`, which wants YYYY-MM-DD or RFC3339. The raw
// f.Since / f.Until strings ("28d", "7d", etc.) are not passed through
// because git does not understand the compact duration form.
func (db *DB) computeOutcomeStats(
	ctx context.Context, s *SessionStats, f StatsFilter,
	from, to time.Time, rows []sessionStatsRow,
) error {
	cwds := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.cwd != "" {
			cwds = append(cwds, r.cwd)
		}
	}
	repos := git.DiscoverRepos(cwds)
	if len(repos) == 0 {
		return nil
	}
	since := from.UTC().Format(time.RFC3339)
	until := to.UTC().Format(time.RFC3339)
	cache := git.NewCache(db.getWriter())
	out := &StatsOutcomeStats{}
	contributed := false
	for _, repo := range repos {
		email := git.AuthorEmail(repo)
		if email == "" {
			continue
		}
		logRes, err := git.AggregateLogCached(
			ctx, cache, repo, email, since, until, time.Hour,
		)
		if err != nil {
			// Per-repo failures are logged but don't abort
			// aggregation across other repos.
			log.Printf(
				"computeOutcomeStats: repo=%s op=log err=%v",
				repo, err,
			)
			continue
		}
		contributed = true
		out.ReposActive++
		out.Commits += logRes.Commits
		out.LOCAdded += logRes.LOCAdded
		out.LOCRemoved += logRes.LOCRemoved
		out.FilesChanged += logRes.FilesChanged

		if f.GHToken != "" {
			prRes, err := git.AggregatePRsCached(
				ctx, cache, repo, since, until,
				f.GHToken, time.Hour,
			)
			if err != nil {
				log.Printf(
					"computeOutcomeStats: repo=%s op=pr err=%v",
					repo, err,
				)
			} else if prRes != nil {
				addPtr(&out.PRsOpened, prRes.Opened)
				addPtr(&out.PRsMerged, prRes.Merged)
			}
		}
	}
	// Leave OutcomeStats nil when every repo was skipped (missing
	// author email) or every git command failed. Emitting an
	// all-zero block would falsely advertise "no commits" when the
	// real signal is "we couldn't derive any".
	if !contributed {
		return nil
	}
	s.OutcomeStats = out
	return nil
}

// addPtr lazily allocates *p on first write, then adds v. Used by
// computeOutcomeStats so PRsOpened / PRsMerged stay nil when no repo
// produced a gh result — distinguishing "gh not configured" from a
// legitimate zero count.
func addPtr(p **int, v int) {
	if *p == nil {
		zero := 0
		*p = &zero
	}
	**p += v
}
