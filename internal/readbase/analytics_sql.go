package readbase

import (
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func AnalyticsSummaryAgentsSQL(where string, args []any, dialect db.QueryDialect) (string, []any) {
	return `
		WITH filtered AS (
			SELECT s.agent, s.message_count
			FROM sessions s
			WHERE ` + where + `
		)
		SELECT agent, ` + dialect.SignedAggregate("COUNT(*)") + `, ` + dialect.SignedAggregate("COALESCE(SUM(message_count), 0)") + `
		FROM filtered
		GROUP BY agent`, args
}

func AnalyticsHeatmapSQL(where string, args []any, localDate string, localDateArgs []any, metric string, dialect db.QueryDialect) (string, []any) {
	valueExpr := "COALESCE(SUM(s.message_count), 0)"
	switch metric {
	case "sessions":
		valueExpr = "COUNT(*)"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = " + dialect.TrueLiteral()
		valueExpr = "COALESCE(SUM(s.total_output_tokens), 0)"
	}
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	return `
		SELECT ` + localDate + ` AS local_date, ` + dialect.SignedAggregate(valueExpr) + ` AS value
		FROM sessions s
		WHERE ` + where + `
		GROUP BY local_date
		ORDER BY local_date`, queryArgs
}

func AnalyticsActivityAgentsSQL(where string, args []any, localDate string, localDateArgs []any, bucketExpr, messageFilter string, modelArgs []any, dialect db.QueryDialect) (string, []any) {
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, modelArgs...)
	return `
		WITH filtered_sessions AS (
			SELECT s.id, s.agent, ` + localDate + ` AS local_date
			FROM sessions s
			WHERE ` + where + `
		)
		SELECT ` + bucketExpr + ` AS bucket, fs.agent, ` + dialect.SignedAggregate("COUNT(*)") + ` AS messages
		FROM filtered_sessions fs
		JOIN messages m ON m.session_id = fs.id
		` + messageFilter + `
		GROUP BY bucket, fs.agent
		ORDER BY bucket, fs.agent`, queryArgs
}

func analyticsModelsSQL(ids []string) (string, []any) {
	ph, args := db.InPlaceholders(ids)
	return `
			SELECT DISTINCT model
			FROM messages
			WHERE session_id IN ` + ph + `
				AND COALESCE(model, '') <> ''
			ORDER BY model`, args
}

func analyticsModelTimesSQL(ids []string) (string, []any) {
	ph, args := db.InPlaceholders(ids)
	return `
			SELECT model, timestamp
			FROM messages
			WHERE session_id IN ` + ph + `
				AND COALESCE(model, '') <> ''`, args
}

func analyticsSignalMessagesSQL(ids []string) (string, []any) {
	ph, args := db.InPlaceholders(ids)
	return `SELECT session_id, ordinal, role, content,
			timestamp, is_system, has_tool_use, COALESCE(source_subtype, '')
		FROM messages
		WHERE session_id IN ` + ph + `
		ORDER BY session_id, ordinal`, args
}

func AnalyticsWhere(f db.AnalyticsFilter, dateCol, tablePrefix string, includeDate bool, timestampParam, localDate string, localDateArgs []any, dialect db.QueryDialect) (string, []any) {
	var dates []string
	var args []any
	if includeDate {
		from, to := PaddedDateBounds(f.From, f.To)
		if f.From != "" {
			dates = append(dates, dateCol+" >= "+timestampParam)
			args = append(args, from)
		}
		if f.To != "" {
			dates = append(dates, dateCol+" <= "+timestampParam)
			args = append(args, to)
		}
		if f.From != "" {
			dates = append(dates, localDate+" >= ?")
			args = append(args, append(localDateArgs, f.From)...)
		}
		if f.To != "" {
			dates = append(dates, localDate+" <= ?")
			args = append(args, append(localDateArgs, f.To)...)
		}
	}

	b := db.NewQueryBuilder(dialect, 0)
	if f.ActiveSince != "" {
		if parsed, ok := ParseAnalyticsTime(f.ActiveSince); ok {
			f.ActiveSince = parsed.Format(time.RFC3339)
		}
	}
	where := db.BuildAnalyticsWhere(f, b, tablePrefix, "", dates)
	args = append(args, b.Args()...)
	return where, args
}

func AnalyticsMessageWindowPred(col, from, to, timestampParam string) (string, []any) {
	var preds []string
	var args []any
	if from != "" {
		preds = append(preds, col+" >= "+timestampParam)
		args = append(args, from)
	}
	if to != "" {
		preds = append(preds, col+" < "+timestampParam)
		args = append(args, to)
	}
	if len(preds) == 0 {
		return "", nil
	}
	return "(" + col + " IS NULL OR (" + strings.Join(preds, " AND ") + "))", args
}

func analyticsCandidateMessagesSQL(ph string, includeContent bool) string {
	contentExpr := "''"
	if includeContent {
		contentExpr = "COALESCE(content, '')"
	}
	return `
			SELECT session_id, ordinal, role, COALESCE(source_subtype, ''), is_system, COALESCE(model, ''),
				has_thinking, has_tool_use, timestamp,
				output_tokens, has_output_tokens, content_length, ` + contentExpr + `
			FROM messages
			WHERE session_id IN ` + ph + `
			ORDER BY session_id, ordinal`
}

func AnalyticsTrendsSQL(systemPrefix string, dialect db.QueryDialect) string {
	return `
		SELECT m.session_id, m.ordinal, m.role, m.is_system,
			COALESCE(m.model, ''), m.content, m.timestamp,
			s.started_at, s.created_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.deleted_at IS NULL
			AND m.role IN ('user', 'assistant')
			AND m.is_system = ` + dialect.FalseLiteral() + `
			AND ` + systemPrefix + `
		ORDER BY m.session_id, m.ordinal`
}

const AnalyticsSessionColumns = `id, project, machine, agent, first_message,
			COALESCE(display_name, session_name) AS display_name,
			started_at, ended_at, created_at, message_count,
			user_message_count, total_output_tokens,
			has_total_output_tokens, is_automated,
			termination_status, health_score, health_grade, outcome,
			outcome_confidence, tool_failure_signal_count,
			tool_retry_count, edit_churn_count, compaction_count,
			mid_task_compaction_count, context_pressure_max,
			quality_signal_version, short_prompt_count,
			unstructured_start, missing_success_criteria_count,
			missing_verification_count, duplicate_prompt_count,
			no_code_context_count, runaway_tool_loop_count`

func AnalyticsCSVPredicate(col, raw string, dialect db.QueryDialect) (string, []any) {
	b := db.NewQueryBuilder(dialect, 0)
	pred := b.ValuesPredicate(col, db.CSVFilterValues(raw), true)
	return pred, b.Args()
}

func AnalyticsMessageFilterClause(col, raw string, dialect db.QueryDialect) string {
	pred, _ := AnalyticsCSVPredicate(col, raw, dialect)
	if pred == "" {
		return ""
	}
	return "WHERE " + pred
}

func AnalyticsAndClause(pred string) string {
	if pred == "" {
		return ""
	}
	return " AND " + pred
}
