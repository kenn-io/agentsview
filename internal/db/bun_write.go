package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

var (
	canonicalSessionConflictClause = canonicalConflictUpdateClause(
		canonicalReplacementColumns((*bunmodel.Session)(nil), "id"),
	)
	canonicalSessionPreserveDataVersionConflictClause = canonicalConflictUpdateClause(
		canonicalReplacementColumns((*bunmodel.Session)(nil), "id", "data_version"),
	)
	canonicalArchiveMessageColumns = canonicalReplacementColumns(
		(*bunmodel.Message)(nil), "id",
	)
)

func canonicalReplacementColumns(model any, excluded ...string) []string {
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, column := range excluded {
		excludedSet[column] = struct{}{}
	}
	columns := bunmodel.ModelColumns(model)
	return slices.DeleteFunc(columns, func(column string) bool {
		_, skip := excludedSet[column]
		return skip
	})
}

func canonicalConflictUpdateClause(columns []string) string {
	return canonicalConflictUpdateClauseForKey("id", columns)
}

func canonicalConflictUpdateClauseForKey(key string, columns []string) string {
	return canonicalConflictUpdateClauseForKeys([]string{key}, columns)
}

func canonicalConflictUpdateClauseForKeys(keys, columns []string) string {
	var clause strings.Builder
	clause.WriteString("CONFLICT (")
	clause.WriteString(strings.Join(keys, ", "))
	clause.WriteString(") DO UPDATE SET ")
	for index, column := range columns {
		if index > 0 {
			clause.WriteString(", ")
		}
		clause.WriteByte('"')
		clause.WriteString(column)
		clause.WriteString("\" = EXCLUDED.\"")
		clause.WriteString(column)
		clause.WriteByte('"')
	}
	return clause.String()
}

func canonicalSessionUpsertConflictClause(preservedColumns []string) string {
	if len(preservedColumns) == 0 {
		return canonicalSessionConflictClause
	}
	if len(preservedColumns) == 1 && preservedColumns[0] == "data_version" {
		return canonicalSessionPreserveDataVersionConflictClause
	}
	excludedColumns := append([]string{"id"}, preservedColumns...)
	return canonicalConflictUpdateClause(canonicalReplacementColumns(
		(*bunmodel.Session)(nil), excludedColumns...,
	))
}

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

func (db *DB) acquireBunWriteConn(ctx context.Context) (bun.Conn, error) {
	db.connMu.RLock()
	defer db.connMu.RUnlock()
	if db.readOnly {
		return bun.Conn{}, ErrReadOnly
	}
	if db.bunWriter == nil {
		if db.writerClosed.Load() {
			return bun.Conn{}, ErrWriterClosed
		}
		return bun.Conn{}, ErrReadOnly
	}
	return db.bunWriter.Conn(ctx)
}

// copyCanonicalRowsFromAttached copies natural-key child rows from an attached
// SQLite archive using the canonical Bun registry as the destination
// projection. Physical IDs are excluded so the destination assigns them, and
// columns absent from a legacy source archive are omitted.
func copyCanonicalRowsFromAttached(
	ctx context.Context,
	tx bun.IDB,
	model any,
	sourceTable string,
	idsTable string,
	excludedColumns ...string,
) error {
	rows, err := tx.QueryContext(ctx,
		"PRAGMA old_db.table_info("+quoteCommonIdentifier(sourceTable)+")",
	)
	if err != nil {
		return fmt.Errorf("reading attached %s columns: %w", sourceTable, err)
	}
	defer rows.Close()

	sourceColumns := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name string
		var typ, defaultValue sql.NullString
		var notNull, primaryKey int
		if err := rows.Scan(
			&cid, &name, &typ, &notNull, &defaultValue, &primaryKey,
		); err != nil {
			return fmt.Errorf("scanning attached %s columns: %w", sourceTable, err)
		}
		sourceColumns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating attached %s columns: %w", sourceTable, err)
	}

	excluded := make(map[string]struct{}, len(excludedColumns))
	for _, column := range excludedColumns {
		excluded[column] = struct{}{}
	}
	columns := bunmodel.ModelColumns(model)
	columns = slices.DeleteFunc(columns, func(column string) bool {
		if _, skip := excluded[column]; skip {
			return true
		}
		_, present := sourceColumns[column]
		return !present
	})
	if len(columns) == 0 {
		return fmt.Errorf("copying attached %s: no compatible canonical columns", sourceTable)
	}
	if _, present := sourceColumns["session_id"]; !present {
		return fmt.Errorf("copying attached %s: session_id column is required", sourceTable)
	}

	quotedColumns := make([]string, len(columns))
	for index, column := range columns {
		quotedColumns[index] = quoteCommonIdentifier(column)
	}
	columnList := strings.Join(quotedColumns, ", ")
	query := "INSERT INTO main." + quoteCommonIdentifier(sourceTable) +
		" (" + columnList + ") SELECT " + columnList +
		" FROM old_db." + quoteCommonIdentifier(sourceTable) +
		" WHERE " + quoteCommonIdentifier("session_id") + " IN (SELECT " +
		quoteCommonIdentifier("id") + " FROM " + quoteCommonIdentifier(idsTable) + ")"
	if _, err := tx.NewRaw(query).Exec(ctx); err != nil {
		return fmt.Errorf("copying attached %s: %w", sourceTable, err)
	}
	return nil
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
	preservedColumns ...string,
) error {
	if row.ID == "" {
		return fmt.Errorf("upserting canonical session: empty id")
	}
	query := store.NewInsert().Model(&row).
		On(canonicalSessionUpsertConflictClause(preservedColumns)).Returning("")
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
	for _, message := range messages {
		row, err := canonicalMessageRow(message)
		if err != nil {
			return nil, nil, nil, err
		}
		messageRows = append(messageRows, row)
	}
	callRows, resultRows, err := canonicalToolRows(messages)
	if err != nil {
		return nil, nil, nil, err
	}
	return messageRows, callRows, resultRows, nil
}

func canonicalMessageRow(message Message) (bunmodel.Message, error) {
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
	row, err := messageToBunRowWithoutID(message)
	if err != nil {
		return bunmodel.Message{}, err
	}
	truncateCanonicalTimestamp(row.Timestamp)
	return row, nil
}

func canonicalToolRows(
	messages []Message,
) ([]bunmodel.ToolCall, []bunmodel.ToolResultEvent, error) {
	callRows := make([]bunmodel.ToolCall, 0)
	resultRows := make([]bunmodel.ToolResultEvent, 0)
	for _, message := range messages {
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
			eventIndices := CanonicalToolResultEventIndexes(call.ResultEvents)
			for eventPosition, result := range call.ResultEvents {
				eventIndex := eventIndices[eventPosition]
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
					return nil, nil, fmt.Errorf(
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
	slices.SortStableFunc(resultRows, func(a, b bunmodel.ToolResultEvent) int {
		if a.SessionID != b.SessionID {
			return strings.Compare(a.SessionID, b.SessionID)
		}
		if a.ToolCallMessageOrdinal != b.ToolCallMessageOrdinal {
			return a.ToolCallMessageOrdinal - b.ToolCallMessageOrdinal
		}
		if a.CallIndex != b.CallIndex {
			return a.CallIndex - b.CallIndex
		}
		return a.EventIndex - b.EventIndex
	})
	return callRows, resultRows, nil
}

// appendArchiveMessageRows writes SQLite messages through the canonical model.
// Parser output is sanitized before reaching this boundary, so an unavailable
// provider timestamp is stored as NULL and malformed values cannot enter the
// archive.
func appendArchiveMessageRows(
	ctx context.Context, tx bun.IDB, sessionID string, messages []Message,
) error {
	return writeArchiveMessageRows(ctx, tx, sessionID, messages, "")
}

func repairArchiveMessageRows(
	ctx context.Context, tx bun.IDB, sessionID string, messages []Message,
) error {
	updates := canonicalReplacementColumns(
		(*bunmodel.Message)(nil), "id", "session_id", "ordinal",
	)
	conflict := canonicalConflictUpdateClauseForKeys(
		[]string{"session_id", "ordinal"}, updates,
	)
	return writeArchiveMessageRows(ctx, tx, sessionID, messages, conflict)
}

func writeArchiveMessageRows(
	ctx context.Context, tx bun.IDB, sessionID string, messages []Message,
	conflict string,
) error {
	if len(messages) == 0 {
		return nil
	}
	lease := archiveMessageRowPool.acquire(len(messages))
	defer archiveMessageRowPool.release(lease)
	rows := lease.rows
	for index, message := range messages {
		if message.SessionID != sessionID {
			return fmt.Errorf(
				"canonical message session id %q does not match %q",
				message.SessionID, sessionID,
			)
		}
		row, err := canonicalMessageRow(message)
		if err != nil {
			return err
		}
		rows[index] = row
	}
	return writeCanonicalBatches(rows, func(batch []bunmodel.Message) error {
		query := tx.NewInsert().Model(&batch).
			Column(canonicalArchiveMessageColumns...).Returning("")
		if conflict != "" {
			query.On(conflict)
		}
		if _, err := query.Exec(ctx); err != nil {
			return fmt.Errorf(
				"inserting canonical messages for %s: %w", sessionID, err,
			)
		}
		return nil
	})
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
	if len(rows) == 0 {
		return nil
	}
	columns := canonicalReplacementColumns((*bunmodel.Message)(nil))
	if tx.Dialect().Name().String() != "custom" {
		columns = canonicalReplacementColumns((*bunmodel.Message)(nil), "id")
	}
	return writeCanonicalBatches(rows, func(batch []bunmodel.Message) error {
		if _, err := tx.NewInsert().Model(&batch).Column(columns...).Returning("").
			Exec(ctx); err != nil {
			return fmt.Errorf(
				"inserting canonical messages for %s: %w", sessionID, err,
			)
		}
		return nil
	})
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

// RepairMessageRows replaces only the requested logical message ordinals.
// SQLite parse-diff uses this to keep stable message IDs, pins, and untouched
// FTS/tool rows while canonicalizing changed and appended rows through Bun.
func RepairMessageRows(
	ctx context.Context,
	tx bun.IDB,
	sessionID string,
	affectedOrdinals []int,
	messages []bunmodel.Message,
	calls []bunmodel.ToolCall,
	results []bunmodel.ToolResultEvent,
) error {
	if err := prepareMessageRepair(
		ctx, tx, sessionID, affectedOrdinals, messages, calls, results,
	); err != nil {
		return err
	}

	columns := canonicalReplacementColumns((*bunmodel.Message)(nil), "id")
	updates := canonicalReplacementColumns(
		(*bunmodel.Message)(nil), "id", "session_id", "ordinal",
	)
	conflict := canonicalConflictUpdateClauseForKeys(
		[]string{"session_id", "ordinal"}, updates,
	)
	if len(messages) > 0 {
		if err := writeCanonicalBatches(
			messages,
			func(batch []bunmodel.Message) error {
				if _, err := tx.NewInsert().Model(&batch).Column(columns...).
					On(conflict).Returning("").Exec(ctx); err != nil {
					return fmt.Errorf(
						"repairing canonical messages for %s: %w", sessionID, err,
					)
				}
				return nil
			},
		); err != nil {
			return err
		}
	}
	return appendToolRows(ctx, tx, sessionID, calls, results)
}

func prepareMessageRepair(
	ctx context.Context,
	tx bun.IDB,
	sessionID string,
	affectedOrdinals []int,
	messages []bunmodel.Message,
	calls []bunmodel.ToolCall,
	results []bunmodel.ToolResultEvent,
) error {
	if err := validateMessageWriteScope(sessionID, messages); err != nil {
		return err
	}
	if err := validateToolWriteScope(sessionID, calls, results); err != nil {
		return err
	}
	affected := make(map[int]struct{}, len(affectedOrdinals))
	for _, ordinal := range affectedOrdinals {
		if _, exists := affected[ordinal]; exists {
			return fmt.Errorf(
				"duplicate canonical repair ordinal %d for %s",
				ordinal, sessionID,
			)
		}
		affected[ordinal] = struct{}{}
	}
	messageOrdinals := make(map[int]struct{}, len(messages))
	for _, row := range messages {
		if _, ok := affected[row.Ordinal]; !ok {
			return fmt.Errorf(
				"canonical message ordinal %d is outside repair scope for %s",
				row.Ordinal, sessionID,
			)
		}
		if _, exists := messageOrdinals[row.Ordinal]; exists {
			return fmt.Errorf(
				"duplicate canonical message ordinal %d for %s",
				row.Ordinal, sessionID,
			)
		}
		messageOrdinals[row.Ordinal] = struct{}{}
	}
	for _, ordinal := range affectedOrdinals {
		if _, ok := messageOrdinals[ordinal]; !ok {
			return fmt.Errorf(
				"canonical repair ordinal %d has no message for %s",
				ordinal, sessionID,
			)
		}
	}
	for _, row := range calls {
		if _, ok := affected[row.MessageOrdinal]; !ok {
			return fmt.Errorf(
				"canonical tool call ordinal %d is outside repair scope for %s",
				row.MessageOrdinal, sessionID,
			)
		}
	}
	for _, row := range results {
		if _, ok := affected[row.ToolCallMessageOrdinal]; !ok {
			return fmt.Errorf(
				"canonical tool result ordinal %d is outside repair scope for %s",
				row.ToolCallMessageOrdinal, sessionID,
			)
		}
	}

	if len(affectedOrdinals) > 0 {
		if err := writeCanonicalBatches(
			affectedOrdinals,
			func(batch []int) error {
				if _, err := tx.NewDelete().Model((*bunmodel.ToolResultEvent)(nil)).
					Where("session_id = ?", sessionID).
					Where("tool_call_message_ordinal IN (?)", bun.List(batch)).
					Exec(ctx); err != nil {
					return fmt.Errorf(
						"clearing canonical repair tool results for %s: %w",
						sessionID, err,
					)
				}
				if _, err := tx.NewDelete().Model((*bunmodel.ToolCall)(nil)).
					Where("session_id = ?", sessionID).
					Where("message_ordinal IN (?)", bun.List(batch)).
					Exec(ctx); err != nil {
					return fmt.Errorf(
						"clearing canonical repair tool calls for %s: %w",
						sessionID, err,
					)
				}
				return nil
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func appendToolRows(
	ctx context.Context, tx bun.IDB, sessionID string,
	calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent,
) error {
	calls = append([]bunmodel.ToolCall(nil), calls...)
	if err := resolveCanonicalToolMessageIDs(ctx, tx, sessionID, calls); err != nil {
		return err
	}
	if len(calls) > 0 {
		if err := writeCanonicalBatches(calls, func(batch []bunmodel.ToolCall) error {
			insert := tx.NewInsert().Model(&batch).
				Column(canonicalReplacementColumns((*bunmodel.ToolCall)(nil), "id")...).
				Returning("")
			if _, err := insert.Exec(ctx); err != nil {
				return fmt.Errorf(
					"inserting canonical tool calls for %s: %w", sessionID, err,
				)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(results) > 0 {
		if err := writeCanonicalBatches(
			results,
			func(batch []bunmodel.ToolResultEvent) error {
				if _, err := tx.NewInsert().Model(&batch).
					Column(canonicalReplacementColumns(
						(*bunmodel.ToolResultEvent)(nil), "id",
					)...).
					Returning("").Exec(ctx); err != nil {
					return fmt.Errorf(
						"inserting canonical tool results for %s: %w", sessionID, err,
					)
				}
				return nil
			},
		); err != nil {
			return err
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
	if len(calls) > 0 {
		if err := writeCanonicalBatches(calls, func(batch []bunmodel.ToolCall) error {
			if _, err := tx.NewInsert().Model(&batch).
				Column(canonicalReplacementColumns((*bunmodel.ToolCall)(nil), "id")...).
				Returning("").
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
				return fmt.Errorf(
					"upserting canonical tool calls for %s: %w", sessionID, err,
				)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(results) > 0 {
		if err := writeCanonicalBatches(
			results,
			func(batch []bunmodel.ToolResultEvent) error {
				if _, err := tx.NewInsert().Model(&batch).
					Column(canonicalReplacementColumns(
						(*bunmodel.ToolResultEvent)(nil), "id",
					)...).
					Returning("").
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
					return fmt.Errorf(
						"upserting canonical tool results for %s: %w", sessionID, err,
					)
				}
				return nil
			},
		); err != nil {
			return err
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
	needed := make(map[int]struct{}, len(calls))
	for _, call := range calls {
		if call.MessageID == nil {
			needed[call.MessageOrdinal] = struct{}{}
		}
	}
	ordinals := make([]int, 0, len(needed))
	for ordinal := range needed {
		ordinals = append(ordinals, ordinal)
	}
	slices.Sort(ordinals)

	messageIDs := make(map[int]*int64, len(ordinals))
	if len(ordinals) > 0 {
		if err := writeCanonicalBatches(ordinals, func(batch []int) error {
			var messages []bunmodel.Message
			if err := tx.NewSelect().Model(&messages).Column("id", "ordinal").
				Where("session_id = ?", sessionID).
				Where("ordinal IN (?)", bun.List(batch)).
				Scan(ctx); err != nil {
				return fmt.Errorf(
					"resolving canonical tool message ids for %s: %w", sessionID, err,
				)
			}
			for _, message := range messages {
				messageIDs[message.Ordinal] = message.ID
			}
			return nil
		}); err != nil {
			return err
		}
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
	if len(rows) > 0 {
		columns := canonicalReplacementColumns((*bunmodel.UsageEvent)(nil))
		if tx.Dialect().Name().String() != "custom" {
			columns = canonicalReplacementColumns((*bunmodel.UsageEvent)(nil), "id")
		}
		if err := writeCanonicalBatches(rows, func(batch []bunmodel.UsageEvent) error {
			query := tx.NewInsert().Model(&batch).Column(columns...).Returning("")
			if _, err := query.Exec(ctx); err != nil {
				return fmt.Errorf(
					"inserting canonical usage events for %s: %w", sessionID, err,
				)
			}
			return nil
		}); err != nil {
			return err
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
	if len(rows) > 0 {
		if err := writeCanonicalBatches(
			rows,
			func(batch []bunmodel.SecretFinding) error {
				if _, err := tx.NewInsert().Model(&batch).
					Column(canonicalReplacementColumns(
						(*bunmodel.SecretFinding)(nil), "id",
					)...).
					Returning("").Exec(ctx); err != nil {
					return fmt.Errorf(
						"inserting canonical secret findings for %s: %w", sessionID, err,
					)
				}
				return nil
			},
		); err != nil {
			return err
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
