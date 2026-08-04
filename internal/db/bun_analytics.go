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

func (s *BunStore) bunAnalyticsSessionsFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, applyDate bool,
) ([]bunmodel.Session, error) {
	var rows []bunmodel.Session
	query := store.NewSelect().Model(&rows).
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
	query = appendBunAnalyticsTerminationFilter(query, f.Termination)
	if models := csvFilterValues(f.Model); len(models) > 0 {
		query = query.Where("EXISTS (SELECT 1 FROM messages AS analytics_model "+
			"WHERE analytics_model.session_id = "+bunAnalyticsSessionAlias+
			".id AND analytics_model.model IN (?))", bun.List(models))
	}
	if applyDate && (f.From != "" || f.To != "") {
		from, to := f.utcRange()
		dialect := s.backend.SessionQueryDialect()
		expr := "COALESCE(" + bunAnalyticsSessionAlias + ".started_at, " +
			bunAnalyticsSessionAlias + ".created_at)"
		fromParam, toParam := "?", "?"
		if dialect.timestampOrderExpr != nil {
			expr = "julianday(COALESCE(NULLIF(" + bunAnalyticsSessionAlias +
				".started_at, ''), " + bunAnalyticsSessionAlias + ".created_at))"
			fromParam = dialect.timestampOrderExpr("?")
			toParam = dialect.timestampOrderExpr("?")
		}
		query = query.Where(expr+" >= "+fromParam, from).
			Where(expr+" <= "+toParam, to)
	}
	if err := query.
		ExcludeColumn("started_at", "ended_at", "signals_pending_since",
			"deleted_at", "local_modified_at", "created_at").
		OrderExpr(bunAnalyticsSessionAlias + ".id ASC").Scan(ctx); err != nil {
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
) *bun.SelectQuery {
	var predicates []string
	var args []any
	for value := range strings.SplitSeq(raw, ",") {
		switch strings.TrimSpace(value) {
		case "clean":
			predicates = append(predicates,
				bunAnalyticsSessionAlias+".termination_status = ?")
			args = append(args, "clean")
		case "awaiting_user":
			predicates = append(predicates,
				bunAnalyticsSessionAlias+".termination_status = ?")
			args = append(args, "awaiting_user")
		case "unclean":
			predicates = append(predicates, bunAnalyticsSessionAlias+
				".termination_status IN ('tool_call_pending', 'truncated')")
		}
	}
	if len(predicates) == 0 {
		return query
	}
	return query.Where("("+strings.Join(predicates, " OR ")+")", args...)
}

func bunAnalyticsMessagesFrom(
	ctx context.Context, store bun.IDB, sessionIDs []string,
) ([]bunmodel.Message, error) {
	if len(sessionIDs) == 0 {
		return []bunmodel.Message{}, nil
	}
	var out []bunmodel.Message
	err := queryChunked(sessionIDs, func(chunk []string) error {
		var rows []bunmodel.Message
		if err := store.NewSelect().Model(&rows).
			ExcludeColumn("timestamp").
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
		if err := store.NewSelect().Model(&rows).
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
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
		if err != nil {
			return err
		}
		result = buildBunAnalyticsSummary(sessions, messages, f)
		return nil
	})
	return result, err
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
		out.Models = csvFilterValues(f.Model)
		sort.Strings(out.Models)
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
		out.Series = append(out.Series, *buckets[key])
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
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
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
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, true)
		if err != nil {
			return err
		}
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
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
			result.Projects = append(result.Projects, item.row)
		}
		sort.Slice(result.Projects, func(i, j int) bool {
			if result.Projects[i].Messages != result.Projects[j].Messages {
				return result.Projects[i].Messages > result.Projects[j].Messages
			}
			return result.Projects[i].Name < result.Projects[j].Name
		})
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
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
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
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, false)
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
		sessions, err := s.bunAnalyticsSessionsFrom(ctx, store, f, false)
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

func bunSignalRows(sessions []bunmodel.Session, messages []bunmodel.Message, f AnalyticsFilter) []SignalRow {
	frustration := map[string]int{}
	for _, message := range messages {
		if message.Role == "user" && !message.IsSystem && signals.IsFrustrationMarker(message.Content) {
			frustration[message.SessionID]++
		}
	}
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
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
		if err != nil {
			return err
		}
		result = AggregateSignals(bunSignalRows(sessions, messages, f))
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
		messages, err := bunAnalyticsMessagesFrom(ctx, store, bunAnalyticsSessionIDs(sessions))
		if err != nil {
			return err
		}
		candidates := SignalCandidates(bunSignalRows(sessions, messages, f), signal, limit)
		candidateIDs := map[string]struct{}{}
		for _, candidate := range candidates {
			candidateIDs[candidate.ID] = struct{}{}
		}
		messageMap := map[string][]SignalMessage{}
		if strings.TrimSpace(f.Model) != "" {
			scope, scopeErr := bunAnalyticsMessageScope(messages, f, true)
			if scopeErr != nil {
				return scopeErr
			}
			for id, scoped := range scope.MessagesBySession() {
				if _, ok := candidateIDs[id]; !ok {
					continue
				}
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
				if _, ok := candidateIDs[message.SessionID]; !ok {
					continue
				}
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
