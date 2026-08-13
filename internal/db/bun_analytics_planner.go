package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const bunAnalyticsTopSessionLimit = 10

type bunTopSessionRow struct {
	ID                string              `bun:"id"`
	Project           string              `bun:"project"`
	FirstMessage      *string             `bun:"first_message"`
	DisplayName       *string             `bun:"display_name"`
	SessionName       *string             `bun:"session_name"`
	MessageCount      int                 `bun:"message_count"`
	OutputTokens      int                 `bun:"output_tokens"`
	DurationMin       float64             `bun:"duration_min"`
	ActiveDurationMin float64             `bun:"active_duration_min"`
	StartedAt         *bunmodel.Timestamp `bun:"started_at"`
	EndedAt           *bunmodel.Timestamp `bun:"ended_at"`
	TerminationStatus *string             `bun:"termination_status"`
}

func (s *BunStore) bunAnalyticsTopSessionsFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, metric string,
) ([]TopSession, error) {
	dialect := s.backend.Capabilities().AnalyticsDialect
	builder, err := newBunAnalyticsSQL(dialect, f)
	if err != nil {
		return nil, err
	}
	ctes := []BunCTEFragment{builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, bunAnalyticsSessionDateScope,
	)}
	qualify := strings.TrimSpace(f.Model) != "" || f.HasTimeFilter()
	if qualify {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
	}
	joins := ""
	if qualify {
		joins += "\nJOIN " + bunAnalyticsQualifiedSessionsCTE +
			" AS qualified ON qualified.session_id = session.id"
	}
	messageCount := "session.message_count"
	outputTokens := "session.total_output_tokens"
	hasOutputTokens := "session.has_total_output_tokens"
	if strings.TrimSpace(f.Model) != "" {
		ctes = append(ctes, BunCTEFragment{
			Name: "analytics_top_message_stats",
			Query: BunSQL(`SELECT session_id,
		CAST(COUNT(*) AS BIGINT) AS message_count,
		CAST(COALESCE(SUM(CASE WHEN has_output_tokens THEN output_tokens ELSE 0 END), 0)
			AS BIGINT)
			AS output_tokens,
		MAX(CASE WHEN has_output_tokens THEN 1 ELSE 0 END) AS has_output_tokens
	FROM ` + bunAnalyticsScopedMessagesCTE + `
	GROUP BY session_id`),
		})
		joins += "\nJOIN analytics_top_message_stats AS message_stats" +
			" ON message_stats.session_id = session.id"
		messageCount = "message_stats.message_count"
		outputTokens = "message_stats.output_tokens"
		hasOutputTokens = "message_stats.has_output_tokens"
	}

	messageTimestamp := bunNullableTimestamp("message.timestamp")
	previousTimestamp := "LAG(" + messageTimestamp + `) OVER (
		PARTITION BY message.session_id ORDER BY message.ordinal)`
	gap := dialect.DurationSeconds(BunSQL(previousTimestamp), BunSQL(messageTimestamp))
	ctes = append(ctes,
		BunCTEFragment{Name: "analytics_top_message_gaps", Query: BunSQL(`SELECT
		message.session_id, `+gap.SQL+` AS gap_seconds
	FROM messages AS message
	JOIN `+bunAnalyticsFilteredSessionsCTE+` AS session
		ON session.id = message.session_id`, gap.Args...)},
		BunCTEFragment{Name: "analytics_top_active_duration", Query: BunSQL(`SELECT
		session_id,
		COALESCE(SUM(CASE
			WHEN gap_seconds > 0 AND gap_seconds < ? THEN gap_seconds
			WHEN gap_seconds >= ? THEN ?
			ELSE 0 END), 0.0) / 60.0 AS active_duration_min
	FROM analytics_top_message_gaps
	GROUP BY session_id`, ActiveGapCapSec, ActiveGapCapSec, ActiveGapCapSec)},
	)

	wallDuration := dialect.DurationSeconds(
		BunSQL(bunNullableTimestamp("session.started_at")),
		BunSQL(bunNullableTimestamp("session.ended_at")),
	)
	where := "1 = 1"
	orderBy := messageCount
	switch metric {
	case "duration":
		where = bunNullableTimestamp("session.started_at") + " IS NOT NULL AND " +
			bunNullableTimestamp("session.ended_at") + " IS NOT NULL AND " +
			s.backend.TimestampOrderExpr(bunNullableTimestamp("session.ended_at")) +
			" >= " + s.backend.TimestampOrderExpr(bunNullableTimestamp("session.started_at"))
		orderBy = "COALESCE(active.active_duration_min, 0)"
	case "output_tokens":
		where = hasOutputTokens + " = TRUE"
		orderBy = outputTokens
	default:
	}
	with := renderBunCTEs(ctes...)
	query := `WITH ` + with.SQL + `
	SELECT session.id, session.project, session.first_message,
		session.display_name, session.session_name,
		` + messageCount + ` AS message_count,
		` + outputTokens + ` AS output_tokens,
		CAST(CASE WHEN session.started_at IS NOT NULL AND session.ended_at IS NOT NULL
			THEN ` + wallDuration.SQL + ` / 60.0 ELSE 0.0 END
			AS DOUBLE PRECISION) AS duration_min,
		COALESCE(active.active_duration_min, 0.0) AS active_duration_min,
		session.started_at, session.ended_at,
		session.termination_status
	FROM ` + bunAnalyticsFilteredSessionsCTE + ` AS session` + joins + `
	LEFT JOIN analytics_top_active_duration AS active ON active.session_id = session.id
	WHERE ` + where + `
	ORDER BY ` + orderBy + ` DESC, session.id ASC
	LIMIT ?`
	args := append(append(append([]any(nil), with.Args...), wallDuration.Args...),
		bunAnalyticsTopSessionLimit)
	var rows []bunTopSessionRow
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun analytics top sessions: %w", err)
	}
	result := make([]TopSession, 0, len(rows))
	for _, row := range rows {
		displayName := row.DisplayName
		if displayName == nil {
			displayName = row.SessionName
		}
		result = append(result, TopSession{
			ID: row.ID, Project: row.Project, FirstMessage: row.FirstMessage,
			DisplayName: displayName, MessageCount: row.MessageCount,
			OutputTokens: row.OutputTokens, DurationMin: row.DurationMin,
			ActiveDurationMin: row.ActiveDurationMin,
			StartedAt:         bunAnalyticsStringPtr(row.StartedAt),
			EndedAt:           bunAnalyticsStringPtr(row.EndedAt),
			TerminationStatus: row.TerminationStatus,
		})
	}
	return result, nil
}
