package db

import (
	"context"
	"database/sql"
	"fmt"
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
			"has_output_tokens", "is_sidechain", "timestamp",
		)
		if includeContent {
			query = query.Column("content")
		}
		if err := query.
			Where("session_id IN (?)", bun.List(chunk)).
			OrderExpr("session_id ASC, ordinal ASC").Scan(ctx); err != nil {
			return fmt.Errorf("querying Bun analytics messages: %w", err)
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

func (s *BunStore) GetAnalyticsSummary(
	ctx context.Context, f AnalyticsFilter,
) (AnalyticsSummary, error) {
	f.IncludeSubagents = true
	var result AnalyticsSummary
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsSummaryAggregateAll(ctx, store, f)
		return err
	})
	return result, err
}

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

func (s *BunStore) GetAnalyticsActivity(
	ctx context.Context, f AnalyticsFilter, granularity string,
) (ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	var result ActivityResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.bunAnalyticsActivityFrom(ctx, store, f, granularity)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsHeatmap(
	ctx context.Context, f AnalyticsFilter, metric string,
) (HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	var result HeatmapResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsHeatmapAggregate(ctx, store, f, metric)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsProjects(
	ctx context.Context, f AnalyticsFilter,
) (ProjectsAnalyticsResponse, error) {
	f.IncludeSubagents = true
	var result ProjectsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsProjectsAggregate(ctx, store, f)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsHourOfWeek(
	ctx context.Context, f AnalyticsFilter,
) (HourOfWeekResponse, error) {
	var result HourOfWeekResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsHourOfWeekAggregate(ctx, store, f)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsSessionShape(
	ctx context.Context, f AnalyticsFilter,
) (SessionShapeResponse, error) {
	var result SessionShapeResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.bunAnalyticsSessionShapeFrom(ctx, store, f)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsTools(
	ctx context.Context, f AnalyticsFilter,
) (ToolsAnalyticsResponse, error) {
	var result ToolsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsToolsAggregate(ctx, store, f)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsSkills(
	ctx context.Context, f AnalyticsFilter, granularity string,
) (SkillsAnalyticsResponse, error) {
	var result SkillsAnalyticsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsSkillsAggregate(ctx, store, f, granularity)
		return err
	})
	return result, err
}

func (s *BunStore) GetAnalyticsVelocity(
	ctx context.Context, f AnalyticsFilter,
) (VelocityResponse, error) {
	var result VelocityResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var err error
		result, err = s.getBunAnalyticsVelocityAggregate(ctx, store, f)
		return err
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
		var err error
		result, err = s.getBunAnalyticsSignalsAggregate(ctx, store, f)
		return err
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
		var candidates []SignalRow
		if signal == "frustration_marker_count" {
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
			candidates = SignalCandidates(
				bunSignalRows(sessions, frustration, f), signal, limit,
			)
		} else {
			var err error
			candidates, err = s.bunSignalCandidatesFrom(ctx, store, f, signal, limit)
			if err != nil {
				return err
			}
		}
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
	if metric != "duration" && metric != "output_tokens" {
		metric = "messages"
	}
	var result TopSessionsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		ranked, err := s.bunAnalyticsTopSessionsFrom(ctx, store, f, metric)
		if err != nil {
			return err
		}
		result = TopSessionsResponse{Metric: metric, Sessions: rankTopSessions(ranked, false)}
		return nil
	})
	return result, err
}
