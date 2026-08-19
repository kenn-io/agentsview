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
}

func (s *BunStore) bunActivityReportScopeFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, q activity.Query,
) ([]activity.SessionMeta, error) {
	builder, err := newBunAnalyticsSQL(
		s.backend.Capabilities().AnalyticsDialect, f,
	)
	if err != nil {
		return nil, err
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
	WHERE (`+s.backend.TimestampOrderExpr(activityEnd)+` >= `+startBound+`
		OR EXISTS (
			SELECT 1 FROM tool_result_events AS terminal
			WHERE terminal.session_id = session.id
				AND terminal.source = 'tool_execution'
				AND terminal.status IN ('completed', 'errored')
				AND `+bunNullableTimestamp("terminal.timestamp")+` IS NOT NULL
				AND `+s.backend.TimestampOrderExpr(
			bunNullableTimestamp("terminal.timestamp"))+` >= `+startBound+`))
		AND `+s.backend.TimestampOrderExpr(activityStart)+` < `+endBound,
			bunmodel.NewTimestamp(q.RangeStart),
			bunmodel.NewTimestamp(q.RangeStart),
			bunmodel.NewTimestamp(q.RangeEnd)),
	})
	query := `WITH ` + ctes.SQL + `
	SELECT session.id AS session_id,
		COALESCE(NULLIF(session.display_name, ''), NULLIF(session.session_name, ''),
			NULLIF(session.project, ''), session.id) AS title,
		session.project, session.agent, session.machine,
		session.started_at, session.ended_at,
		session.is_automated
	FROM activity_report_sessions AS session
	ORDER BY session.id ASC`
	var rows []bunActivityReportScopeRow
	if err := store.NewRaw(query, ctes.Args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun activity report scope: %w", err)
	}
	sessions := make([]activity.SessionMeta, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, activity.SessionMeta{
			SessionID: row.SessionID, Title: row.Title, Project: row.Project,
			Agent: row.Agent, Machine: row.Machine,
			StartedAt:   bunAnalyticsTimeString(row.StartedAt),
			EndedAt:     bunAnalyticsTimeString(row.EndedAt),
			IsAutomated: row.IsAutomated,
		})
	}
	return sessions, nil
}

// bunActivityReportCandidateSourceFrom streams timestamp-paired activity
// candidates directly from the guarded Bun snapshot. SQL owns adjacency and
// terminal tool-event pairing; Go only scans the bounded candidate stream.
func (s *BunStore) bunActivityReportCandidateSourceFrom(
	store bun.IDB, ids []string, q activity.Query,
) activity.CandidateSource {
	return func(
		ctx context.Context, yield func(activity.IntervalCandidate) error,
	) error {
		if len(ids) == 0 {
			return nil
		}
		orderedTimestamp := func(operand string) string {
			return s.backend.TimestampOrderExpr(bunNullableTimestamp(operand))
		}
		orderedBound := func(operand string) string {
			return s.backend.TimestampOrderExpr(operand)
		}
		terminalTimestamp := orderedTimestamp("terminal.timestamp")
		orderedTerminalTimestamp := orderedTimestamp("te.timestamp")
		nextTerminalTimestamp := orderedTimestamp("twm.next_terminal_timestamp")
		nextMessageTimestamp := orderedTimestamp("twm.next_message_timestamp")
		lastMessageTimestamp := orderedTimestamp("lm.timestamp")
		messageTimestamp := orderedTimestamp("m.timestamp")
		successorTimestamp := orderedTimestamp("next.timestamp")
		query := `WITH
terminal_events AS (
	SELECT terminal.session_id,
		terminal.tool_call_message_ordinal AS ordinal,
		terminal.call_index, terminal.event_index, terminal.timestamp
	FROM tool_result_events AS terminal
	WHERE terminal.session_id IN (?0)
		AND terminal.source = 'tool_execution'
		AND terminal.status IN ('completed', 'errored')
		AND ` + bunNullableTimestamp("terminal.timestamp") + ` IS NOT NULL
		AND ` + terminalTimestamp + ` >= ` + orderedBound("?1") + `
),
terminal_sessions AS (
	SELECT DISTINCT session_id FROM terminal_events
),
ordered_terminal AS (
	SELECT te.*,
		LEAD(te.ordinal) OVER terminal_order AS next_terminal_ordinal,
		LEAD(te.timestamp) OVER terminal_order AS next_terminal_timestamp
	FROM terminal_events AS te
	WINDOW terminal_order AS (
		PARTITION BY te.session_id
		ORDER BY ` + orderedTerminalTimestamp + `, te.call_index, te.event_index
	)
),
terminal_with_message AS (
	SELECT ot.*, next_message.ordinal AS next_message_ordinal,
		next_message.timestamp AS next_message_timestamp,
		next_message.role AS next_message_role,
		next_message.model AS next_message_model
	FROM ordered_terminal AS ot
	LEFT JOIN messages AS next_message ON next_message.id = (
		SELECT next.id FROM messages AS next
		WHERE next.session_id = ot.session_id
			AND next.ordinal > ot.ordinal
			AND ` + bunNullableTimestamp("next.timestamp") + ` IS NOT NULL
			AND ` + successorTimestamp + ` > ` + orderedTimestamp("ot.timestamp") + `
		ORDER BY next.ordinal
		LIMIT 1
	)
),
last_messages AS (
	SELECT message.session_id, message.ordinal, message.timestamp
	FROM terminal_sessions AS terminal_session
	JOIN messages AS message ON message.id = (
		SELECT latest.id FROM messages AS latest
		WHERE latest.session_id = terminal_session.session_id
			AND ` + bunNullableTimestamp("latest.timestamp") + ` IS NOT NULL
		ORDER BY latest.ordinal DESC
		LIMIT 1
	)
),
first_tail_events AS (
	SELECT lm.session_id, lm.ordinal, lm.timestamp,
		te.call_index, te.event_index, te.timestamp AS terminal_timestamp,
		ROW_NUMBER() OVER (
			PARTITION BY lm.session_id
			ORDER BY ` + orderedTerminalTimestamp + `, te.call_index, te.event_index
		) AS row_num
	FROM last_messages AS lm
	JOIN terminal_events AS te ON te.session_id = lm.session_id
	WHERE ` + orderedTerminalTimestamp + ` > ` + lastMessageTimestamp + `
),
terminal_candidates AS (
	SELECT twm.session_id, twm.ordinal AS start_ordinal,
		CASE WHEN twm.next_terminal_timestamp IS NOT NULL
				AND (twm.next_message_timestamp IS NULL
					OR ` + nextTerminalTimestamp + ` < ` + nextMessageTimestamp + `)
			THEN twm.next_terminal_ordinal ELSE twm.next_message_ordinal END AS end_ordinal,
		twm.timestamp AS start_timestamp,
		CASE WHEN twm.next_terminal_timestamp IS NOT NULL
				AND (twm.next_message_timestamp IS NULL
					OR ` + nextTerminalTimestamp + ` < ` + nextMessageTimestamp + `)
			THEN twm.next_terminal_timestamp ELSE twm.next_message_timestamp END AS end_timestamp,
		CASE WHEN twm.next_terminal_timestamp IS NOT NULL
				AND (twm.next_message_timestamp IS NULL
					OR ` + nextTerminalTimestamp + ` < ` + nextMessageTimestamp + `)
			THEN 'tool' ELSE twm.next_message_role END AS closing_role,
		CASE WHEN twm.next_terminal_timestamp IS NOT NULL
				AND (twm.next_message_timestamp IS NULL
					OR ` + nextTerminalTimestamp + ` < ` + nextMessageTimestamp + `)
			THEN '' ELSE twm.next_message_model END AS closing_model,
		twm.call_index, twm.event_index
	FROM terminal_with_message AS twm
	UNION ALL
	SELECT fte.session_id, fte.ordinal, fte.ordinal,
		fte.timestamp, fte.terminal_timestamp, 'tool', '',
		fte.call_index, fte.event_index
	FROM first_tail_events AS fte
	WHERE fte.row_num = 1
),
all_candidates AS (
	SELECT candidate.session_id, candidate.start_ordinal,
		candidate.end_ordinal, candidate.start_timestamp,
		candidate.end_timestamp, candidate.closing_role,
		candidate.closing_model,
		COALESCE((
			SELECT prior.model FROM messages AS prior
			WHERE prior.session_id = candidate.session_id
				AND prior.ordinal <= candidate.start_ordinal
				AND prior.role = 'assistant' AND prior.model != ''
			ORDER BY prior.ordinal DESC LIMIT 1
		), 'unknown') AS prior_model,
		1 AS source_order, candidate.call_index, candidate.event_index
	FROM terminal_candidates AS candidate
	WHERE candidate.end_timestamp IS NOT NULL
		AND ` + orderedTimestamp("candidate.start_timestamp") + ` < ` + orderedBound("?2") + `
	UNION ALL
	SELECT m.session_id, m.ordinal, successor.ordinal,
		m.timestamp, successor.timestamp, successor.role, successor.model,
		COALESCE((
			SELECT prior.model FROM messages AS prior
			WHERE prior.session_id = m.session_id
				AND prior.ordinal <= m.ordinal
				AND prior.role = 'assistant' AND prior.model != ''
				AND ` + bunNullableTimestamp("prior.timestamp") + ` IS NOT NULL
				AND ` + orderedTimestamp("prior.timestamp") + ` > (
					SELECT ` + orderedTimestamp("prior_previous.timestamp") + `
					FROM messages AS prior_previous
					WHERE prior_previous.session_id = prior.session_id
						AND prior_previous.ordinal < prior.ordinal
						AND ` + bunNullableTimestamp("prior_previous.timestamp") + ` IS NOT NULL
					ORDER BY prior_previous.ordinal DESC LIMIT 1
				)
			ORDER BY prior.ordinal DESC LIMIT 1
		), 'unknown') AS prior_model,
		0 AS source_order, 0 AS call_index, 0 AS event_index
	FROM messages AS m
	JOIN messages AS successor ON successor.id = (
		SELECT next.id FROM messages AS next
		WHERE next.session_id = m.session_id
			AND next.ordinal > m.ordinal
			AND ` + bunNullableTimestamp("next.timestamp") + ` IS NOT NULL
		ORDER BY next.ordinal LIMIT 1
	)
	WHERE m.session_id IN (?0)
		AND ` + bunNullableTimestamp("m.timestamp") + ` IS NOT NULL
		AND ` + messageTimestamp + ` >= ` + orderedBound("?1") + `
		AND ` + messageTimestamp + ` < ` + orderedBound("?2") + `
)
SELECT session_id, start_ordinal, end_ordinal,
	start_timestamp, end_timestamp, closing_role, closing_model, prior_model
FROM all_candidates
ORDER BY ` + orderedTimestamp("start_timestamp") + `,
	session_id, start_ordinal, source_order, call_index, event_index`
		formatted := store.NewRaw(
			query, bun.List(ids),
			bunmodel.NewTimestamp(q.RangeStart.Add(
				-time.Duration(q.GapCapSeconds)*time.Second,
			)),
			bunmodel.NewTimestamp(q.EffectiveEnd),
		).String()
		rows, err := store.QueryContext(ctx, formatted)
		if err != nil {
			return fmt.Errorf("querying Bun activity report candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var candidate activity.IntervalCandidate
			var start, end bunmodel.Timestamp
			if err := rows.Scan(
				&candidate.SessionID, &candidate.StartOrdinal,
				&candidate.EndOrdinal, &start, &end,
				&candidate.ClosingRole, &candidate.ClosingModel,
				&candidate.PriorModel,
			); err != nil {
				return fmt.Errorf("scanning Bun activity report candidate: %w", err)
			}
			candidate.Start = start.Time
			candidate.End = end.Time
			if err := yield(candidate); err != nil {
				return err
			}
		}
		return rows.Err()
	}
}

// ActivityReportCandidateSource exposes the shared Bun pairing stream for
// cross-backend contract tests.
func (s *BunStore) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return func(
		ctx context.Context, yield func(activity.IntervalCandidate) error,
	) error {
		return s.consistentView(ctx, func(store bun.IDB) error {
			return s.bunActivityReportCandidateSourceFrom(store, ids, q)(ctx, yield)
		})
	}
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
