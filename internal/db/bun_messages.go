package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// GetMessages returns ordinal-paginated canonical messages with tool data.
func (s *BunStore) GetMessages(
	ctx context.Context, sessionID string, from, limit int, asc bool,
) ([]Message, error) {
	if limit <= 0 || limit > MaxMessageLimit {
		limit = DefaultMessageLimit
	}
	var pendingMessages []Message
	err := s.consistentView(ctx, func(store bun.IDB) error {
		query := store.NewSelect().Model((*bunmodel.Message)(nil)).
			Where("session_id = ?", sessionID)
		if asc {
			query = query.Where("ordinal >= ?", from).OrderExpr("ordinal ASC")
		} else {
			query = query.Where("ordinal <= ?", from).OrderExpr("ordinal DESC")
		}
		rows, err := scanBunMessages(ctx, query.Limit(limit))
		if err != nil {
			return fmt.Errorf("querying messages: %w", err)
		}
		if err := attachBunToolData(ctx, store, rows); err != nil {
			return err
		}
		pendingMessages = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pendingMessages, nil
}

// GetMessagesWindow implements linear and around-anchor retrieval once for all
// Bun backends. The anchor is retained even when its role is filtered out.
func (s *BunStore) GetMessagesWindow(
	ctx context.Context, sessionID string, window MessageWindow,
) ([]Message, error) {
	if window.Around == nil {
		from := 0
		if window.From != nil {
			from = *window.From
		}
		if len(window.Roles) == 0 {
			return s.GetMessages(ctx, sessionID, from, window.Limit, window.Asc)
		}
		if window.Limit <= 0 || window.Limit > MaxMessageLimit {
			window.Limit = DefaultMessageLimit
		}
		var pendingMessages []Message
		err := s.consistentView(ctx, func(store bun.IDB) error {
			query := store.NewSelect().Model((*bunmodel.Message)(nil)).
				Where("session_id = ?", sessionID).
				Where("role IN (?)", bun.List(window.Roles))
			if window.Asc {
				query = query.Where("ordinal >= ?", from).OrderExpr("ordinal ASC")
			} else {
				query = query.Where("ordinal <= ?", from).OrderExpr("ordinal DESC")
			}
			rows, err := scanBunMessages(ctx, query.Limit(window.Limit))
			if err != nil {
				return fmt.Errorf("querying role-filtered messages: %w", err)
			}
			if err := attachBunToolData(ctx, store, rows); err != nil {
				return err
			}
			pendingMessages = rows
			return nil
		})
		if err != nil {
			return nil, err
		}
		return pendingMessages, nil
	}

	anchor := *window.Around
	var pendingMessages []Message
	err := s.consistentView(ctx, func(store bun.IDB) error {
		queryPart := func(operator, order string, limit int, roles bool) ([]Message, error) {
			if limit <= 0 {
				return []Message{}, nil
			}
			query := store.NewSelect().Model((*bunmodel.Message)(nil)).
				Where("session_id = ?", sessionID).
				Where("ordinal "+operator+" ?", anchor).
				OrderExpr("ordinal " + order).Limit(limit)
			if roles && len(window.Roles) > 0 {
				query = query.Where("role IN (?)", bun.List(window.Roles))
			}
			return scanBunMessages(ctx, query)
		}
		before, err := queryPart("<", "DESC", window.Before, true)
		if err != nil {
			return fmt.Errorf("querying before-window messages: %w", err)
		}
		slices.Reverse(before)
		anchorRows, err := queryPart("=", "ASC", 1, false)
		if err != nil {
			return fmt.Errorf("querying anchor message: %w", err)
		}
		after, err := queryPart(">", "ASC", window.After, true)
		if err != nil {
			return fmt.Errorf("querying after-window messages: %w", err)
		}
		messages := make([]Message, 0, len(before)+len(anchorRows)+len(after))
		messages = append(messages, before...)
		messages = append(messages, anchorRows...)
		messages = append(messages, after...)
		if err := attachBunToolData(ctx, store, messages); err != nil {
			return err
		}
		pendingMessages = messages
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pendingMessages, nil
}

// GetAllMessages returns all canonical messages in ordinal order.
func (s *BunStore) GetAllMessages(
	ctx context.Context, sessionID string,
) ([]Message, error) {
	pendingMessages := []Message{}
	err := s.consistentView(ctx, func(store bun.IDB) error {
		rows, err := scanBunMessages(ctx, store.NewSelect().
			Model((*bunmodel.Message)(nil)).Where("session_id = ?", sessionID).
			OrderExpr("ordinal ASC"))
		if err != nil {
			return fmt.Errorf("querying all messages: %w", err)
		}
		if err := attachBunToolData(ctx, store, rows); err != nil {
			return err
		}
		pendingMessages = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pendingMessages, nil
}

func scanBunMessages(ctx context.Context, query *bun.SelectQuery) ([]Message, error) {
	var rows []bunmodel.Message
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(rows))
	for _, row := range rows {
		message := messageFromBunRow(row)
		if row.ID == nil {
			message.ID = int64(message.Ordinal)
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func attachBunToolData(
	ctx context.Context, store bun.IDB, messages []Message,
) error {
	if len(messages) == 0 {
		return nil
	}
	messageIndex := make(map[int]int, len(messages))
	ordinals := make([]int, 0, len(messages))
	for i, message := range messages {
		messageIndex[message.Ordinal] = i
		ordinals = append(ordinals, message.Ordinal)
	}
	const hydrationBatchSize = 500
	calls := make([]bunmodel.ToolCall, 0)
	for start := 0; start < len(ordinals); start += hydrationBatchSize {
		end := min(start+hydrationBatchSize, len(ordinals))
		var batch []bunmodel.ToolCall
		if err := store.NewSelect().Model(&batch).
			Where("session_id = ?", messages[0].SessionID).
			Where("message_ordinal IN (?)", bun.List(ordinals[start:end])).
			OrderExpr("message_ordinal ASC").OrderExpr("call_index ASC").
			Scan(ctx); err != nil {
			return fmt.Errorf("querying tool calls: %w", err)
		}
		calls = append(calls, batch...)
	}
	for _, row := range calls {
		messagePosition, ok := messageIndex[row.MessageOrdinal]
		if !ok || row.CallIndex < 0 {
			continue
		}
		for len(messages[messagePosition].ToolCalls) <= row.CallIndex {
			messages[messagePosition].ToolCalls = append(
				messages[messagePosition].ToolCalls, ToolCall{},
			)
		}
		messages[messagePosition].ToolCalls[row.CallIndex] = toolCallFromBunRow(row)
	}
	events := make([]bunmodel.ToolResultEvent, 0)
	for start := 0; start < len(ordinals); start += hydrationBatchSize {
		end := min(start+hydrationBatchSize, len(ordinals))
		var batch []bunmodel.ToolResultEvent
		if err := store.NewSelect().Model(&batch).
			Where("session_id = ?", messages[0].SessionID).
			Where("tool_call_message_ordinal IN (?)", bun.List(ordinals[start:end])).
			OrderExpr("tool_call_message_ordinal ASC").
			OrderExpr("call_index ASC").OrderExpr("event_index ASC").
			Scan(ctx); err != nil {
			return fmt.Errorf("querying tool result events: %w", err)
		}
		events = append(events, batch...)
	}
	for _, row := range events {
		messagePosition, ok := messageIndex[row.ToolCallMessageOrdinal]
		if !ok || row.CallIndex < 0 ||
			row.CallIndex >= len(messages[messagePosition].ToolCalls) {
			continue
		}
		messages[messagePosition].ToolCalls[row.CallIndex].ResultEvents = append(
			messages[messagePosition].ToolCalls[row.CallIndex].ResultEvents,
			toolResultEventFromBunRow(row),
		)
	}
	return nil
}

type bunTimingSessionRow struct {
	ID        string              `bun:"id"`
	StartedAt *bunmodel.Timestamp `bun:"started_at"`
	EndedAt   *bunmodel.Timestamp `bun:"ended_at"`
}

type bunTimingMessageRow struct {
	Ordinal    int                 `bun:"ordinal"`
	Timestamp  *bunmodel.Timestamp `bun:"timestamp"`
	HasToolUse bool                `bun:"has_tool_use"`
}

type bunTimingCallRow struct {
	MessageOrdinal    int     `bun:"message_ordinal"`
	CallIndex         int     `bun:"call_index"`
	ToolUseID         string  `bun:"tool_use_id"`
	ToolName          string  `bun:"tool_name"`
	Category          string  `bun:"category"`
	SkillName         *string `bun:"skill_name"`
	SubagentSessionID *string `bun:"subagent_session_id"`
	InputJSON         *string `bun:"input_json"`
}

type bunTimingEventRow struct {
	ToolCallMessageOrdinal int                 `bun:"tool_call_message_ordinal"`
	CallIndex              int                 `bun:"call_index"`
	Source                 string              `bun:"source"`
	Status                 string              `bun:"status"`
	Timestamp              *bunmodel.Timestamp `bun:"timestamp"`
	EventIndex             int                 `bun:"event_index"`
}

func toolCallFromBunRow(row bunmodel.ToolCall) ToolCall {
	call := ToolCall{
		SessionID: row.SessionID, MessageOrdinal: row.MessageOrdinal,
		ToolName: row.ToolName, Category: row.Category, ToolUseID: row.ToolUseID,
		CallIndex: row.CallIndex,
	}
	if row.MessageID != nil {
		call.MessageID = *row.MessageID
	}
	if row.InputJSON != nil {
		call.InputJSON = *row.InputJSON
	}
	if row.SkillName != nil {
		call.SkillName = *row.SkillName
	}
	if row.ResultContentLength != nil {
		call.ResultContentLength = *row.ResultContentLength
	}
	if row.ResultContent != nil {
		call.ResultContent = *row.ResultContent
	}
	if row.SubagentSessionID != nil {
		call.SubagentSessionID = *row.SubagentSessionID
	}
	if row.FilePath != nil {
		call.FilePath = *row.FilePath
	}
	return call
}

func toolResultEventFromBunRow(row bunmodel.ToolResultEvent) ToolResultEvent {
	event := ToolResultEvent{
		Source: row.Source, Status: row.Status, Content: row.Content,
		ContentLength: row.ContentLength, EventIndex: row.EventIndex,
		Timestamp: requiredTimestampFromBunRowPtr(row.Timestamp),
	}
	if row.ToolUseID != nil {
		event.ToolUseID = *row.ToolUseID
	}
	if row.AgentID != nil {
		event.AgentID = *row.AgentID
	}
	if row.SubagentSessionID != nil {
		event.SubagentSessionID = *row.SubagentSessionID
	}
	return event
}

// GetResumeModelCounts counts non-synthetic assistant models.
func (s *BunStore) GetResumeModelCounts(
	ctx context.Context, sessionID string,
) ([]ModelCount, error) {
	var counts []ModelCount
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Table("messages").
			Column("model").ColumnExpr("COUNT(*) AS count").
			Where("session_id = ?", sessionID).Where("role = ?", "assistant").
			Where("model <> ''").Where("model <> ?", "<synthetic>").
			Group("model").OrderExpr("model ASC").Scan(ctx, &counts)
	})
	if err != nil {
		return nil, fmt.Errorf("querying resume model counts: %w", err)
	}
	return counts, nil
}

// GetSessionActivity reduces canonical timestamps in Go so bucket semantics do
// not depend on an engine's date functions.
func (s *BunStore) GetSessionActivity(
	ctx context.Context, sessionID string,
) (*SessionActivityResponse, error) {
	type activityMessage struct {
		Ordinal   int    `bun:"ordinal"`
		Role      string `bun:"role"`
		Content   string `bun:"content"`
		IsSystem  bool   `bun:"is_system"`
		Timestamp any    `bun:"timestamp"`
	}
	var rows []activityMessage
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Table("messages").
			Column("ordinal", "role", "content", "is_system", "timestamp").
			Where("session_id = ?", sessionID).
			OrderExpr("ordinal ASC").Scan(ctx, &rows)
	})
	if err != nil {
		return nil, fmt.Errorf("querying activity messages: %w", err)
	}
	type visibleMessage struct {
		message activityMessage
		at      time.Time
	}
	visible := make([]visibleMessage, 0, len(rows))
	var minTime, maxTime time.Time
	for _, row := range rows {
		if row.IsSystem || IsSystemPrefixed(row.Content, row.Role) {
			continue
		}
		at, ok := bunActivityTime(row.Timestamp)
		if !ok {
			continue
		}
		if len(visible) == 0 || at.Before(minTime) {
			minTime = at
		}
		if len(visible) == 0 || at.After(maxTime) {
			maxTime = at
		}
		visible = append(visible, visibleMessage{message: row, at: at})
	}
	if len(visible) == 0 {
		return &SessionActivityResponse{
			Buckets: []SessionActivityBucket{}, TotalMessages: len(rows),
		}, nil
	}
	anchor := minTime.Unix()
	interval := SnapInterval(maxTime.Unix() - minTime.Unix())
	type bucketValue struct {
		user, assistant int
		first           *int
	}
	populated := make(map[int]bucketValue)
	maxIndex := 0
	for _, item := range visible {
		index := int((item.at.Unix() - anchor) / interval)
		value := populated[index]
		switch item.message.Role {
		case "user":
			value.user++
		case "assistant":
			value.assistant++
		}
		if value.first == nil || item.message.Ordinal < *value.first {
			ordinal := item.message.Ordinal
			value.first = &ordinal
		}
		populated[index] = value
		maxIndex = max(maxIndex, index)
	}
	buckets := make([]SessionActivityBucket, maxIndex+1)
	for index := range buckets {
		start := time.Unix(anchor+int64(index)*interval, 0).UTC()
		value := populated[index]
		buckets[index] = SessionActivityBucket{
			StartTime: start.Format(time.RFC3339),
			EndTime:   start.Add(time.Duration(interval) * time.Second).Format(time.RFC3339),
			UserCount: value.user, AssistantCount: value.assistant,
			FirstOrdinal: value.first,
		}
	}
	return &SessionActivityResponse{
		Buckets: buckets, IntervalSeconds: interval, TotalMessages: len(rows),
	}, nil
}

func bunActivityTime(value any) (time.Time, bool) {
	switch value := value.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return value.UTC(), !value.IsZero()
	case string:
		parsed, err := bunmodel.ParseTimestamp(value)
		return parsed.Time, err == nil && !parsed.IsZero()
	case []byte:
		return bunActivityTime(string(value))
	default:
		return time.Time{}, false
	}
}

// GetSessionTiming assembles timing from canonical rows in Go.
func (s *BunStore) GetSessionTiming(
	ctx context.Context, sessionID string,
) (*SessionTiming, error) {
	now := time.Now().UTC()
	type timingRows struct {
		sessionRow bunTimingSessionRow
		messages   []bunTimingMessageRow
		calls      []bunTimingCallRow
		events     []bunTimingEventRow
		subagents  map[string]bunTimingSessionRow
	}
	var pending timingRows
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attempt := timingRows{subagents: make(map[string]bunTimingSessionRow)}
		if err := store.NewSelect().Table("sessions").
			Column("id", "started_at", "ended_at").Where("id = ?", sessionID).
			Where("deleted_at IS NULL").Scan(ctx, &attempt.sessionRow); err != nil {
			return err
		}
		if err := store.NewSelect().Table("messages").
			Column("ordinal", "timestamp", "has_tool_use").
			Where("session_id = ?", sessionID).
			OrderExpr("ordinal ASC").Scan(ctx, &attempt.messages); err != nil {
			return err
		}
		if err := store.NewSelect().Table("tool_calls").
			Column(
				"message_ordinal", "call_index", "tool_use_id", "tool_name",
				"category", "skill_name", "subagent_session_id", "input_json",
			).
			Where("session_id = ?", sessionID).
			OrderExpr("message_ordinal ASC").OrderExpr("call_index ASC").
			Scan(ctx, &attempt.calls); err != nil {
			return err
		}
		subagentIDs := make([]string, 0)
		for _, call := range attempt.calls {
			if call.SubagentSessionID != nil && *call.SubagentSessionID != "" {
				subagentIDs = append(subagentIDs, *call.SubagentSessionID)
			}
		}
		if len(subagentIDs) > 0 {
			var rows []bunTimingSessionRow
			if err := store.NewSelect().Table("sessions").
				Column("id", "started_at", "ended_at").
				Where("id IN (?)", bun.List(subagentIDs)).Scan(ctx, &rows); err != nil {
				return fmt.Errorf("querying timing subagents: %w", err)
			}
			for _, row := range rows {
				attempt.subagents[row.ID] = row
			}
		}
		if err := store.NewSelect().Table("tool_result_events").
			Column(
				"tool_call_message_ordinal", "call_index", "source", "status",
				"timestamp", "event_index",
			).
			Where("session_id = ?", sessionID).
			OrderExpr("tool_call_message_ordinal ASC").OrderExpr("call_index ASC").
			OrderExpr("event_index ASC").Scan(ctx, &attempt.events); err != nil {
			return err
		}
		pending = attempt
		return nil
	})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying session timing: %w", err)
	}
	sessionRow := pending.sessionRow
	messages := pending.messages
	calls := pending.calls
	events := pending.events
	subagents := pending.subagents
	session := Session{
		ID: sessionRow.ID, StartedAt: timestampFromBunRow(sessionRow.StartedAt),
		EndedAt: timestampFromBunRow(sessionRow.EndedAt),
	}
	turnRows := make([]TurnRow, 0, len(messages))
	for index, message := range messages {
		turn := TurnRow{
			MessageID: int64(message.Ordinal), Ordinal: int64(message.Ordinal),
			Timestamp:  requiredTimestampFromBunRowPtr(message.Timestamp),
			HasToolUse: message.HasToolUse,
		}
		if message.HasToolUse {
			end := ""
			if index+1 < len(messages) {
				end = requiredTimestampFromBunRowPtr(messages[index+1].Timestamp)
			} else if session.EndedAt != nil {
				end = *session.EndedAt
			}
			if duration, ok := bunTimingMillis(turn.Timestamp, end); ok {
				turn.DurationMs = &duration
			}
		}
		turnRows = append(turnRows, turn)
	}
	eventsByCall := make(map[[2]int][]bunTimingEventRow)
	for _, event := range events {
		key := [2]int{event.ToolCallMessageOrdinal, event.CallIndex}
		eventsByCall[key] = append(eventsByCall[key], event)
	}
	callRows := make([]CallRow, 0, len(calls))
	for _, call := range calls {
		row := CallRow{
			MessageID: int64(call.MessageOrdinal), ToolUseID: call.ToolUseID,
			ToolName: call.ToolName, Category: call.Category,
			SkillName: call.SkillName, SubagentSessionID: call.SubagentSessionID,
		}
		if call.InputJSON != nil {
			row.InputJSON = *call.InputJSON
		}
		usedSubagentTiming := false
		if call.SubagentSessionID != nil {
			if subagent, ok := subagents[*call.SubagentSessionID]; ok {
				start := requiredTimestampFromBunRowPtr(subagent.StartedAt)
				end := requiredTimestampFromBunRowPtr(subagent.EndedAt)
				if end == "" {
					end = now.Format(time.RFC3339)
				}
				if duration, valid := bunTimingMillis(start, end); valid {
					row.DurationMs = &duration
					usedSubagentTiming = true
				}
			}
		}
		if !usedSubagentTiming {
			var started, completed string
			for _, event := range eventsByCall[[2]int{call.MessageOrdinal, call.CallIndex}] {
				if event.Source != "tool_execution" || event.Timestamp == nil {
					continue
				}
				switch event.Status {
				case "started":
					if started == "" {
						started = requiredTimestampFromBunRowPtr(event.Timestamp)
					}
				case "completed", "errored":
					completed = requiredTimestampFromBunRowPtr(event.Timestamp)
				}
			}
			if duration, valid := bunTimingMillis(started, completed); valid {
				row.DurationMs = &duration
				row.CompletedAt = completed
			}
		}
		callRows = append(callRows, row)
	}
	sort.Slice(callRows, func(i, j int) bool {
		return callRows[i].MessageID < callRows[j].MessageID
	})
	return AssembleTiming(&session, turnRows, callRows, now), nil
}

func bunTimingMillis(start, end string) (int64, bool) {
	startTime, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return 0, false
	}
	endTime, err := time.Parse(time.RFC3339Nano, end)
	if err != nil || endTime.Before(startTime) {
		return 0, false
	}
	return endTime.Sub(startTime).Milliseconds(), true
}
