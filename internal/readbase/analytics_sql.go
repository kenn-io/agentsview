package readbase

import "go.kenn.io/agentsview/internal/db"

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
