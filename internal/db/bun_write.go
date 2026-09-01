package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const canonicalWriteBatchSize = 100

func (db *DB) beginBunWriteTx(
	ctx context.Context,
) (bun.Tx, error) {
	db.connMu.RLock()
	defer db.connMu.RUnlock()
	if db.readOnly {
		return bun.Tx{}, ErrReadOnly
	}
	if db.bunWriter == nil {
		if db.writerClosed.Load() {
			return bun.Tx{}, ErrWriterClosed
		}
		return bun.Tx{}, ErrReadOnly
	}
	return db.bunWriter.BeginTx(ctx, (*sql.TxOptions)(nil))
}

// CanonicalSessionRow converts one public session into its portable Bun row.
func CanonicalSessionRow(session Session) (bunmodel.Session, error) {
	row, err := sessionToBunRow(session)
	if err != nil {
		return bunmodel.Session{}, err
	}
	normalizeCanonicalSessionTimestampPrecision(&row)
	return row, nil
}

// UpsertSessionRow writes the complete canonical session shape with exact
// replacement semantics. Adapters with target-owned curation or ownership
// rules must apply those policies before calling this helper.
func UpsertSessionRow(
	ctx context.Context, store bun.IDB, row bunmodel.Session,
) error {
	if row.ID == "" {
		return fmt.Errorf("upserting canonical session: empty id")
	}
	query := store.NewInsert().Model(&row).
		On("CONFLICT (id) DO UPDATE").Returning("")
	for _, column := range bunmodel.ModelColumns((*bunmodel.Session)(nil)) {
		if column == "id" {
			continue
		}
		query = query.Set("? = EXCLUDED.?", bun.Ident(column), bun.Ident(column))
	}
	if _, err := query.Exec(ctx); err != nil {
		return fmt.Errorf("upserting canonical session %s: %w", row.ID, err)
	}
	return nil
}

// CanonicalSessionRowMatches reports whether the complete portable session row
// already has the requested value. Timestamp precision is normalized to the
// microseconds shared by PostgreSQL and DuckDB before comparison.
func CanonicalSessionRowMatches(
	ctx context.Context, store bun.IDB, row bunmodel.Session,
) (bool, error) {
	var stored bunmodel.Session
	if err := store.NewSelect().Model(&stored).
		Where("id = ?", row.ID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("reading canonical session %s: %w", row.ID, err)
	}
	normalizeCanonicalSessionTimestampPrecision(&stored)
	normalizeCanonicalSessionTimestampPrecision(&row)
	return reflect.DeepEqual(stored, row), nil
}

func normalizeCanonicalSessionTimestampPrecision(row *bunmodel.Session) {
	for _, value := range []*bunmodel.Timestamp{
		row.StartedAt,
		row.EndedAt,
		row.SignalsPendingSince,
		row.DeletedAt,
		row.LocalModifiedAt,
	} {
		truncateCanonicalTimestamp(value)
	}
	row.CreatedAt.Time = row.CreatedAt.Truncate(time.Microsecond)
}

// CanonicalMessageRows converts messages and their nested tool payloads once
// for any replication target. Source row IDs are deliberately omitted; the
// canonical relationships use session, ordinal, call index, and event index.
func CanonicalMessageRows(
	messages []Message,
) ([]bunmodel.Message, []bunmodel.ToolCall, []bunmodel.ToolResultEvent, error) {
	messageRows := make([]bunmodel.Message, 0, len(messages))
	callRows := make([]bunmodel.ToolCall, 0)
	resultRows := make([]bunmodel.ToolResultEvent, 0)
	for _, message := range messages {
		message.Role = SanitizeUTF8(message.Role)
		message.Content = SanitizeUTF8(message.Content)
		message.ThinkingText = SanitizeUTF8(message.ThinkingText)
		message.Model = SanitizeUTF8(message.Model)
		message.TokenUsage = []byte(SanitizeUTF8(string(message.TokenUsage)))
		message.ClaudeMessageID = SanitizeUTF8(message.ClaudeMessageID)
		message.ClaudeRequestID = SanitizeUTF8(message.ClaudeRequestID)
		message.SourceType = SanitizeUTF8(message.SourceType)
		message.SourceSubtype = SanitizeUTF8(message.SourceSubtype)
		message.PromptSource = SanitizeUTF8(message.PromptSource)
		message.SourceUUID = SanitizeUTF8(message.SourceUUID)
		message.SourceParentUUID = SanitizeUTF8(message.SourceParentUUID)
		row, err := messageToBunRow(message)
		if err != nil {
			return nil, nil, nil, err
		}
		row.ID = nil
		truncateCanonicalTimestamp(row.Timestamp)
		messageRows = append(messageRows, row)
		for callIndex, call := range message.ToolCalls {
			callRows = append(callRows, bunmodel.ToolCall{
				SessionID: message.SessionID, MessageOrdinal: message.Ordinal,
				CallIndex: callIndex, ToolName: SanitizeUTF8(call.ToolName),
				Category:            SanitizeUTF8(call.Category),
				ToolUseID:           SanitizeUTF8(call.ToolUseID),
				InputJSON:           optionalCanonicalString(call.InputJSON),
				SkillName:           optionalCanonicalString(call.SkillName),
				ResultContentLength: optionalCanonicalInt(call.ResultContentLength),
				ResultContent:       optionalCanonicalString(call.ResultContent),
				SubagentSessionID:   optionalCanonicalString(call.SubagentSessionID),
				FilePath:            optionalCanonicalString(call.FilePath),
			})
			eventIndices := make(map[int]struct{}, len(call.ResultEvents))
			uniqueEventIndices := true
			for _, result := range call.ResultEvents {
				if _, exists := eventIndices[result.EventIndex]; exists {
					uniqueEventIndices = false
					break
				}
				eventIndices[result.EventIndex] = struct{}{}
			}
			for eventPosition, result := range call.ResultEvents {
				eventIndex := result.EventIndex
				if !uniqueEventIndices {
					eventIndex = eventPosition
				}
				if result.ContentLength == 0 {
					result.ContentLength = len(result.Content)
				}
				if result.ToolUseID == "" {
					result.ToolUseID = call.ToolUseID
				}
				if result.SubagentSessionID == "" {
					result.SubagentSessionID = call.SubagentSessionID
				}
				timestamp, err := timestampToBunRow(result.Timestamp)
				if err != nil {
					return nil, nil, nil, fmt.Errorf(
						"tool result %q ordinal %d event %d timestamp: %w",
						message.SessionID, message.Ordinal, eventIndex, err,
					)
				}
				truncateCanonicalTimestamp(timestamp)
				resultRows = append(resultRows, bunmodel.ToolResultEvent{
					SessionID:              message.SessionID,
					ToolCallMessageOrdinal: message.Ordinal,
					CallIndex:              callIndex, EventIndex: eventIndex,
					ToolUseID:         optionalCanonicalString(result.ToolUseID),
					AgentID:           optionalCanonicalString(result.AgentID),
					SubagentSessionID: optionalCanonicalString(result.SubagentSessionID),
					Source:            SanitizeUTF8(result.Source),
					Status:            SanitizeUTF8(result.Status),
					Content:           SanitizeUTF8(result.Content),
					ContentLength:     result.ContentLength, Timestamp: timestamp,
				})
			}
		}
	}
	return messageRows, callRows, resultRows, nil
}

// CanonicalUsageEventRows converts source accounting into target-assigned
// rows while preserving nullable ordinals, costs, and timestamps.
func CanonicalUsageEventRows(events []UsageEvent) ([]bunmodel.UsageEvent, error) {
	rows := make([]bunmodel.UsageEvent, 0, len(events))
	for _, event := range events {
		occurredAt, err := timestampToBunRow(event.OccurredAt)
		if err != nil {
			return nil, fmt.Errorf("usage event %q occurred_at: %w", event.DedupKey, err)
		}
		truncateCanonicalTimestamp(occurredAt)
		var cost *int64
		if event.Cost != nil {
			value := event.Cost.Microdollars
			cost = &value
		}
		rows = append(rows, bunmodel.UsageEvent{
			ID:        event.ID,
			SessionID: event.SessionID, MessageOrdinal: event.MessageOrdinal,
			Source: SanitizeUTF8(event.Source), Model: SanitizeUTF8(event.Model),
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens,
			CacheReadInputTokens:     event.CacheReadInputTokens,
			ReasoningTokens:          event.ReasoningTokens, CostMicrodollars: cost,
			CostStatus: SanitizeUTF8(event.CostStatus),
			CostSource: SanitizeUTF8(event.CostSource), OccurredAt: occurredAt,
			DedupKey: SanitizeUTF8(event.DedupKey),
		})
	}
	return rows, nil
}

// CanonicalSecretFindingRows converts source findings for replication.
func CanonicalSecretFindingRows(findings []SecretFinding) []bunmodel.SecretFinding {
	createdAt := bunmodel.NewTimestamp(time.Now().UTC())
	rows := make([]bunmodel.SecretFinding, len(findings))
	for i, finding := range findings {
		rows[i] = bunmodel.SecretFinding{
			SessionID: finding.SessionID, RuleName: SanitizeUTF8(finding.RuleName),
			Confidence:     SanitizeUTF8(finding.Confidence),
			LocationKind:   SanitizeUTF8(finding.LocationKind),
			MessageOrdinal: finding.MessageOrdinal, CallIndex: finding.CallIndex,
			EventIndex: finding.EventIndex, MatchStart: finding.MatchStart,
			MatchEnd: finding.MatchEnd, MatchIndex: finding.MatchIndex,
			RedactedMatch: SanitizeUTF8(finding.RedactedMatch),
			RulesVersion:  SanitizeUTF8(finding.RulesVersion), CreatedAt: createdAt,
		}
	}
	return rows
}

func optionalCanonicalString(value string) *string {
	value = SanitizeUTF8(value)
	if value == "" {
		return nil
	}
	return &value
}

func optionalCanonicalInt(value int) *int {
	if value == 0 {
		return nil
	}
	return &value
}

func truncateCanonicalTimestamp(value *bunmodel.Timestamp) {
	if value == nil {
		return
	}
	value.Time = value.Truncate(time.Microsecond)
}

// ReplaceMessageRows replaces a session's canonical message rows. Tool rows
// are cleared first because every adapter relates them to the message's
// portable (session_id, ordinal) key.
func ReplaceMessageRows(
	ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.Message,
) error {
	if err := validateMessageWriteScope(sessionID, rows); err != nil {
		return err
	}
	for _, model := range []any{
		(*bunmodel.ToolResultEvent)(nil),
		(*bunmodel.ToolCall)(nil),
		(*bunmodel.Message)(nil),
	} {
		if _, err := tx.NewDelete().Model(model).
			Where("session_id = ?", sessionID).Exec(ctx); err != nil {
			return fmt.Errorf("clearing canonical message rows for %s: %w", sessionID, err)
		}
	}
	return appendMessageRows(ctx, tx, sessionID, rows)
}

// CanonicalSessionDependentRowsMatch reports whether a target already stores
// the canonical message, tool, result, and usage rows for one source snapshot.
// Storage-assigned identifiers are ignored.
func CanonicalSessionDependentRowsMatch(
	ctx context.Context, store bun.IDB, sessionID string,
	messages []bunmodel.Message, calls []bunmodel.ToolCall,
	results []bunmodel.ToolResultEvent, usage []bunmodel.UsageEvent,
) (bool, error) {
	var storedMessages []bunmodel.Message
	if err := store.NewSelect().Model(&storedMessages).
		Where("session_id = ?", sessionID).OrderExpr("ordinal ASC").Scan(ctx); err != nil {
		return false, fmt.Errorf("reading canonical messages for comparison: %w", err)
	}
	var storedCalls []bunmodel.ToolCall
	if err := store.NewSelect().Model(&storedCalls).
		Where("session_id = ?", sessionID).
		OrderExpr("message_ordinal ASC").OrderExpr("call_index ASC").Scan(ctx); err != nil {
		return false, fmt.Errorf("reading canonical tool calls for comparison: %w", err)
	}
	var storedResults []bunmodel.ToolResultEvent
	if err := store.NewSelect().Model(&storedResults).
		Where("session_id = ?", sessionID).
		OrderExpr("tool_call_message_ordinal ASC").OrderExpr("call_index ASC").
		OrderExpr("event_index ASC").Scan(ctx); err != nil {
		return false, fmt.Errorf("reading canonical tool results for comparison: %w", err)
	}
	var storedUsage []bunmodel.UsageEvent
	if err := store.NewSelect().Model(&storedUsage).
		Where("session_id = ?", sessionID).
		OrderExpr("occurred_at ASC NULLS FIRST").OrderExpr("id ASC").Scan(ctx); err != nil {
		return false, fmt.Errorf("reading canonical usage events for comparison: %w", err)
	}

	messages = append([]bunmodel.Message(nil), messages...)
	calls = append([]bunmodel.ToolCall(nil), calls...)
	results = append([]bunmodel.ToolResultEvent(nil), results...)
	usage = append([]bunmodel.UsageEvent(nil), usage...)
	for i := range storedMessages {
		storedMessages[i].ID = nil
		if len(storedMessages[i].TokenUsage) == 0 {
			storedMessages[i].TokenUsage = nil
		}
	}
	for i := range messages {
		messages[i].ID = nil
		if len(messages[i].TokenUsage) == 0 {
			messages[i].TokenUsage = nil
		}
	}
	for i := range storedCalls {
		storedCalls[i].ID = nil
		storedCalls[i].MessageID = nil
	}
	for i := range calls {
		calls[i].ID = nil
		calls[i].MessageID = nil
	}
	for i := range storedResults {
		storedResults[i].ID = nil
	}
	for i := range results {
		results[i].ID = nil
	}
	for i := range storedUsage {
		storedUsage[i].ID = 0
	}
	for i := range usage {
		usage[i].ID = 0
	}
	return reflect.DeepEqual(storedMessages, messages) &&
		reflect.DeepEqual(storedCalls, calls) &&
		reflect.DeepEqual(storedResults, results) &&
		reflect.DeepEqual(storedUsage, usage), nil
}

// AppendMessageRows appends canonical messages without disturbing rows already
// stored for the session. SQLite's append-only parser path uses this after
// selecting only ordinals newer than the durable transcript.
func AppendMessageRows(
	ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.Message,
) error {
	if err := validateMessageWriteScope(sessionID, rows); err != nil {
		return err
	}
	return appendMessageRows(ctx, tx, sessionID, rows)
}

func appendMessageRows(
	ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.Message,
) error {
	for start := 0; start < len(rows); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(rows))
		batch := rows[start:end]
		query := tx.NewInsert().Model(&batch).Returning("")
		if tx.Dialect().Name().String() != "custom" {
			query = query.ExcludeColumn("id")
		}
		if _, err := query.Exec(ctx); err != nil {
			return fmt.Errorf("inserting canonical messages for %s: %w", sessionID, err)
		}
	}
	return nil
}

// ReplaceToolRows replaces a session's canonical tool-call and result-event
// rows after its messages have been written.
func ReplaceToolRows(
	ctx context.Context, tx bun.IDB, sessionID string,
	calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent,
) error {
	if err := validateToolWriteScope(sessionID, calls, results); err != nil {
		return err
	}
	return replaceToolRows(ctx, tx, sessionID, calls, results)
}

// AppendToolRows appends canonical tool rows without disturbing earlier
// ordinals in an append-only transcript.
func AppendToolRows(
	ctx context.Context, tx bun.IDB, sessionID string,
	calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent,
) error {
	if err := validateToolWriteScope(sessionID, calls, results); err != nil {
		return err
	}
	return appendToolRows(ctx, tx, sessionID, calls, results)
}

func appendToolRows(
	ctx context.Context, tx bun.IDB, sessionID string,
	calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent,
) error {
	calls = append([]bunmodel.ToolCall(nil), calls...)
	if err := resolveCanonicalToolMessageIDs(ctx, tx, sessionID, calls); err != nil {
		return err
	}
	for start := 0; start < len(calls); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(calls))
		batch := calls[start:end]
		insert := tx.NewInsert().Model(&batch).ExcludeColumn("id").Returning("")
		if _, err := insert.Exec(ctx); err != nil {
			return fmt.Errorf("inserting canonical tool calls for %s: %w", sessionID, err)
		}
	}
	for start := 0; start < len(results); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(results))
		batch := results[start:end]
		if _, err := tx.NewInsert().Model(&batch).ExcludeColumn("id").
			Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("inserting canonical tool results for %s: %w", sessionID, err)
		}
	}
	return nil
}

type canonicalToolCallKey struct {
	MessageOrdinal int `bun:"message_ordinal"`
	CallIndex      int `bun:"call_index"`
}

type canonicalToolResultKey struct {
	MessageOrdinal int `bun:"tool_call_message_ordinal"`
	CallIndex      int `bun:"call_index"`
	EventIndex     int `bun:"event_index"`
}

func replaceToolRows(
	ctx context.Context, tx bun.IDB, sessionID string,
	calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent,
) error {
	var existingCalls []canonicalToolCallKey
	if err := tx.NewSelect().Model((*bunmodel.ToolCall)(nil)).
		Column("message_ordinal", "call_index").
		Where("session_id = ?", sessionID).Scan(ctx, &existingCalls); err != nil {
		return fmt.Errorf("reading canonical tool calls for %s: %w", sessionID, err)
	}
	var existingResults []canonicalToolResultKey
	if err := tx.NewSelect().Model((*bunmodel.ToolResultEvent)(nil)).
		Column("tool_call_message_ordinal", "call_index", "event_index").
		Where("session_id = ?", sessionID).Scan(ctx, &existingResults); err != nil {
		return fmt.Errorf("reading canonical tool results for %s: %w", sessionID, err)
	}

	calls = append([]bunmodel.ToolCall(nil), calls...)
	if err := resolveCanonicalToolMessageIDs(ctx, tx, sessionID, calls); err != nil {
		return err
	}
	for start := 0; start < len(calls); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(calls))
		batch := calls[start:end]
		if _, err := tx.NewInsert().Model(&batch).ExcludeColumn("id").Returning("").
			On("CONFLICT (session_id, message_ordinal, call_index) DO UPDATE").
			Set("message_id = EXCLUDED.message_id").
			Set("tool_name = EXCLUDED.tool_name").
			Set("category = EXCLUDED.category").
			Set("tool_use_id = EXCLUDED.tool_use_id").
			Set("input_json = EXCLUDED.input_json").
			Set("skill_name = EXCLUDED.skill_name").
			Set("result_content_length = EXCLUDED.result_content_length").
			Set("result_content = EXCLUDED.result_content").
			Set("subagent_session_id = EXCLUDED.subagent_session_id").
			Set("file_path = EXCLUDED.file_path").
			Exec(ctx); err != nil {
			return fmt.Errorf("upserting canonical tool calls for %s: %w", sessionID, err)
		}
	}
	for start := 0; start < len(results); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(results))
		batch := results[start:end]
		if _, err := tx.NewInsert().Model(&batch).ExcludeColumn("id").Returning("").
			On("CONFLICT (session_id, tool_call_message_ordinal, call_index, event_index) DO UPDATE").
			Set("tool_use_id = EXCLUDED.tool_use_id").
			Set("agent_id = EXCLUDED.agent_id").
			Set("subagent_session_id = EXCLUDED.subagent_session_id").
			Set("source = EXCLUDED.source").
			Set("status = EXCLUDED.status").
			Set("content = EXCLUDED.content").
			Set("content_length = EXCLUDED.content_length").
			Set("timestamp = EXCLUDED.timestamp").
			Exec(ctx); err != nil {
			return fmt.Errorf("upserting canonical tool results for %s: %w", sessionID, err)
		}
	}

	desiredResults := make(map[canonicalToolResultKey]struct{}, len(results))
	for _, row := range results {
		desiredResults[canonicalToolResultKey{
			MessageOrdinal: row.ToolCallMessageOrdinal,
			CallIndex:      row.CallIndex, EventIndex: row.EventIndex,
		}] = struct{}{}
	}
	for _, key := range existingResults {
		if _, ok := desiredResults[key]; ok {
			continue
		}
		if _, err := tx.NewDelete().Model((*bunmodel.ToolResultEvent)(nil)).
			Where("session_id = ?", sessionID).
			Where("tool_call_message_ordinal = ?", key.MessageOrdinal).
			Where("call_index = ?", key.CallIndex).
			Where("event_index = ?", key.EventIndex).Exec(ctx); err != nil {
			return fmt.Errorf("deleting stale canonical tool result for %s: %w", sessionID, err)
		}
	}
	desiredCalls := make(map[canonicalToolCallKey]struct{}, len(calls))
	for _, row := range calls {
		desiredCalls[canonicalToolCallKey{
			MessageOrdinal: row.MessageOrdinal, CallIndex: row.CallIndex,
		}] = struct{}{}
	}
	for _, key := range existingCalls {
		if _, ok := desiredCalls[key]; ok {
			continue
		}
		if _, err := tx.NewDelete().Model((*bunmodel.ToolCall)(nil)).
			Where("session_id = ?", sessionID).
			Where("message_ordinal = ?", key.MessageOrdinal).
			Where("call_index = ?", key.CallIndex).Exec(ctx); err != nil {
			return fmt.Errorf("deleting stale canonical tool call for %s: %w", sessionID, err)
		}
	}
	return nil
}

func resolveCanonicalToolMessageIDs(
	ctx context.Context, tx bun.IDB, sessionID string, calls []bunmodel.ToolCall,
) error {
	var messages []bunmodel.Message
	if len(calls) > 0 {
		if err := tx.NewSelect().Model(&messages).Column("id", "ordinal").
			Where("session_id = ?", sessionID).Scan(ctx); err != nil {
			return fmt.Errorf("resolving canonical tool message ids for %s: %w", sessionID, err)
		}
	}
	messageIDs := make(map[int]*int64, len(messages))
	for _, message := range messages {
		messageIDs[message.Ordinal] = message.ID
	}
	for i := range calls {
		if calls[i].MessageID == nil {
			messageID, ok := messageIDs[calls[i].MessageOrdinal]
			if !ok {
				return fmt.Errorf(
					"canonical tool call for %s has missing message ordinal %d",
					sessionID, calls[i].MessageOrdinal,
				)
			}
			calls[i].MessageID = messageID
		}
	}
	return nil
}

// ReplaceUsageEventRows replaces the full accounting set for one session.
func ReplaceUsageEventRows(
	ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.UsageEvent,
) error {
	for _, row := range rows {
		if row.SessionID != sessionID {
			return fmt.Errorf("usage event session %q does not match %q", row.SessionID, sessionID)
		}
	}
	if _, err := tx.NewDelete().Model((*bunmodel.UsageEvent)(nil)).
		Where("session_id = ?", sessionID).Exec(ctx); err != nil {
		return fmt.Errorf("clearing canonical usage events for %s: %w", sessionID, err)
	}
	for start := 0; start < len(rows); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(rows))
		batch := rows[start:end]
		query := tx.NewInsert().Model(&batch).Returning("")
		if tx.Dialect().Name().String() != "custom" {
			query = query.ExcludeColumn("id")
		}
		if _, err := query.Exec(ctx); err != nil {
			return fmt.Errorf("inserting canonical usage events for %s: %w", sessionID, err)
		}
	}
	return nil
}

// ReplaceSecretFindingRows replaces the full persisted finding set for one
// session without reconstructing source payloads.
func ReplaceSecretFindingRows(
	ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.SecretFinding,
) error {
	for _, row := range rows {
		if row.SessionID != sessionID {
			return fmt.Errorf("secret finding session %q does not match %q", row.SessionID, sessionID)
		}
	}
	if _, err := tx.NewDelete().Model((*bunmodel.SecretFinding)(nil)).
		Where("session_id = ?", sessionID).Exec(ctx); err != nil {
		return fmt.Errorf("clearing canonical secret findings for %s: %w", sessionID, err)
	}
	for start := 0; start < len(rows); start += canonicalWriteBatchSize {
		end := min(start+canonicalWriteBatchSize, len(rows))
		batch := rows[start:end]
		if _, err := tx.NewInsert().Model(&batch).ExcludeColumn("id").
			Returning("").Exec(ctx); err != nil {
			return fmt.Errorf("inserting canonical secret findings for %s: %w", sessionID, err)
		}
	}
	return nil
}

// CanonicalSecretFindingRowsMatch compares persisted finding content while
// ignoring target-assigned IDs and replication-time created_at values.
func CanonicalSecretFindingRowsMatch(
	ctx context.Context, store bun.IDB, sessionID string,
	rows []bunmodel.SecretFinding,
) (bool, error) {
	var stored []bunmodel.SecretFinding
	if err := store.NewSelect().Model(&stored).
		Where("session_id = ?", sessionID).
		OrderExpr("message_ordinal ASC").OrderExpr("match_start ASC").
		OrderExpr("match_index ASC").Scan(ctx); err != nil {
		return false, fmt.Errorf("reading canonical secret findings for comparison: %w", err)
	}
	rows = append([]bunmodel.SecretFinding(nil), rows...)
	for i := range stored {
		stored[i].ID = nil
		stored[i].CreatedAt = bunmodel.Timestamp{}
	}
	for i := range rows {
		rows[i].ID = nil
		rows[i].CreatedAt = bunmodel.Timestamp{}
	}
	return reflect.DeepEqual(stored, rows), nil
}

func validateMessageWriteScope(sessionID string, rows []bunmodel.Message) error {
	for _, row := range rows {
		if row.SessionID != sessionID {
			return fmt.Errorf("message session %q does not match %q", row.SessionID, sessionID)
		}
	}
	return nil
}

func validateToolWriteScope(
	sessionID string, calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent,
) error {
	callKeys := make(map[canonicalToolCallKey]struct{}, len(calls))
	for _, row := range calls {
		if row.SessionID != sessionID {
			return fmt.Errorf("tool call session %q does not match %q", row.SessionID, sessionID)
		}
		key := canonicalToolCallKey{
			MessageOrdinal: row.MessageOrdinal, CallIndex: row.CallIndex,
		}
		if _, exists := callKeys[key]; exists {
			return fmt.Errorf(
				"duplicate canonical tool call (%d, %d) for %s",
				key.MessageOrdinal, key.CallIndex, sessionID,
			)
		}
		callKeys[key] = struct{}{}
	}
	resultKeys := make(map[canonicalToolResultKey]struct{}, len(results))
	for _, row := range results {
		if row.SessionID != sessionID {
			return fmt.Errorf("tool result session %q does not match %q", row.SessionID, sessionID)
		}
		parent := canonicalToolCallKey{
			MessageOrdinal: row.ToolCallMessageOrdinal, CallIndex: row.CallIndex,
		}
		if _, exists := callKeys[parent]; !exists {
			return fmt.Errorf(
				"canonical tool result for %s has missing tool call (%d, %d)",
				sessionID, parent.MessageOrdinal, parent.CallIndex,
			)
		}
		key := canonicalToolResultKey{
			MessageOrdinal: row.ToolCallMessageOrdinal,
			CallIndex:      row.CallIndex, EventIndex: row.EventIndex,
		}
		if _, exists := resultKeys[key]; exists {
			return fmt.Errorf(
				"duplicate canonical tool result (%d, %d, %d) for %s",
				key.MessageOrdinal, key.CallIndex, key.EventIndex, sessionID,
			)
		}
		resultKeys[key] = struct{}{}
	}
	return nil
}
