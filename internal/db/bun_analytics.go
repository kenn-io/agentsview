package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/signals"
)

const bunAnalyticsSessionAlias = `"session"`

const bunAnalyticsContentSessionBatchSize = 32

func (s *BunStore) bunAnalyticsSessionsFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, applyDate bool,
) ([]bunmodel.Session, error) {
	var rows []bunmodel.Session
	query := s.bunAnalyticsSessionSelect(store, f, applyDate).
		ExcludeColumn("started_at", "ended_at", "signals_pending_since",
			"deleted_at", "local_modified_at", "created_at").
		OrderExpr(bunAnalyticsSessionAlias + ".id ASC")
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying Bun analytics sessions: %w", err)
	}
	if err := loadBunAnalyticsSessionTimes(ctx, store, rows); err != nil {
		return nil, err
	}

	loc := f.location()
	filtered := rows[:0]
	for _, row := range rows {
		if f.ActiveSince != "" {
			cutoff, err := parseTimestamp(f.ActiveSince)
			if err == nil {
				activityTime := bunAnalyticsSessionTime(row)
				if row.EndedAt != nil {
					activityTime = row.EndedAt.UTC()
				}
				if activityTime.Before(cutoff) {
					continue
				}
			}
		}
		if applyDate {
			date := bunAnalyticsSessionTime(row).In(loc).Format("2006-01-02")
			if !inDateRange(date, f.From, f.To) {
				continue
			}
		}
		filtered = append(filtered, row)
	}
	rows = filtered
	if (f.DayOfWeek != nil || f.Hour != nil) && len(rows) > 0 {
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(rows))
		if err != nil {
			return nil, err
		}
		keep := make(map[string]bool, len(rows))
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, false)
			if scopeErr != nil {
				return nil, scopeErr
			}
			for id, scoped := range scope.MessagesBySession() {
				keep[id] = len(scoped) > 0
			}
		} else {
			flt := f.messageScopeFilter()
			for _, message := range messages {
				t, ok := bunAnalyticsMessageLocalTime(message, loc)
				if flt.MatchesDayHour(t, ok) {
					keep[message.SessionID] = true
				}
			}
		}
		filtered = rows[:0]
		for _, row := range rows {
			if keep[row.ID] {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	return rows, nil
}

func (s *BunStore) bunAnalyticsSessionSelect(
	store bun.IDB, f AnalyticsFilter, applyDate bool,
) *bun.SelectQuery {
	query := store.NewSelect().Model((*bunmodel.Session)(nil)).
		Where(bunAnalyticsSessionAlias + ".message_count > 0").
		Where(bunAnalyticsSessionAlias + ".deleted_at IS NULL")

	switch {
	case f.IncludeSubagents && f.IncludeForks:
	case f.IncludeSubagents:
		query = query.Where(bunAnalyticsSessionAlias + ".relationship_type != 'fork'")
	case f.IncludeForks:
		query = query.Where(bunAnalyticsSessionAlias + ".relationship_type != 'subagent'")
	default:
		query = query.Where(bunAnalyticsSessionAlias +
			".relationship_type NOT IN ('subagent', 'fork')")
	}
	query = appendBunAnalyticsCSVFilter(query,
		bunAnalyticsSessionAlias+".machine", f.Machine)
	query = appendBunAnalyticsCSVFilter(query,
		bunAnalyticsSessionAlias+".agent", f.Agent)
	if f.Project != "" {
		query = query.Where(bunAnalyticsSessionAlias+".project = ?", f.Project)
	}
	if f.GitBranch != "" {
		clause, args := BranchPairClauseArgs(
			bunAnalyticsSessionAlias+".project",
			bunAnalyticsSessionAlias+".git_branch", f.GitBranch, nil,
		)
		query = query.Where(clause, args...)
	}
	if f.MinUserMessages > 0 {
		query = query.Where(bunAnalyticsSessionAlias+
			".user_message_count >= ?", f.MinUserMessages)
	}
	scope := normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if f.IncludeSubagents {
			query = query.Where("("+bunAnalyticsSessionAlias+
				".user_message_count > 1 OR "+bunAnalyticsSessionAlias+
				".relationship_type = 'subagent' OR "+bunAnalyticsSessionAlias+
				".is_automated = ?)", true)
		} else if scope == "human" {
			query = query.Where(bunAnalyticsSessionAlias +
				".user_message_count > 1")
		} else {
			query = query.Where("("+bunAnalyticsSessionAlias+
				".user_message_count > 1 OR "+bunAnalyticsSessionAlias+
				".is_automated = ?)", true)
		}
	}
	switch scope {
	case "human":
		query = query.Where(bunAnalyticsSessionAlias+".is_automated = ?", false)
	case "automated":
		query = query.Where(bunAnalyticsSessionAlias+".is_automated = ?", true)
	}
	if f.ExcludeInteractive {
		query = query.Where(bunAnalyticsSessionAlias+".is_automated = ?", true)
	}
	query = appendBunAnalyticsTerminationFilter(
		query, f.Termination, s.backend.TimestampOrderExpr, time.Now().UTC(),
	)
	if models := csvFilterValues(f.Model); len(models) > 0 {
		query = query.Where("EXISTS (SELECT 1 FROM messages AS analytics_model "+
			"WHERE analytics_model.session_id = "+bunAnalyticsSessionAlias+
			".id AND analytics_model.model IN (?))", bun.List(models))
	}
	if applyDate && (f.From != "" || f.To != "") {
		from, to := f.utcRange()
		timestampOrderExpr := s.backend.TimestampOrderExpr
		expr := timestampOrderExpr("COALESCE(" +
			bunNullableTimestamp(bunAnalyticsSessionAlias+".started_at") + ", " +
			bunAnalyticsSessionAlias + ".created_at)")
		fromParam, toParam := timestampOrderExpr("?"), timestampOrderExpr("?")
		query = query.Where(expr+" >= "+fromParam, from).
			Where(expr+" <= "+toParam, to)
	}
	return query
}

type bunAnalyticsSessionTimes struct {
	ID                  string         `bun:"id"`
	StartedAt           sql.NullString `bun:"started_at"`
	EndedAt             sql.NullString `bun:"ended_at"`
	SignalsPendingSince sql.NullString `bun:"signals_pending_since"`
	DeletedAt           sql.NullString `bun:"deleted_at"`
	LocalModifiedAt     sql.NullString `bun:"local_modified_at"`
	CreatedAt           sql.NullString `bun:"created_at"`
}

func loadBunAnalyticsSessionTimes(
	ctx context.Context, store bun.IDB, sessions []bunmodel.Session,
) error {
	if len(sessions) == 0 {
		return nil
	}
	index := make(map[string]int, len(sessions))
	ids := make([]string, len(sessions))
	for i := range sessions {
		index[sessions[i].ID] = i
		ids[i] = sessions[i].ID
	}
	return queryChunked(ids, func(chunk []string) error {
		var rows []bunAnalyticsSessionTimes
		if err := store.NewSelect().Table("sessions").
			Column("id").
			ColumnExpr("CAST(started_at AS VARCHAR) AS started_at").
			ColumnExpr("CAST(ended_at AS VARCHAR) AS ended_at").
			ColumnExpr("CAST(signals_pending_since AS VARCHAR) AS signals_pending_since").
			ColumnExpr("CAST(deleted_at AS VARCHAR) AS deleted_at").
			ColumnExpr("CAST(local_modified_at AS VARCHAR) AS local_modified_at").
			ColumnExpr("CAST(created_at AS VARCHAR) AS created_at").
			Where("id IN (?)", bun.List(chunk)).Scan(ctx, &rows); err != nil {
			return fmt.Errorf("querying Bun analytics session timestamps: %w", err)
		}
		for _, row := range rows {
			i, ok := index[row.ID]
			if !ok {
				continue
			}
			sessions[i].StartedAt = parseBunAnalyticsTimestamp(row.StartedAt)
			sessions[i].EndedAt = parseBunAnalyticsTimestamp(row.EndedAt)
			sessions[i].SignalsPendingSince = parseBunAnalyticsTimestamp(row.SignalsPendingSince)
			sessions[i].DeletedAt = parseBunAnalyticsTimestamp(row.DeletedAt)
			sessions[i].LocalModifiedAt = parseBunAnalyticsTimestamp(row.LocalModifiedAt)
			if created := parseBunAnalyticsTimestamp(row.CreatedAt); created != nil {
				sessions[i].CreatedAt = *created
			}
		}
		return nil
	})
}

func parseBunAnalyticsTimestamp(value sql.NullString) *bunmodel.Timestamp {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed, err := bunmodel.ParseTimestamp(value.String)
	if err == nil {
		return &parsed
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05Z07",
	} {
		value, parseErr := time.Parse(layout, value.String)
		if parseErr == nil {
			parsed := bunmodel.NewTimestamp(value)
			return &parsed
		}
	}
	return nil
}

func appendBunAnalyticsCSVFilter(
	query *bun.SelectQuery, column, raw string,
) *bun.SelectQuery {
	values := csvFilterValues(raw)
	if len(values) == 0 {
		return query
	}
	return query.Where(column+" IN (?)", bun.List(values))
}

func appendBunAnalyticsTerminationFilter(
	query *bun.SelectQuery, raw string,
	timestampOrderExpr func(string) string,
	referenceTime time.Time,
) *bun.SelectQuery {
	return appendBunTerminationFilter(
		query, raw, bunAnalyticsSessionAlias, timestampOrderExpr, referenceTime,
	)
}

func bunAnalyticsMessagesFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string,
) ([]bunmodel.Message, error) {
	return bunAnalyticsMessagesProjectedFrom(ctx, store, sessionIDs, false)
}

func bunAnalyticsMessagesWithContentFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string,
) ([]bunmodel.Message, error) {
	return bunAnalyticsMessagesProjectedFrom(ctx, store, sessionIDs, true)
}

func bunAnalyticsMessagesProjectedFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string, includeContent bool,
) ([]bunmodel.Message, error) {
	if len(sessionIDs) == 0 {
		return []bunmodel.Message{}, nil
	}
	var out []bunmodel.Message
	err := queryChunked(sessionIDs, func(chunk []string) error {
		var rows []bunmodel.Message
		query := store.NewSelect().Model(&rows).Column(
			"session_id", "ordinal", "role", "has_thinking", "has_tool_use",
			"content_length", "is_system", "model", "output_tokens",
			"has_output_tokens", "is_sidechain",
		)
		if includeContent {
			query = query.Column("content")
		}
		if err := query.
			Where("session_id IN (?)", bun.List(chunk)).
			OrderExpr("session_id ASC, ordinal ASC").Scan(ctx); err != nil {
			return fmt.Errorf("querying Bun analytics messages: %w", err)
		}
		var timestamps []struct {
			SessionID string         `bun:"session_id"`
			Ordinal   int            `bun:"ordinal"`
			Timestamp sql.NullString `bun:"timestamp"`
		}
		if err := store.NewSelect().Table("messages").
			Column("session_id", "ordinal").
			ColumnExpr("CAST(timestamp AS VARCHAR) AS timestamp").
			Where("session_id IN (?)", bun.List(chunk)).
			Scan(ctx, &timestamps); err != nil {
			return fmt.Errorf("querying Bun analytics message timestamps: %w", err)
		}
		timestampMap := make(map[string]sql.NullString, len(timestamps))
		for _, row := range timestamps {
			timestampMap[fmt.Sprintf("%s\x00%d", row.SessionID, row.Ordinal)] = row.Timestamp
		}
		for i := range rows {
			rows[i].Timestamp = parseBunAnalyticsTimestamp(
				timestampMap[fmt.Sprintf("%s\x00%d", rows[i].SessionID, rows[i].Ordinal)],
			)
		}
		out = append(out, rows...)
		return nil
	})
	return out, err
}

func bunAnalyticsToolsFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string,
) ([]bunmodel.ToolCall, error) {
	if len(sessionIDs) == 0 {
		return []bunmodel.ToolCall{}, nil
	}
	var out []bunmodel.ToolCall
	err := queryChunked(sessionIDs, func(chunk []string) error {
		var rows []bunmodel.ToolCall
		if err := store.NewSelect().Model(&rows).Column(
			"session_id", "message_ordinal", "tool_name", "category",
			"call_index", "tool_use_id", "skill_name", "file_path",
		).
			Where("session_id IN (?)", bun.List(chunk)).
			OrderExpr("session_id ASC, message_ordinal ASC, call_index ASC").
			Scan(ctx); err != nil {
			return fmt.Errorf("querying Bun analytics tool calls: %w", err)
		}
		out = append(out, rows...)
		return nil
	})
	return out, err
}

func bunAnalyticsSessionIDs(rows []bunmodel.Session) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

func bunAnalyticsSessionTime(row bunmodel.Session) time.Time {
	if row.StartedAt != nil && !row.StartedAt.IsZero() {
		return row.StartedAt.UTC()
	}
	return row.CreatedAt.UTC()
}

func bunAnalyticsTimeString(value *bunmodel.Timestamp) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339Nano)
}

func bunAnalyticsStringPtr(value *bunmodel.Timestamp) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	formatted := value.Time.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func bunAnalyticsMessageLocalTime(
	row bunmodel.Message, loc *time.Location,
) (time.Time, bool) {
	if row.Timestamp == nil || row.Timestamp.IsZero() {
		return time.Time{}, false
	}
	return row.Timestamp.In(loc), true
}

func bunAnalyticsMessageScope(
	rows []bunmodel.Message, f AnalyticsFilter, includeContent bool,
) (*messageScope, error) {
	bySession := make(map[string][]ScopedMessage)
	reducer := NewScopeReducer(f.messageScopeFilter(), func(row ScopedMessage) {
		bySession[row.SessionID] = append(bySession[row.SessionID], row)
	})
	loc := f.location()
	for _, row := range rows {
		local, hasLocal := bunAnalyticsMessageLocalTime(row, loc)
		content := ""
		if includeContent {
			content = row.Content
		}
		if err := reducer.Push(MessageInput{
			SessionID: row.SessionID, Ordinal: row.Ordinal, Role: row.Role,
			Model: row.Model, IsSystem: row.IsSystem,
			Timestamp: bunAnalyticsTimeString(row.Timestamp), LocalTime: local,
			HasLocalTime: hasLocal, HasThinking: row.HasThinking,
			HasToolUse: row.HasToolUse, OutputTokens: row.OutputTokens,
			HasOutputTokens: row.HasOutputTokens, ContentLength: row.ContentLength,
			Content: content,
		}); err != nil {
			return nil, err
		}
	}
	return &messageScope{bySession: bySession}, nil
}

func bunAnalyticsMessagesBySession(
	rows []bunmodel.Message,
) map[string][]bunmodel.Message {
	out := make(map[string][]bunmodel.Message)
	for _, row := range rows {
		out[row.SessionID] = append(out[row.SessionID], row)
	}
	return out
}

func bunAnalyticsToolsBySession(
	rows []bunmodel.ToolCall,
) map[string][]bunmodel.ToolCall {
	out := make(map[string][]bunmodel.ToolCall)
	for _, row := range rows {
		out[row.SessionID] = append(out[row.SessionID], row)
	}
	return out
}

func bunAnalyticsModels(rows []bunmodel.Message) []string {
	set := map[string]struct{}{}
	for _, row := range rows {
		if row.Model != "" {
			set[row.Model] = struct{}{}
		}
	}
	return sortedSetKeys(set)
}

func (s *BunStore) GetAnalyticsSummary(
	ctx context.Context, f AnalyticsFilter,
) (AnalyticsSummary, error) {
	f.IncludeSubagents = true
	var result AnalyticsSummary
	err := s.consistentView(ctx, func(store bun.IDB) error {
		if bunAnalyticsSummaryCanAggregate(f) {
			var err error
			result, err = s.getBunAnalyticsSummaryAggregate(ctx, store, f)
			return err
		}
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		ids := bunAnalyticsSessionIDs(sessions)
		if strings.TrimSpace(f.Model) == "" && !f.HasTimeFilter() {
			models, err := bunAnalyticsDistinctModelsFrom(ctx, store, ids)
			if err != nil {
				return err
			}
			result = buildBunAnalyticsSummary(sessions, nil, f)
			result.Models = models
			return nil
		}
		messages, err := bunAnalyticsMessagesFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		result = buildBunAnalyticsSummary(sessions, messages, f)
		return nil
	})
	return result, err
}

func bunAnalyticsSummaryCanAggregate(f AnalyticsFilter) bool {
	return strings.TrimSpace(f.Model) == "" && !f.HasTimeFilter() &&
		f.ActiveSince == "" && f.location() == time.UTC
}

func bunAnalyticsUTCDateExpr(alias string) string {
	return "SUBSTR(CAST(COALESCE(" + bunNullableTimestamp(alias+".started_at") +
		", " + alias + ".created_at) AS VARCHAR), 1, 10)"
}

func appendBunAnalyticsExactDate(
	query *bun.SelectQuery, f AnalyticsFilter, alias string,
) *bun.SelectQuery {
	expr := bunAnalyticsUTCDateExpr(alias)
	if f.From != "" {
		query = query.Where(expr+" >= ?", f.From)
	}
	if f.To != "" {
		query = query.Where(expr+" <= ?", f.To)
	}
	return query
}

func (s *BunStore) getBunAnalyticsSummaryAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (AnalyticsSummary, error) {
	filteredColumns := bunAnalyticsSessionAlias + ".id, " +
		bunAnalyticsSessionAlias + ".project, " +
		bunAnalyticsSessionAlias + ".agent, " +
		bunAnalyticsSessionAlias + ".message_count, " +
		bunAnalyticsSessionAlias + ".total_output_tokens, " +
		bunAnalyticsSessionAlias + ".has_total_output_tokens, " +
		bunAnalyticsUTCDateExpr(bunAnalyticsSessionAlias) + " AS local_date"
	filtered := s.bunAnalyticsSessionSelect(store, f, false).
		ColumnExpr(filteredColumns)
	filtered = appendBunAnalyticsExactDate(filtered, f, bunAnalyticsSessionAlias)

	resultRows := newBunAnalyticsSummaryStreamModel()
	if err := store.NewRaw(bunAnalyticsSummaryAggregateSQL, filtered).
		Scan(ctx, resultRows); err != nil {
		return AnalyticsSummary{}, fmt.Errorf(
			"querying Bun analytics summary: %w", err,
		)
	}
	if !resultRows.seen {
		return AnalyticsSummary{
			Models: []string{}, Agents: map[string]*AgentSummary{},
		}, nil
	}
	return resultRows.summary, nil
}

const bunAnalyticsSummaryAggregateSQL = `
WITH filtered AS (?),
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
),
summary AS (
	SELECT COUNT(*) AS total_sessions,
		CAST(COALESCE(SUM(message_count), 0) AS BIGINT) AS total_messages,
		CAST(COALESCE(SUM(CASE WHEN has_total_output_tokens
			THEN total_output_tokens ELSE 0 END), 0) AS BIGINT) AS total_output_tokens,
		CAST(COALESCE(SUM(CASE WHEN has_total_output_tokens
			THEN 1 ELSE 0 END), 0) AS BIGINT) AS token_reporting_sessions,
		COUNT(DISTINCT project) AS active_projects,
		COUNT(DISTINCT local_date) AS active_days,
		CAST(COALESCE(ROUND(AVG(message_count), 1), 0)
			AS DOUBLE PRECISION) AS avg_messages,
		COALESCE((
			SELECT CAST(AVG(message_count) AS BIGINT)
			FROM ranked
			WHERE rn IN (
				CAST((n + 1) / 2 AS BIGINT),
				CAST((n + 2) / 2 AS BIGINT)
			)
		), 0) AS median_messages,
		COALESCE((
			SELECT message_count
			FROM ranked
			WHERE rn = CASE
				WHEN CAST(n * 0.9 AS BIGINT) + 1 < n
				THEN CAST(n * 0.9 AS BIGINT) + 1
				ELSE n
			END
			LIMIT 1
		), 0) AS p90_messages,
		COALESCE((
			SELECT project
			FROM project_totals
			ORDER BY messages DESC, project ASC
			LIMIT 1
		), '') AS most_active,
		CAST(COALESCE(ROUND((
			SELECT SUM(messages)
			FROM (
				SELECT messages
				FROM project_totals
				ORDER BY messages DESC
				LIMIT 3
			) AS top_projects
		) * 1.0 / NULLIF(SUM(message_count), 0), 3), 0)
			AS DOUBLE PRECISION) AS concentration
	FROM filtered
),
dimensions AS (
	SELECT 'agent' AS kind, agent AS name,
		COUNT(*) AS sessions,
		CAST(COALESCE(SUM(message_count), 0) AS BIGINT) AS messages
	FROM filtered
	GROUP BY agent
	UNION ALL
	SELECT 'model' AS kind, analytics_model.model AS name,
		CAST(0 AS BIGINT) AS sessions,
		CAST(0 AS BIGINT) AS messages
	FROM filtered AS filtered_session
	JOIN messages AS analytics_model
		ON analytics_model.session_id = filtered_session.id
	WHERE analytics_model.model != ''
	GROUP BY analytics_model.model
)
SELECT summary.total_sessions,
	summary.total_messages,
	summary.total_output_tokens,
	summary.token_reporting_sessions,
	summary.active_projects,
	summary.active_days,
	summary.avg_messages,
	summary.median_messages,
	summary.p90_messages,
	summary.most_active,
	summary.concentration,
	dimensions.kind,
	dimensions.name,
	dimensions.sessions AS dimension_sessions,
	dimensions.messages AS dimension_messages
FROM summary
LEFT JOIN dimensions ON 1 = 1
ORDER BY dimensions.kind ASC, dimensions.name ASC`

type bunAnalyticsSummaryStreamModel struct {
	seen     bool
	summary  AnalyticsSummary
	kind     sql.NullString
	name     sql.NullString
	sessions sql.NullInt64
	messages sql.NullInt64
	dest     [15]any
}

func newBunAnalyticsSummaryStreamModel() *bunAnalyticsSummaryStreamModel {
	model := &bunAnalyticsSummaryStreamModel{}
	model.dest = [15]any{
		&model.summary.TotalSessions,
		&model.summary.TotalMessages,
		&model.summary.TotalOutputTokens,
		&model.summary.TokenReportingSessions,
		&model.summary.ActiveProjects,
		&model.summary.ActiveDays,
		&model.summary.AvgMessages,
		&model.summary.MedianMessages,
		&model.summary.P90Messages,
		&model.summary.MostActive,
		&model.summary.Concentration,
		&model.kind,
		&model.name,
		&model.sessions,
		&model.messages,
	}
	return model
}

func (m *bunAnalyticsSummaryStreamModel) ScanRows(
	_ context.Context, rows *sql.Rows,
) (int, error) {
	count := 0
	for rows.Next() {
		m.kind = sql.NullString{}
		m.name = sql.NullString{}
		m.sessions = sql.NullInt64{}
		m.messages = sql.NullInt64{}
		if err := rows.Scan(m.dest[:]...); err != nil {
			return count, err
		}
		if !m.seen {
			m.summary.Models = []string{}
			m.summary.Agents = make(map[string]*AgentSummary)
			m.seen = true
		}
		if !m.kind.Valid || !m.name.Valid {
			count++
			continue
		}
		switch m.kind.String {
		case "agent":
			if m.sessions.Valid && m.messages.Valid {
				m.summary.Agents[m.name.String] = &AgentSummary{
					Sessions: int(m.sessions.Int64), Messages: int(m.messages.Int64),
				}
			}
		case "model":
			m.summary.Models = append(m.summary.Models, m.name.String)
		}
		count++
	}
	return count, rows.Err()
}

func (m *bunAnalyticsSummaryStreamModel) Value() any {
	return &m.summary
}

func bunAnalyticsDistinctModelsFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string,
) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	modelSet := make(map[string]struct{})
	if err := queryChunked(sessionIDs, func(chunk []string) error {
		var models []string
		if err := store.NewSelect().Table("messages").
			Column("model").Distinct().
			Where("session_id IN (?)", bun.List(chunk)).
			Where("model != ?", "").Scan(ctx, &models); err != nil {
			return fmt.Errorf("querying Bun analytics models: %w", err)
		}
		for _, model := range models {
			modelSet[model] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return sortedSetKeys(modelSet), nil
}

func buildBunAnalyticsSummary(
	sessions []bunmodel.Session, messages []bunmodel.Message, f AnalyticsFilter,
) AnalyticsSummary {
	out := AnalyticsSummary{Agents: map[string]*AgentSummary{}, Models: []string{}}
	if len(sessions) == 0 {
		return out
	}
	stats := map[string]MessageStats{}
	modelRows := messages
	if strings.TrimSpace(f.Model) != "" {
		scope, err := bunAnalyticsMessageScope(messages, f, false)
		if err == nil {
			stats = scope.StatsBySession()
			modelRows = nil
			for _, scoped := range scope.MessagesBySession() {
				for _, row := range scoped {
					if row.HasOutputTokens {
						modelRows = append(modelRows, bunmodel.Message{Model: f.Model})
					}
				}
			}
		}
	}
	days := map[string]struct{}{}
	projects := map[string]int{}
	counts := make([]int, 0, len(sessions))
	loc := f.location()
	for _, row := range sessions {
		messageCount := row.MessageCount
		outputTokens := row.TotalOutputTokens
		hasTokens := row.HasTotalOutputTokens
		if strings.TrimSpace(f.Model) != "" {
			stat := stats[row.ID]
			messageCount = stat.Messages
			outputTokens = stat.OutputTokens
			hasTokens = stat.HasOutputTokens
		}
		out.TotalSessions++
		out.TotalMessages += messageCount
		if hasTokens {
			out.TotalOutputTokens += outputTokens
			out.TokenReportingSessions++
		}
		date := bunAnalyticsSessionTime(row).In(loc).Format("2006-01-02")
		days[date] = struct{}{}
		projects[row.Project] += messageCount
		counts = append(counts, messageCount)
		if out.Agents[row.Agent] == nil {
			out.Agents[row.Agent] = &AgentSummary{}
		}
		out.Agents[row.Agent].Sessions++
		out.Agents[row.Agent].Messages += messageCount
	}
	out.Models = bunAnalyticsModelsFiltered(modelRows, f)
	if strings.TrimSpace(f.Model) != "" {
		out.Models = bunAnalyticsPresentRequestedModels(messages, f)
	}
	out.ActiveProjects = len(projects)
	out.ActiveDays = len(days)
	out.AvgMessages = round1(float64(out.TotalMessages) / float64(out.TotalSessions))
	sort.Ints(counts)
	out.MedianMessages = medianInt(counts, len(counts))
	idx := int(float64(len(counts)) * 0.9)
	if idx >= len(counts) {
		idx = len(counts) - 1
	}
	out.P90Messages = counts[idx]
	maxMessages := -1
	for project, count := range projects {
		if count > maxMessages || (count == maxMessages && project < out.MostActive) {
			maxMessages = count
			out.MostActive = project
		}
	}
	if out.TotalMessages > 0 {
		projectCounts := make([]int, 0, len(projects))
		for _, count := range projects {
			projectCounts = append(projectCounts, count)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(projectCounts)))
		top := 0
		for _, count := range projectCounts[:min(3, len(projectCounts))] {
			top += count
		}
		out.Concentration = math.Round(
			float64(top)/float64(out.TotalMessages)*1000,
		) / 1000
	}
	return out
}

func bunAnalyticsPresentRequestedModels(
	rows []bunmodel.Message, f AnalyticsFilter,
) []string {
	requested := make(map[string]struct{})
	for _, model := range csvFilterValues(f.Model) {
		requested[model] = struct{}{}
	}
	present := make(map[string]struct{})
	filter := f.messageScopeFilter()
	for _, row := range rows {
		if _, ok := requested[row.Model]; !ok {
			continue
		}
		local, hasLocal := bunAnalyticsMessageLocalTime(row, f.location())
		if filter.MatchesDayHour(local, hasLocal) {
			present[row.Model] = struct{}{}
		}
	}
	return sortedSetKeys(present)
}

func bunAnalyticsModelsFiltered(rows []bunmodel.Message, f AnalyticsFilter) []string {
	if !f.HasTimeFilter() {
		return bunAnalyticsModels(rows)
	}
	set := map[string]struct{}{}
	filter := f.messageScopeFilter()
	for _, row := range rows {
		local, ok := bunAnalyticsMessageLocalTime(row, f.location())
		if row.Model != "" && filter.MatchesDayHour(local, ok) {
			set[row.Model] = struct{}{}
		}
	}
	return sortedSetKeys(set)
}

func (s *BunStore) GetAnalyticsActivity(
	ctx context.Context, f AnalyticsFilter, granularity string,
) (ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	var result ActivityResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		ids := bunAnalyticsSessionIDs(sessions)
		messages, err := bunAnalyticsMessagesFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		tools, err := bunAnalyticsToolsFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		result = buildBunAnalyticsActivity(sessions, messages, tools, f, granularity)
		return nil
	})
	return result, err
}

func buildBunAnalyticsActivity(
	sessions []bunmodel.Session, messages []bunmodel.Message,
	tools []bunmodel.ToolCall, f AnalyticsFilter, granularity string,
) ActivityResponse {
	out := ActivityResponse{Granularity: granularity}
	bySession := bunAnalyticsMessagesBySession(messages)
	toolBySession := bunAnalyticsToolsBySession(tools)
	var scoped map[string]MessageStats
	if strings.TrimSpace(f.Model) != "" {
		if scope, err := bunAnalyticsMessageScope(messages, f, false); err == nil {
			scoped = scope.StatsBySession()
		}
	}
	buckets := map[string]*ActivityEntry{}
	for _, session := range sessions {
		date := bucketDate(
			bunAnalyticsSessionTime(session).In(f.location()).Format("2006-01-02"),
			granularity,
		)
		entry := buckets[date]
		if entry == nil {
			entry = &ActivityEntry{Date: date, ByAgent: map[string]int{}}
			buckets[date] = entry
		}
		entry.Sessions++
		stat := MessageStats{}
		if scoped != nil {
			stat = scoped[session.ID]
		} else {
			for _, message := range bySession[session.ID] {
				stat.Messages++
				if message.Role == "user" && !message.IsSystem {
					stat.UserMessages++
				}
				if message.Role == "assistant" {
					stat.AssistantMessages++
				}
				if message.HasThinking {
					stat.ThinkingMessages++
				}
			}
		}
		entry.Messages += stat.Messages
		entry.UserMessages += stat.UserMessages
		entry.AssistantMessages += stat.AssistantMessages
		entry.ThinkingMessages += stat.ThinkingMessages
		entry.ByAgent[session.Agent] += stat.Messages
		if scoped == nil {
			entry.ToolCalls += len(toolBySession[session.ID])
		} else {
			entry.ToolCalls += bunAnalyticsScopedToolCount(
				toolBySession[session.ID], bySession[session.ID], f,
			)
		}
	}
	for _, key := range sortedMapKeys(buckets) {
		entry := buckets[key]
		if entry != nil {
			out.Series = append(out.Series, *entry)
		}
	}
	return out
}

func bunAnalyticsScopedToolCount(
	tools []bunmodel.ToolCall, messages []bunmodel.Message, f AnalyticsFilter,
) int {
	models := map[string]struct{}{}
	for _, model := range csvFilterValues(f.Model) {
		models[model] = struct{}{}
	}
	messageByOrdinal := map[int]bunmodel.Message{}
	for _, message := range messages {
		messageByOrdinal[message.Ordinal] = message
	}
	count := 0
	for _, tool := range tools {
		message := messageByOrdinal[tool.MessageOrdinal]
		if _, ok := models[message.Model]; !ok {
			continue
		}
		local, hasLocal := bunAnalyticsMessageLocalTime(message, f.location())
		if f.messageScopeFilter().MatchesDayHour(local, hasLocal) {
			count++
		}
	}
	return count
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *BunStore) GetAnalyticsHeatmap(
	ctx context.Context, f AnalyticsFilter, metric string,
) (HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	var result HeatmapResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(
			ctx, store, bunAnalyticsSessionIDs(sessions),
		)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		stats := map[string]MessageStats{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, false)
			if scopeErr != nil {
				return scopeErr
			}
			stats = scope.StatsBySession()
		}
		anyTokenReporting := false
		for _, row := range sessions {
			date := bunAnalyticsSessionTime(row).In(f.location()).Format("2006-01-02")
			switch metric {
			case "sessions":
				counts[date]++
			case "output_tokens":
				if strings.TrimSpace(f.Model) != "" {
					if stats[row.ID].HasOutputTokens {
						anyTokenReporting = true
						counts[date] += stats[row.ID].OutputTokens
					}
				} else if row.HasTotalOutputTokens {
					anyTokenReporting = true
					counts[date] += row.TotalOutputTokens
				}
			default:
				metric = "messages"
				if strings.TrimSpace(f.Model) != "" {
					counts[date] += stats[row.ID].Messages
				} else {
					counts[date] += row.MessageCount
				}
			}
		}
		entriesFrom := clampFrom(f.From, f.To)
		values := make([]int, 0, len(counts))
		for date, value := range counts {
			if value > 0 && inDateRange(date, entriesFrom, f.To) {
				values = append(values, value)
			}
		}
		sort.Ints(values)
		levels := computeQuartileLevels(values)
		result = HeatmapResponse{
			Metric: metric, EntriesFrom: entriesFrom, Levels: levels,
		}
		if metric != "output_tokens" || anyTokenReporting {
			result.Entries = buildDateEntries(entriesFrom, f.To, counts, levels)
		}
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsProjects(
	ctx context.Context, f AnalyticsFilter,
) (ProjectsAnalyticsResponse, error) {
	f.IncludeSubagents = true
	var result ProjectsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attempt := ProjectsAnalyticsResponse{Projects: []ProjectAnalytics{}}
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(
			ctx, store, bunAnalyticsSessionIDs(sessions),
		)
		if err != nil {
			return err
		}
		stats := map[string]MessageStats{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, false)
			if scopeErr != nil {
				return scopeErr
			}
			stats = scope.StatsBySession()
		}
		type acc struct {
			row    ProjectAnalytics
			counts []int
			days   map[string]int
		}
		projects := map[string]*acc{}
		for _, session := range sessions {
			item := projects[session.Project]
			if item == nil {
				item = &acc{row: ProjectAnalytics{
					Name: session.Project, Agents: map[string]int{},
				}, days: map[string]int{}}
				projects[session.Project] = item
			}
			count := session.MessageCount
			if strings.TrimSpace(f.Model) != "" {
				count = stats[session.ID].Messages
			}
			date := bunAnalyticsSessionTime(session).In(f.location()).Format("2006-01-02")
			item.row.Sessions++
			item.row.Messages += count
			item.row.Agents[session.Agent]++
			item.counts = append(item.counts, count)
			item.days[date] += count
			if item.row.FirstSession == "" || date < item.row.FirstSession {
				item.row.FirstSession = date
			}
			if date > item.row.LastSession {
				item.row.LastSession = date
			}
		}
		for _, item := range projects {
			sort.Ints(item.counts)
			item.row.AvgMessages = round1(float64(item.row.Messages) /
				float64(item.row.Sessions))
			item.row.MedianMessages = medianInt(item.counts, len(item.counts))
			if len(item.days) > 0 {
				item.row.DailyTrend = round1(float64(item.row.Messages) /
					float64(len(item.days)))
			}
			attempt.Projects = append(attempt.Projects, item.row)
		}
		sort.Slice(attempt.Projects, func(i, j int) bool {
			if attempt.Projects[i].Messages != attempt.Projects[j].Messages {
				return attempt.Projects[i].Messages > attempt.Projects[j].Messages
			}
			return attempt.Projects[i].Name < attempt.Projects[j].Name
		})
		result = attempt
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsHourOfWeek(
	ctx context.Context, f AnalyticsFilter,
) (HourOfWeekResponse, error) {
	var result HourOfWeekResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		scopeFilter := f
		scopeFilter.DayOfWeek = nil
		scopeFilter.Hour = nil
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, scopeFilter, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(
			ctx, store, bunAnalyticsSessionIDs(sessions),
		)
		if err != nil {
			return err
		}
		var grid [7][24]int
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, scopeFilter, false)
			if scopeErr != nil {
				return scopeErr
			}
			for _, scoped := range scope.MessagesBySession() {
				for _, message := range scoped {
					if message.HasLocalTime {
						grid[(int(message.LocalTime.Weekday())+6)%7][message.LocalTime.Hour()]++
					}
				}
			}
		} else {
			for _, message := range messages {
				if local, ok := bunAnalyticsMessageLocalTime(message, f.location()); ok {
					grid[(int(local.Weekday())+6)%7][local.Hour()]++
				}
			}
		}
		result = HourOfWeekResponseFromGrid(grid)
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsSessionShape(
	ctx context.Context, f AnalyticsFilter,
) (SessionShapeResponse, error) {
	var result SessionShapeResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
		if err != nil {
			return err
		}
		bySession := bunAnalyticsMessagesBySession(messages)
		stats := map[string]MessageStats{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, false)
			if scopeErr != nil {
				return scopeErr
			}
			stats = scope.StatsBySession()
		}
		lengths, durations, autonomy := map[string]int{}, map[string]int{}, map[string]int{}
		for _, session := range sessions {
			count := session.MessageCount
			userCount, toolCount := 0, 0
			if strings.TrimSpace(f.Model) != "" {
				stat := stats[session.ID]
				count, userCount, toolCount = stat.Messages, stat.UserMessages, stat.ToolUseMessages
			} else {
				for _, message := range bySession[session.ID] {
					if message.Role == "user" && !message.IsSystem {
						userCount++
					}
					if message.Role == "assistant" && message.HasToolUse {
						toolCount++
					}
				}
			}
			lengths[lengthBucket(count)]++
			if session.StartedAt != nil && session.EndedAt != nil &&
				!session.EndedAt.Before(session.StartedAt.Time) {
				durations[durationBucket(session.EndedAt.Time.Sub(
					session.StartedAt.Time).Minutes())]++
			}
			if userCount > 0 {
				autonomy[autonomyBucket(float64(toolCount)/float64(userCount))]++
			}
		}
		result = SessionShapeResponse{
			Count: len(sessions), LengthDistribution: mapToBuckets(lengths, lengthOrder),
			DurationDistribution: mapToBuckets(durations, durationOrder),
			AutonomyDistribution: mapToBuckets(autonomy, autonomyOrder),
		}
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsTools(
	ctx context.Context, f AnalyticsFilter,
) (ToolsAnalyticsResponse, error) {
	var result ToolsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		candidateFilter := f
		candidateFilter.DayOfWeek = nil
		candidateFilter.Hour = nil
		sessions, err := s.bunAnalyticsSessionsFrom(
			ctx, store, candidateFilter, false,
		)
		if err != nil {
			return err
		}
		ids := bunAnalyticsSessionIDs(sessions)
		messages, err := bunAnalyticsMessagesFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		tools, err := bunAnalyticsToolsFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		sessionMap := make(map[string]bunmodel.Session, len(sessions))
		messageMap := map[string]bunmodel.Message{}
		for _, session := range sessions {
			sessionMap[session.ID] = session
		}
		for _, message := range messages {
			messageMap[fmt.Sprintf("%s\x00%d", message.SessionID, message.Ordinal)] = message
		}
		models := map[string]struct{}{}
		for _, model := range csvFilterValues(f.Model) {
			models[model] = struct{}{}
		}
		var rows []ToolAnalyticsRow
		for _, tool := range tools {
			session, ok := sessionMap[tool.SessionID]
			if !ok {
				continue
			}
			message := messageMap[fmt.Sprintf("%s\x00%d", tool.SessionID, tool.MessageOrdinal)]
			if len(models) > 0 {
				if _, selected := models[message.Model]; !selected {
					continue
				}
			}
			used, date, keep := f.ResolveSkillRowTime(
				bunAnalyticsTimeString(message.Timestamp),
				bunAnalyticsSessionTime(session).UTC().Format(time.RFC3339Nano),
			)
			_ = used
			if !keep {
				continue
			}
			rows = append(rows, ToolAnalyticsRow{
				SessionID: tool.SessionID, ToolName: tool.ToolName,
				Category: tool.Category, Agent: session.Agent, Date: date, Count: 1,
			})
		}
		result = BuildToolsAnalytics(rows)
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsSkills(
	ctx context.Context, f AnalyticsFilter, granularity string,
) (SkillsAnalyticsResponse, error) {
	var result SkillsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		candidateFilter := f
		candidateFilter.DayOfWeek = nil
		candidateFilter.Hour = nil
		sessions, err := s.bunAnalyticsSessionsFrom(
			ctx, store, candidateFilter, false,
		)
		if err != nil {
			return err
		}
		ids := bunAnalyticsSessionIDs(sessions)
		messages, err := bunAnalyticsMessagesFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		tools, err := bunAnalyticsToolsFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		sessionMap := make(map[string]bunmodel.Session, len(sessions))
		messageMap := map[string]bunmodel.Message{}
		for _, session := range sessions {
			sessionMap[session.ID] = session
		}
		for _, message := range messages {
			messageMap[fmt.Sprintf("%s\x00%d", message.SessionID, message.Ordinal)] = message
		}
		models := map[string]struct{}{}
		for _, model := range csvFilterValues(f.Model) {
			models[model] = struct{}{}
		}
		var rows []SkillAnalyticsRow
		for _, tool := range tools {
			if tool.SkillName == nil || strings.TrimSpace(*tool.SkillName) == "" {
				continue
			}
			session, ok := sessionMap[tool.SessionID]
			if !ok {
				continue
			}
			message := messageMap[fmt.Sprintf("%s\x00%d", tool.SessionID, tool.MessageOrdinal)]
			if len(models) > 0 {
				if _, selected := models[message.Model]; !selected {
					continue
				}
			}
			used, date, keep := f.ResolveSkillRowTime(
				bunAnalyticsTimeString(message.Timestamp),
				bunAnalyticsSessionTime(session).UTC().Format(time.RFC3339Nano),
			)
			if !keep {
				continue
			}
			rows = append(rows, SkillAnalyticsRow{
				SessionID: tool.SessionID, SkillName: *tool.SkillName,
				Agent: session.Agent, Project: session.Project, Date: date,
				LastUsedAt: used, Count: 1,
			})
		}
		result = BuildSkillsAnalytics(rows, f.From, f.To, granularity)
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsVelocity(
	ctx context.Context, f AnalyticsFilter,
) (VelocityResponse, error) {
	var result VelocityResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		ids := bunAnalyticsSessionIDs(sessions)
		messages, err := bunAnalyticsMessagesFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		tools, err := bunAnalyticsToolsFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		messageMap := map[string][]velocityMsg{}
		messageCounts := map[string]int{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, false)
			if scopeErr != nil {
				return scopeErr
			}
			for id, timing := range scope.TimingBySession() {
				messageCounts[id] = len(timing)
				for _, message := range timing {
					messageMap[id] = append(messageMap[id], velocityMsg{
						role: message.Role, ts: message.Time, valid: message.Valid,
						contentLength: message.ContentLength,
					})
				}
			}
		} else {
			for _, message := range messages {
				local, valid := bunAnalyticsMessageLocalTime(message, f.location())
				messageMap[message.SessionID] = append(messageMap[message.SessionID], velocityMsg{
					role: message.Role, ts: local, valid: valid,
					contentLength: message.ContentLength,
				})
				messageCounts[message.SessionID]++
			}
		}
		toolCounts := map[string]int{}
		if strings.TrimSpace(f.Model) == "" {
			for _, tool := range tools {
				toolCounts[tool.SessionID]++
			}
		} else {
			messagesBySession := bunAnalyticsMessagesBySession(messages)
			for id, sessionTools := range bunAnalyticsToolsBySession(tools) {
				toolCounts[id] = bunAnalyticsScopedToolCount(
					sessionTools, messagesBySession[id], f,
				)
			}
		}
		overall := &velocityAccumulator{}
		byAgent := map[string]*velocityAccumulator{}
		byComplexity := map[string]*velocityAccumulator{}
		for _, session := range sessions {
			rows := messageMap[session.ID]
			if len(rows) < 2 {
				continue
			}
			if byAgent[session.Agent] == nil {
				byAgent[session.Agent] = &velocityAccumulator{}
			}
			complexity := complexityBucket(messageCounts[session.ID])
			if byComplexity[complexity] == nil {
				byComplexity[complexity] = &velocityAccumulator{}
			}
			processSessionVelocity([]*velocityAccumulator{
				overall, byAgent[session.Agent], byComplexity[complexity],
			}, rows, toolCounts[session.ID])
		}
		result = VelocityResponse{
			Overall: overall.computeOverview(), ByAgent: []VelocityBreakdown{},
			ByComplexity: []VelocityBreakdown{},
		}
		for _, key := range sortedMapKeys(byAgent) {
			result.ByAgent = append(result.ByAgent, VelocityBreakdown{
				Label: key, Sessions: byAgent[key].sessions,
				Overview: byAgent[key].computeOverview(),
			})
		}
		order := map[string]int{"1-15": 0, "16-60": 1, "61+": 2}
		keys := sortedMapKeys(byComplexity)
		sort.Slice(keys, func(i, j int) bool { return order[keys[i]] < order[keys[j]] })
		for _, key := range keys {
			result.ByComplexity = append(result.ByComplexity, VelocityBreakdown{
				Label: key, Sessions: byComplexity[key].sessions,
				Overview: byComplexity[key].computeOverview(),
			})
		}
		return nil
	})
	return result, err
}

func bunFrustrationCountsFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string,
) (map[string]int, error) {
	frustration := make(map[string]int)
	err := queryChunkedSize(
		sessionIDs, bunAnalyticsContentSessionBatchSize,
		func(chunk []string) error {
			rows, err := store.NewSelect().Table("messages").
				Column("session_id", "is_system", "content").
				Where("session_id IN (?)", bun.List(chunk)).
				Where("role = ?", "user").Rows(ctx)
			if err != nil {
				return fmt.Errorf("querying Bun frustration messages: %w", err)
			}
			for rows.Next() {
				var sessionID, content string
				var isSystem bool
				if err := rows.Scan(&sessionID, &isSystem, &content); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scanning Bun frustration message: %w", err)
				}
				if !isSystem && signals.IsFrustrationMarker(content) {
					frustration[sessionID]++
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iterating Bun frustration messages: %w", err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("closing Bun frustration messages: %w", err)
			}
			return nil
		},
	)
	return frustration, err
}

func bunSignalRows(
	sessions []bunmodel.Session, frustration map[string]int, f AnalyticsFilter,
) []SignalRow {
	rows := make([]SignalRow, 0, len(sessions))
	for _, session := range sessions {
		rows = append(rows, SignalRow{
			ID: session.ID, Agent: session.Agent, Project: session.Project,
			Date:         bunAnalyticsSessionTime(session).In(f.location()).Format("2006-01-02"),
			FirstMessage: session.FirstMessage, IsAutomated: session.IsAutomated,
			HealthScore: session.HealthScore, HealthGrade: session.HealthGrade,
			Outcome: session.Outcome, OutcomeConfidence: session.OutcomeConfidence,
			ToolFailureSignalCount: session.ToolFailureSignalCount,
			ToolRetryCount:         session.ToolRetryCount, EditChurnCount: session.EditChurnCount,
			CompactionCount:             session.CompactionCount,
			MidTaskCompactionCount:      session.MidTaskCompactionCount,
			ContextPressureMax:          session.ContextPressureMax,
			QualitySignalVersion:        session.QualitySignalVersion,
			ShortPromptCount:            session.ShortPromptCount,
			UnstructuredStart:           session.UnstructuredStart,
			MissingSuccessCriteriaCount: session.MissingSuccessCriteriaCount,
			MissingVerificationCount:    session.MissingVerificationCount,
			DuplicatePromptCount:        session.DuplicatePromptCount,
			NoCodeContextCount:          session.NoCodeContextCount,
			RunawayToolLoopCount:        session.RunawayToolLoopCount,
			FrustrationMarkerCount:      frustration[session.ID],
		})
	}
	return rows
}

func (s *BunStore) GetAnalyticsSignals(
	ctx context.Context, f AnalyticsFilter,
) (SignalsAnalyticsResponse, error) {
	var result SignalsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		frustration, err := bunFrustrationCountsFrom(
			ctx, store, bunAnalyticsSessionIDs(sessions),
		)
		if err != nil {
			return err
		}
		result = AggregateSignals(bunSignalRows(sessions, frustration, f))
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsSignalSessions(
	ctx context.Context, f AnalyticsFilter, signal string, limit int,
) (SignalSessionsResponse, error) {
	if !IsSupportedAnalyticsSignal(signal) {
		return SignalSessionsResponse{}, ErrUnsupportedAnalyticsSignal
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	var result SignalSessionsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		frustration, err := bunFrustrationCountsFrom(
			ctx, store, bunAnalyticsSessionIDs(sessions),
		)
		if err != nil {
			return err
		}
		candidates := SignalCandidates(
			bunSignalRows(sessions, frustration, f), signal, limit,
		)
		candidateIDs := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			candidateIDs = append(candidateIDs, candidate.ID)
		}
		messages, err := bunAnalyticsMessagesWithContentFrom(
			ctx, store, candidateIDs,
		)
		if err != nil {
			return err
		}
		messageMap := map[string][]SignalMessage{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, true)
			if scopeErr != nil {
				return scopeErr
			}
			for id, scoped := range scope.MessagesBySession() {
				for _, message := range scoped {
					messageMap[id] = append(messageMap[id], SignalMessage{
						SessionID: id, Ordinal: message.Ordinal, Role: message.Role,
						Content: message.Content, Timestamp: message.Timestamp,
						IsSystem: message.IsSystem, HasToolUse: message.HasToolUse,
					})
				}
			}
		} else {
			for _, message := range messages {
				messageMap[message.SessionID] = append(messageMap[message.SessionID], SignalMessage{
					SessionID: message.SessionID, Ordinal: message.Ordinal,
					Role: message.Role, Content: message.Content,
					Timestamp: bunAnalyticsTimeString(message.Timestamp),
					IsSystem:  message.IsSystem, HasToolUse: message.HasToolUse,
				})
			}
		}
		result = SignalSessionsResponse{
			Signal: signal, Sessions: BuildSignalExamples(candidates, messageMap, signal),
		}
		return nil
	})
	return result, err
}

func (s *BunStore) GetAnalyticsTopSessions(
	ctx context.Context, f AnalyticsFilter, metric string,
) (TopSessionsResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	var result TopSessionsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
		if err != nil {
			return err
		}
		messagesBySession := bunAnalyticsMessagesBySession(messages)
		stats := map[string]MessageStats{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, false)
			if scopeErr != nil {
				return scopeErr
			}
			stats = scope.StatsBySession()
		}
		var ranked []TopSession
		for _, session := range sessions {
			row := TopSession{
				ID: session.ID, Project: session.Project,
				FirstMessage: session.FirstMessage, DisplayName: session.DisplayName,
				MessageCount: session.MessageCount, OutputTokens: session.TotalOutputTokens,
				StartedAt:         bunAnalyticsStringPtr(session.StartedAt),
				EndedAt:           bunAnalyticsStringPtr(session.EndedAt),
				TerminationStatus: session.TerminationStatus,
			}
			if row.DisplayName == nil {
				row.DisplayName = session.SessionName
			}
			if strings.TrimSpace(f.Model) != "" {
				row.MessageCount = stats[session.ID].Messages
				row.OutputTokens = stats[session.ID].OutputTokens
			}
			if session.StartedAt != nil && session.EndedAt != nil &&
				!session.EndedAt.Before(session.StartedAt.Time) {
				row.DurationMin = session.EndedAt.Time.Sub(session.StartedAt.Time).Minutes()
			}
			previous := time.Time{}
			for _, message := range messagesBySession[session.ID] {
				if message.Timestamp == nil {
					continue
				}
				if !previous.IsZero() {
					gap := message.Timestamp.Time.Sub(previous).Seconds()
					if gap > 0 {
						row.ActiveDurationMin += min(gap, ActiveGapCapSec) / 60
					}
				}
				previous = message.Timestamp.Time
			}
			switch metric {
			case "duration":
				if session.StartedAt == nil || session.EndedAt == nil ||
					session.EndedAt.Before(session.StartedAt.Time) {
					continue
				}
			case "output_tokens":
				if strings.TrimSpace(f.Model) == "" && !session.HasTotalOutputTokens {
					continue
				}
				if strings.TrimSpace(f.Model) != "" && !stats[session.ID].HasOutputTokens {
					continue
				}
			default:
				metric = "messages"
			}
			ranked = append(ranked, row)
		}
		sort.SliceStable(ranked, func(i, j int) bool {
			var left, right float64
			switch metric {
			case "duration":
				left, right = ranked[i].ActiveDurationMin, ranked[j].ActiveDurationMin
			case "output_tokens":
				left, right = float64(ranked[i].OutputTokens), float64(ranked[j].OutputTokens)
			default:
				left, right = float64(ranked[i].MessageCount), float64(ranked[j].MessageCount)
			}
			if left != right {
				return left > right
			}
			return ranked[i].ID < ranked[j].ID
		})
		if len(ranked) > 10 {
			ranked = ranked[:10]
		}
		result = TopSessionsResponse{Metric: metric, Sessions: rankTopSessions(ranked, false)}
		return nil
	})
	return result, err
}
