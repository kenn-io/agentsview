package db

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/uptrace/bun"
)

type bunSignalOverall struct {
	Sessions, HealthSum, HealthCount              int
	FailureSum, Retries, EditChurn                int
	SessionsWithFailures, Compactions             int
	SessionsWithCompaction, MidTaskCompactions    int
	SessionsWithMidTask, ContextCount             int
	HighPressure                                  int
	ContextPressureSum                            float64
	QualityComputed                               int
	ShortPrompts, ShortPromptSessions             int
	Unstructured, MissingCriteria                 int
	MissingCriteriaSessions, MissingVerification  int
	MissingVerificationSessions, DuplicatePrompts int
	DuplicateSessions, NoCodeContext              int
	NoCodeSessions, RunawayLoops, RunawaySessions int
}

type bunSignalDimension struct {
	Kind, Label                      string
	Sessions, HealthSum, HealthCount int
	Completed, Errored, Abandoned    int
	FailureSum                       int
}

type bunSignalCalibrationRow struct {
	Signal                                 string
	Affected, Baseline                     int
	AffectedIncomplete, BaselineIncomplete int
	AffectedScoreSum, AffectedScoreCount   int
	BaselineScoreSum, BaselineScoreCount   int
}

func (s *BunStore) bunSignalFactsCTE(
	store bun.IDB, f AnalyticsFilter,
) (BunSQLFragment, error) {
	builder, err := newBunAnalyticsSQL(s.backend.Capabilities().AnalyticsDialect, f)
	if err != nil {
		return BunSQLFragment{}, err
	}
	ctes := []BunCTEFragment{builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, bunAnalyticsSessionDateScope,
	)}
	join := ""
	if builder.needsMessageQualification() {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
		join = " JOIN " + bunAnalyticsQualifiedSessionsCTE +
			" AS qualified ON qualified.session_id = session.id"
	}
	instant := "COALESCE(" + bunNullableTimestamp("session.started_at") +
		", session.created_at)"
	date := builder.dialect.Date(builder.dialect.LocalTimestamp(instant, builder.zone))
	ctes = append(ctes, BunCTEFragment{Name: "analytics_signal_facts", Query: BunSQL(
		"SELECT session.*, "+date.SQL+" AS local_date FROM "+
			bunAnalyticsFilteredSessionsCTE+" AS session"+join, date.Args...)})
	return renderBunCTEs(ctes...), nil
}

func (s *BunStore) getBunAnalyticsSignalsAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (SignalsAnalyticsResponse, error) {
	with, err := s.bunSignalFactsCTE(store, f)
	if err != nil {
		return SignalsAnalyticsResponse{}, err
	}
	overall, err := bunSignalOverallFrom(ctx, store, with)
	if err != nil {
		return SignalsAnalyticsResponse{}, err
	}
	dimensions, err := bunSignalDimensionsFrom(ctx, store, with)
	if err != nil {
		return SignalsAnalyticsResponse{}, err
	}
	calibrations, err := bunSignalCalibrationsFrom(ctx, store, with)
	if err != nil {
		return SignalsAnalyticsResponse{}, err
	}
	result := shapeBunSignals(overall, dimensions, calibrations)
	frustration, err := bunFrustrationSignalFrom(ctx, store, with)
	if err != nil {
		return SignalsAnalyticsResponse{}, err
	}
	result.QualityHealth.Totals.FrustrationMarkerCount = frustration.total
	result.QualityHealth.SessionsWithSignal.FrustrationMarkerCount = frustration.sessions
	result.Calibration[frustration.calibration.Signal] = frustration.calibration
	return result, nil
}

type bunFrustrationSignal struct {
	total, sessions int
	calibration     SignalCalibration
}

func bunFrustrationSignalFrom(
	ctx context.Context, store bun.IDB, with BunSQLFragment,
) (bunFrustrationSignal, error) {
	var rows []SignalRow
	if err := store.NewRaw(`WITH `+with.SQL+` SELECT id, outcome,
		health_score, health_grade FROM analytics_signal_facts ORDER BY id`,
		with.Args...).Scan(ctx, &rows); err != nil {
		return bunFrustrationSignal{}, fmt.Errorf("querying Bun frustration candidates: %w", err)
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	counts, err := bunFrustrationCountsFrom(ctx, store, ids)
	if err != nil {
		return bunFrustrationSignal{}, err
	}
	result := bunFrustrationSignal{}
	for i := range rows {
		rows[i].FrustrationMarkerCount = counts[rows[i].ID]
		result.total += rows[i].FrustrationMarkerCount
		if rows[i].FrustrationMarkerCount > 0 {
			result.sessions++
		}
	}
	result.calibration = calibrateSignal(rows, "frustration_marker_count")
	return result, nil
}

func bunSignalOverallFrom(
	ctx context.Context, store bun.IDB, with BunSQLFragment,
) (bunSignalOverall, error) {
	var row bunSignalOverall
	query := `WITH ` + with.SQL + ` SELECT
	COUNT(*) AS sessions,
	CAST(COALESCE(SUM(health_score),0) AS BIGINT) AS health_sum,
	COUNT(health_score) AS health_count,
	CAST(COALESCE(SUM(tool_failure_signal_count),0) AS BIGINT) AS failure_sum,
	CAST(COALESCE(SUM(tool_retry_count),0) AS BIGINT) AS retries,
	CAST(COALESCE(SUM(edit_churn_count),0) AS BIGINT) AS edit_churn,
	CAST(SUM(CASE WHEN tool_failure_signal_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS sessions_with_failures,
	CAST(COALESCE(SUM(compaction_count),0) AS BIGINT) AS compactions,
	CAST(SUM(CASE WHEN compaction_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS sessions_with_compaction,
	CAST(COALESCE(SUM(mid_task_compaction_count),0) AS BIGINT) AS mid_task_compactions,
	CAST(SUM(CASE WHEN mid_task_compaction_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS sessions_with_mid_task,
	COUNT(context_pressure_max) AS context_count,
	CAST(SUM(CASE WHEN context_pressure_max >= 0.8 THEN 1 ELSE 0 END) AS BIGINT) AS high_pressure,
	CAST(COALESCE(SUM(context_pressure_max),0) AS DOUBLE PRECISION) AS context_pressure_sum,
	CAST(SUM(CASE WHEN quality_signal_version > 0 THEN 1 ELSE 0 END) AS BIGINT) AS quality_computed,
	CAST(COALESCE(SUM(CASE WHEN quality_signal_version > 0 THEN short_prompt_count ELSE 0 END),0) AS BIGINT) AS short_prompts,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND short_prompt_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS short_prompt_sessions,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND unstructured_start THEN 1 ELSE 0 END) AS BIGINT) AS unstructured,
	CAST(COALESCE(SUM(CASE WHEN quality_signal_version > 0 THEN missing_success_criteria_count ELSE 0 END),0) AS BIGINT) AS missing_criteria,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND missing_success_criteria_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS missing_criteria_sessions,
	CAST(COALESCE(SUM(CASE WHEN quality_signal_version > 0 THEN missing_verification_count ELSE 0 END),0) AS BIGINT) AS missing_verification,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND missing_verification_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS missing_verification_sessions,
	CAST(COALESCE(SUM(CASE WHEN quality_signal_version > 0 THEN duplicate_prompt_count ELSE 0 END),0) AS BIGINT) AS duplicate_prompts,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND duplicate_prompt_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS duplicate_sessions,
	CAST(COALESCE(SUM(CASE WHEN quality_signal_version > 0 THEN no_code_context_count ELSE 0 END),0) AS BIGINT) AS no_code_context,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND no_code_context_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS no_code_sessions,
	CAST(COALESCE(SUM(CASE WHEN quality_signal_version > 0 THEN runaway_tool_loop_count ELSE 0 END),0) AS BIGINT) AS runaway_loops,
	CAST(SUM(CASE WHEN quality_signal_version > 0 AND runaway_tool_loop_count > 0 THEN 1 ELSE 0 END) AS BIGINT) AS runaway_sessions
FROM analytics_signal_facts`
	if err := store.NewRaw(query, with.Args...).Scan(ctx, &row); err != nil {
		return row, fmt.Errorf("querying Bun signal totals: %w", err)
	}
	return row, nil
}

func bunSignalDimensionsFrom(
	ctx context.Context, store bun.IDB, with BunSQLFragment,
) ([]bunSignalDimension, error) {
	query := `WITH ` + with.SQL + `
SELECT 'agent' AS kind, agent AS label, COUNT(*) AS sessions,
	CAST(COALESCE(SUM(health_score),0) AS BIGINT) AS health_sum,
	COUNT(health_score) AS health_count,
	CAST(SUM(CASE WHEN outcome='completed' THEN 1 ELSE 0 END) AS BIGINT) AS completed,
	CAST(SUM(CASE WHEN outcome='errored' THEN 1 ELSE 0 END) AS BIGINT) AS errored,
	CAST(SUM(CASE WHEN outcome='abandoned' THEN 1 ELSE 0 END) AS BIGINT) AS abandoned,
	CAST(COALESCE(SUM(tool_failure_signal_count),0) AS BIGINT) AS failure_sum
FROM analytics_signal_facts GROUP BY agent
UNION ALL SELECT 'project', project, COUNT(*),
	CAST(COALESCE(SUM(health_score),0) AS BIGINT), COUNT(health_score),
	CAST(SUM(CASE WHEN outcome='completed' THEN 1 ELSE 0 END) AS BIGINT),
	CAST(SUM(CASE WHEN outcome='errored' THEN 1 ELSE 0 END) AS BIGINT),
	CAST(SUM(CASE WHEN outcome='abandoned' THEN 1 ELSE 0 END) AS BIGINT),
	CAST(COALESCE(SUM(tool_failure_signal_count),0) AS BIGINT)
FROM analytics_signal_facts GROUP BY project
UNION ALL SELECT 'trend', local_date, COUNT(*),
	CAST(COALESCE(SUM(health_score),0) AS BIGINT), COUNT(health_score),
	CAST(SUM(CASE WHEN outcome='completed' THEN 1 ELSE 0 END) AS BIGINT),
	CAST(SUM(CASE WHEN outcome='errored' THEN 1 ELSE 0 END) AS BIGINT),
	CAST(SUM(CASE WHEN outcome='abandoned' THEN 1 ELSE 0 END) AS BIGINT),
	CAST(COALESCE(SUM(tool_failure_signal_count),0) AS BIGINT)
FROM analytics_signal_facts GROUP BY local_date
UNION ALL SELECT 'grade', health_grade, COUNT(*), 0,0,0,0,0,0
FROM analytics_signal_facts WHERE COALESCE(health_grade,'') != '' GROUP BY health_grade
UNION ALL SELECT 'outcome', outcome, COUNT(*), 0,0,0,0,0,0
FROM analytics_signal_facts WHERE COALESCE(outcome,'') != '' GROUP BY outcome
UNION ALL SELECT 'confidence', outcome_confidence, COUNT(*), 0,0,0,0,0,0
FROM analytics_signal_facts WHERE COALESCE(outcome_confidence,'') != '' GROUP BY outcome_confidence
ORDER BY kind, label`
	var rows []bunSignalDimension
	if err := store.NewRaw(query, with.Args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun signal dimensions: %w", err)
	}
	return rows, nil
}

func bunSignalCalibrationsFrom(
	ctx context.Context, store bun.IDB, with BunSQLFragment,
) ([]bunSignalCalibrationRow, error) {
	expressions := []struct{ name, expression string }{
		{"tool_failure_signals", "tool_failure_signal_count > 0"},
		{"tool_retries", "tool_retry_count > 0"},
		{"edit_churn", "edit_churn_count > 0"},
		{"sessions_with_compaction", "compaction_count > 0"},
		{"mid_task_compaction_count", "mid_task_compaction_count > 0"},
		{"high_pressure_sessions", "context_pressure_max >= 0.8"},
		{"short_prompt_count", "short_prompt_count > 0"},
		{"unstructured_start", "unstructured_start"},
		{"missing_success_criteria_count", "missing_success_criteria_count > 0"},
		{"missing_verification_count", "missing_verification_count > 0"},
		{"duplicate_prompt_count", "duplicate_prompt_count > 0"},
		{"no_code_context_count", "no_code_context_count > 0"},
		{"runaway_tool_loop_count", "runaway_tool_loop_count > 0"},
	}
	incomplete := "(outcome IN ('errored','abandoned') OR health_grade IN ('D','F'))"
	parts := make([]string, 0, len(expressions))
	for _, item := range expressions {
		parts = append(parts, `SELECT '`+item.name+`' AS signal,
	CAST(SUM(CASE WHEN `+item.expression+` THEN 1 ELSE 0 END) AS BIGINT) AS affected,
	CAST(SUM(CASE WHEN NOT (`+item.expression+`) OR (`+item.expression+`) IS NULL THEN 1 ELSE 0 END) AS BIGINT) AS baseline,
	CAST(SUM(CASE WHEN (`+item.expression+`) AND `+incomplete+` THEN 1 ELSE 0 END) AS BIGINT) AS affected_incomplete,
	CAST(SUM(CASE WHEN (NOT (`+item.expression+`) OR (`+item.expression+`) IS NULL) AND `+incomplete+` THEN 1 ELSE 0 END) AS BIGINT) AS baseline_incomplete,
	CAST(COALESCE(SUM(CASE WHEN (`+item.expression+`) THEN health_score ELSE 0 END),0) AS BIGINT) AS affected_score_sum,
	CAST(SUM(CASE WHEN (`+item.expression+`) AND health_score IS NOT NULL THEN 1 ELSE 0 END) AS BIGINT) AS affected_score_count,
	CAST(COALESCE(SUM(CASE WHEN (NOT (`+item.expression+`) OR (`+item.expression+`) IS NULL) THEN health_score ELSE 0 END),0) AS BIGINT) AS baseline_score_sum,
	CAST(SUM(CASE WHEN (NOT (`+item.expression+`) OR (`+item.expression+`) IS NULL) AND health_score IS NOT NULL THEN 1 ELSE 0 END) AS BIGINT) AS baseline_score_count
FROM analytics_signal_facts`)
	}
	var rows []bunSignalCalibrationRow
	if err := store.NewRaw("WITH "+with.SQL+" "+strings.Join(parts, " UNION ALL "),
		with.Args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun signal calibrations: %w", err)
	}
	return rows, nil
}

func shapeBunSignals(
	o bunSignalOverall, dimensions []bunSignalDimension,
	calibrations []bunSignalCalibrationRow,
) SignalsAnalyticsResponse {
	r := SignalsAnalyticsResponse{
		ScoredSessions: o.HealthCount, UnscoredSessions: o.Sessions - o.HealthCount,
		GradeDistribution: map[string]int{}, OutcomeDistribution: map[string]int{},
		OutcomeConfidenceDistribution: map[string]int{}, Calibration: map[string]SignalCalibration{},
		Trend: []SignalsTrendBucket{}, ByAgent: []SignalsAgentRow{}, ByProject: []SignalsProjectRow{},
		ToolHealth:    SignalsToolHealth{TotalFailureSignals: o.FailureSum, TotalRetries: o.Retries, TotalEditChurn: o.EditChurn, SessionsWithFailures: o.SessionsWithFailures},
		ContextHealth: SignalsContextHealth{SessionsWithCompaction: o.SessionsWithCompaction, MidTaskCompactionCount: o.MidTaskCompactions, SessionsWithMidTaskCompac: o.SessionsWithMidTask, SessionsWithContextData: o.ContextCount, HighPressureSessions: o.HighPressure},
		QualityHealth: SignalsQualityHealth{ComputedSessions: o.QualityComputed, Totals: QualitySignalTotals{ShortPromptCount: o.ShortPrompts, UnstructuredStart: o.Unstructured, MissingSuccessCriteriaCount: o.MissingCriteria, MissingVerificationCount: o.MissingVerification, DuplicatePromptCount: o.DuplicatePrompts, NoCodeContextCount: o.NoCodeContext, RunawayToolLoopCount: o.RunawayLoops}, SessionsWithSignal: QualitySignalTotals{ShortPromptCount: o.ShortPromptSessions, UnstructuredStart: o.Unstructured, MissingSuccessCriteriaCount: o.MissingCriteriaSessions, MissingVerificationCount: o.MissingVerificationSessions, DuplicatePromptCount: o.DuplicateSessions, NoCodeContextCount: o.NoCodeSessions, RunawayToolLoopCount: o.RunawaySessions}},
	}
	if o.HealthCount > 0 {
		v := round1(float64(o.HealthSum) / float64(o.HealthCount))
		r.AvgHealthScore = &v
	}
	if o.Sessions > 0 {
		r.ToolHealth.FailureRate = math.Round(float64(o.SessionsWithFailures)/float64(o.Sessions)*1000) / 10
		r.ContextHealth.AvgCompactionCount = round1(float64(o.Compactions) / float64(o.Sessions))
	}
	if o.ContextCount > 0 {
		v := math.Round(o.ContextPressureSum/float64(o.ContextCount)*1000) / 1000
		r.ContextHealth.AvgContextPressure = &v
	}
	for _, d := range dimensions {
		var avg *float64
		if d.HealthCount > 0 {
			v := round1(float64(d.HealthSum) / float64(d.HealthCount))
			avg = &v
		}
		completed, failures := 0.0, 0.0
		if d.Sessions > 0 {
			completed = math.Round(float64(d.Completed)/float64(d.Sessions)*1000) / 10
			failures = round1(float64(d.FailureSum) / float64(d.Sessions))
		}
		switch d.Kind {
		case "agent":
			r.ByAgent = append(r.ByAgent, SignalsAgentRow{Agent: d.Label, SessionCount: d.Sessions, AvgHealthScore: avg, CompletedRate: completed, AvgFailureSignals: failures})
		case "project":
			r.ByProject = append(r.ByProject, SignalsProjectRow{Project: d.Label, SessionCount: d.Sessions, AvgHealthScore: avg, CompletedRate: completed, AvgFailureSignals: failures})
		case "trend":
			r.Trend = append(r.Trend, SignalsTrendBucket{Date: d.Label, SessionCount: d.Sessions, AvgHealthScore: avg, Completed: d.Completed, Errored: d.Errored, Abandoned: d.Abandoned, AvgFailureSignals: failures})
		case "grade":
			r.GradeDistribution[d.Label] = d.Sessions
		case "outcome":
			r.OutcomeDistribution[d.Label] = d.Sessions
		case "confidence":
			r.OutcomeConfidenceDistribution[d.Label] = d.Sessions
		}
	}
	sort.Slice(r.ByProject, func(i, j int) bool {
		if r.ByProject[i].SessionCount != r.ByProject[j].SessionCount {
			return r.ByProject[i].SessionCount > r.ByProject[j].SessionCount
		}
		return r.ByProject[i].Project < r.ByProject[j].Project
	})
	for _, c := range calibrations {
		r.Calibration[c.Signal] = shapeSignalCalibration(c)
	}
	return r
}

func shapeSignalCalibration(c bunSignalCalibrationRow) SignalCalibration {
	r := SignalCalibration{Signal: c.Signal, AffectedSessions: c.Affected, BaselineSessions: c.Baseline}
	if c.Affected > 0 {
		r.AffectedIncompleteRate = round1(float64(c.AffectedIncomplete) / float64(c.Affected) * 100)
	}
	if c.Baseline > 0 {
		r.BaselineIncompleteRate = round1(float64(c.BaselineIncomplete) / float64(c.Baseline) * 100)
	}
	if c.Affected > 0 && c.Baseline > 0 && r.BaselineIncompleteRate > 0 {
		v := round1(r.AffectedIncompleteRate / r.BaselineIncompleteRate)
		r.IncompleteLift = &v
	}
	if c.AffectedScoreCount > 0 && c.BaselineScoreCount > 0 {
		v := round1(float64(c.AffectedScoreSum)/float64(c.AffectedScoreCount) - float64(c.BaselineScoreSum)/float64(c.BaselineScoreCount))
		r.AvgScoreDelta = &v
	}
	return r
}

func (s *BunStore) bunSignalCandidatesFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, signal string, limit int,
) ([]SignalRow, error) {
	with, err := s.bunSignalFactsCTE(store, f)
	if err != nil {
		return nil, err
	}
	expression := map[string]string{
		"outcome_errored":      "CASE WHEN outcome = 'errored' THEN 1 ELSE 0 END",
		"outcome_abandoned":    "CASE WHEN outcome = 'abandoned' THEN 1 ELSE 0 END",
		"outcome_completed":    "CASE WHEN outcome = 'completed' THEN 1 ELSE 0 END",
		"tool_failure_signals": "tool_failure_signal_count",
		"tool_retries":         "tool_retry_count", "edit_churn": "edit_churn_count",
		"sessions_with_compaction":       "CASE WHEN compaction_count > 0 THEN 1 ELSE 0 END",
		"mid_task_compaction_count":      "mid_task_compaction_count",
		"high_pressure_sessions":         "CASE WHEN context_pressure_max >= 0.8 THEN 1 ELSE 0 END",
		"short_prompt_count":             "short_prompt_count",
		"unstructured_start":             "CASE WHEN unstructured_start THEN 1 ELSE 0 END",
		"missing_success_criteria_count": "missing_success_criteria_count",
		"missing_verification_count":     "missing_verification_count",
		"duplicate_prompt_count":         "duplicate_prompt_count",
		"no_code_context_count":          "no_code_context_count",
		"runaway_tool_loop_count":        "runaway_tool_loop_count",
	}[signal]
	if expression == "" {
		return nil, ErrUnsupportedAnalyticsSignal
	}
	query := `WITH ` + with.SQL + ` SELECT id, agent, project,
		local_date AS date, first_message, is_automated, health_score,
		health_grade, outcome, outcome_confidence,
		tool_failure_signal_count, tool_retry_count, edit_churn_count,
		compaction_count, mid_task_compaction_count, context_pressure_max,
		quality_signal_version, short_prompt_count, unstructured_start,
		missing_success_criteria_count, missing_verification_count,
		duplicate_prompt_count, no_code_context_count, runaway_tool_loop_count
	FROM analytics_signal_facts
	WHERE ` + expression + ` > 0
	ORDER BY ` + expression + ` DESC,
		CASE WHEN outcome IN ('errored','abandoned') OR health_grade IN ('D','F')
			THEN 1 ELSE 0 END DESC,
		CASE WHEN health_score IS NULL THEN 1 ELSE 0 END ASC,
		health_score ASC, local_date DESC
	LIMIT ?`
	var rows []SignalRow
	args := append(append([]any(nil), with.Args...), limit)
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun signal candidates: %w", err)
	}
	return rows, nil
}
