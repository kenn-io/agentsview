package db

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type bunActivityReportScopeRow struct {
	SessionID   string              `bun:"session_id"`
	Title       string              `bun:"title"`
	Project     string              `bun:"project"`
	Agent       string              `bun:"agent"`
	Machine     string              `bun:"machine"`
	StartedAt   *bunmodel.Timestamp `bun:"started_at"`
	EndedAt     *bunmodel.Timestamp `bun:"ended_at"`
	IsAutomated bool                `bun:"is_automated"`
	Ordinal     *int                `bun:"ordinal"`
	Role        *string             `bun:"role"`
	Timestamp   *bunmodel.Timestamp `bun:"timestamp"`
	Model       *string             `bun:"model"`
}

func (s *BunStore) bunActivityReportScopeFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, q activity.Query,
) ([]activity.SessionMeta, []activity.ActivityEvent, error) {
	builder, err := newBunAnalyticsSQL(
		s.backend.Capabilities().AnalyticsDialect, f,
	)
	if err != nil {
		return nil, nil, err
	}
	filtered := builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, bunAnalyticsFactDateScope,
	)
	latestMessage := `(SELECT message.timestamp FROM messages AS message
		WHERE message.session_id = session.id
		AND ` + bunNullableTimestamp("message.timestamp") + ` IS NOT NULL
		ORDER BY ` + s.backend.TimestampOrderExpr(
		bunNullableTimestamp("message.timestamp")) + ` DESC,
		message.ordinal DESC LIMIT 1)`
	activityStart := "COALESCE(" + bunNullableTimestamp("session.started_at") +
		", session.created_at)"
	activityEnd := "COALESCE(" + bunNullableTimestamp("session.ended_at") + ", " +
		"CASE WHEN " + s.backend.TimestampOrderExpr(latestMessage) + " >= " +
		s.backend.TimestampOrderExpr(activityStart) + " THEN " + latestMessage + " END, " +
		bunNullableTimestamp("session.started_at") +
		", session.created_at)"
	startBound := s.backend.TimestampOrderExpr("?")
	endBound := s.backend.TimestampOrderExpr("?")
	ctes := renderBunCTEs(filtered, BunCTEFragment{
		Name: "activity_report_sessions",
		Query: BunSQL(`SELECT session.*
	FROM `+bunAnalyticsFilteredSessionsCTE+` AS session
	WHERE `+s.backend.TimestampOrderExpr(activityEnd)+` >= `+startBound+`
		AND `+s.backend.TimestampOrderExpr(activityStart)+` < `+endBound,
			q.RangeStart.UTC().Format(time.RFC3339Nano),
			q.RangeEnd.UTC().Format(time.RFC3339Nano)),
	})
	query := `WITH ` + ctes.SQL + `
	SELECT session.id AS session_id,
		COALESCE(NULLIF(session.display_name, ''), NULLIF(session.session_name, ''),
			NULLIF(session.project, ''), session.id) AS title,
		session.project, session.agent, session.machine,
		session.started_at, session.ended_at,
		session.is_automated,
		message.ordinal, message.role, message.timestamp,
		message.model
	FROM activity_report_sessions AS session
	LEFT JOIN messages AS message ON message.session_id = session.id
		AND ` + bunNullableTimestamp("message.timestamp") + ` IS NOT NULL
	ORDER BY session.id ASC, message.ordinal ASC`
	var rows []bunActivityReportScopeRow
	if err := store.NewRaw(query, ctes.Args...).Scan(ctx, &rows); err != nil {
		return nil, nil, fmt.Errorf("querying Bun activity report scope: %w", err)
	}
	sessions := make([]activity.SessionMeta, 0)
	events := make([]activity.ActivityEvent, 0)
	seen := make(map[string]struct{})
	for _, row := range rows {
		if _, ok := seen[row.SessionID]; !ok {
			seen[row.SessionID] = struct{}{}
			sessions = append(sessions, activity.SessionMeta{
				SessionID: row.SessionID, Title: row.Title, Project: row.Project,
				Agent: row.Agent, Machine: row.Machine,
				StartedAt:   bunAnalyticsTimeString(row.StartedAt),
				EndedAt:     bunAnalyticsTimeString(row.EndedAt),
				IsAutomated: row.IsAutomated,
			})
		}
		if row.Ordinal == nil || row.Role == nil || row.Timestamp == nil || row.Model == nil {
			continue
		}
		events = append(events, activity.ActivityEvent{
			SessionID: row.SessionID, Ordinal: *row.Ordinal, Role: *row.Role,
			Timestamp: bunAnalyticsTimeString(row.Timestamp), Model: *row.Model,
		})
	}
	return sessions, events, nil
}

func (s *BunStore) bunActivityReportUsageProjectionsFrom(
	ctx context.Context, store bun.IDB, ids []string, filter UsageFilter,
) ([]bunUsageProjection, error) {
	projections, err := s.loadBunUsageProjections(ctx, store, filter, false, ids)
	if err != nil || len(projections) == 0 {
		return projections, err
	}
	var peers []bunUsageProjection
	query := store.NewSelect().TableExpr("messages AS m").
		ColumnExpr(bunMessageUsageColumns()+","+bunUsageSessionColumns).
		Join("JOIN sessions AS s ON s.id = m.session_id").
		Where("s.deleted_at IS NULL").
		Where("m.token_usage != ?", "").
		Where("m.model != ?", "").Where("m.model != ?", "<synthetic>").
		Where("m.session_id NOT IN (?)", bun.List(ids)).
		Where(`EXISTS (
			SELECT 1 FROM messages AS candidate
			WHERE candidate.session_id IN (?)
				AND candidate.claude_message_id != ''
				AND candidate.claude_request_id != ''
				AND candidate.claude_message_id = m.claude_message_id
				AND candidate.claude_request_id = m.claude_request_id
		)`, bun.List(ids))
	query = appendBunUsageFilters(
		query, filter, "m.model", s.backend.TimestampOrderExpr, time.Now().UTC(),
	)
	query = appendBunUsageBounds(
		query, filter, "m.timestamp", true, func(column string) string { return column },
		s.backend.TimestampOrderExpr,
	)
	if err := query.Scan(ctx, &peers); err != nil {
		return nil, fmt.Errorf("querying activity-report snapshot peers: %w", err)
	}
	for _, peer := range peers {
		peer.CostStatus = ""
		peer.CostSource = ""
		peer.UsageDedupKey = ""
		projections = append(projections, peer)
	}
	return projections, nil
}
