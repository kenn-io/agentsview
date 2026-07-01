// ABOUTME: Diff-based session message replacement: updates only the
// ABOUTME: rows that changed instead of delete+reinserting the session.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// diffDeleteChunkSize bounds IN(...) parameter lists when clearing
// tool rows for updated messages, mirroring the insert chunk limits.
const diffDeleteChunkSize = 500

// messageInsertArgs returns the values for insertMessageCols in
// declaration order. insertMessagesTx and the diff UPDATE share it
// so the persisted tuple has exactly one definition.
func messageInsertArgs(m Message) []any {
	return []any{
		m.SessionID, m.Ordinal, m.Role, m.Content,
		m.ThinkingText,
		m.Timestamp, m.HasThinking, m.HasToolUse,
		m.ContentLength, m.IsSystem,
		m.Model, string(m.TokenUsage),
		m.ContextTokens, m.OutputTokens,
		m.HasContextTokens, m.HasOutputTokens,
		m.ClaudeMessageID, m.ClaudeRequestID,
		m.SourceType, m.SourceSubtype, m.SourceUUID,
		m.SourceParentUUID, m.IsSidechain, m.IsCompactBoundary,
	}
}

// messageUpdateSetClause is the UPDATE assignment list derived from
// insertMessageCols, so the two can never drift apart.
var messageUpdateSetClause = func() string {
	cols := strings.Split(insertMessageCols, ",")
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, strings.TrimSpace(c)+" = ?")
	}
	return strings.Join(parts, ", ")
}()

// messageRowEqual reports whether two messages would persist the
// same message row, tool_calls rows, and tool_result_events rows.
// Both sides go through the same resolve helpers the insert path
// uses, so equality is defined on exactly the persisted tuples.
func messageRowEqual(a, b Message) bool {
	aArgs, bArgs := messageInsertArgs(a), messageInsertArgs(b)
	for i := range aArgs {
		if aArgs[i] != bArgs[i] {
			return false
		}
	}

	aCalls := resolveToolCalls([]Message{a}, []int64{0})
	bCalls := resolveToolCalls([]Message{b}, []int64{0})
	if len(aCalls) != len(bCalls) {
		return false
	}
	for i := range aCalls {
		if !toolCallRowEqual(aCalls[i], bCalls[i]) {
			return false
		}
	}

	aEvents := resolveToolResultEvents([]Message{a})
	bEvents := resolveToolResultEvents([]Message{b})
	if len(aEvents) != len(bEvents) {
		return false
	}
	for i := range aEvents {
		if aEvents[i] != bEvents[i] {
			return false
		}
	}
	return true
}

// toolCallRowEqual compares the persisted tool_calls columns except
// message_id (rowid-derived on both sides of a diff).
func toolCallRowEqual(a, b ToolCall) bool {
	return a.SessionID == b.SessionID &&
		a.ToolName == b.ToolName &&
		a.Category == b.Category &&
		a.ToolUseID == b.ToolUseID &&
		a.InputJSON == b.InputJSON &&
		a.SkillName == b.SkillName &&
		a.ResultContentLength == b.ResultContentLength &&
		a.ResultContent == b.ResultContent &&
		a.SubagentSessionID == b.SubagentSessionID &&
		a.FilePath == b.FilePath &&
		a.CallIndex == b.CallIndex
}

type messageDiffUpdate struct {
	id  int64
	msg Message
}

type messageDiffPlan struct {
	updates []messageDiffUpdate
	inserts []Message
}

// planStoredMessageDiff loads the session's stored messages and
// plans an in-place diff against msgs. ok=false means the caller
// must use the full replace path; a load failure also degrades to
// full replace rather than failing the write.
func (db *DB) planStoredMessageDiff(
	sessionID string, msgs []Message,
) (messageDiffPlan, bool) {
	stored, err := db.GetAllMessages(
		context.Background(), sessionID,
	)
	if err != nil {
		return messageDiffPlan{}, false
	}
	return planSessionMessageDiff(stored, msgs)
}

// planSessionMessageDiff classifies incoming messages against stored
// rows by ordinal. It refuses (ok=false) when the diff cannot be
// applied safely or profitably:
//   - a stored ordinal is absent from the incoming set (truncation
//     or reordering needs delete handling and pin re-matching);
//   - duplicate ordinals on either side;
//   - a changed row's source_uuid differs from the stored row's
//     (the ordinal now holds a different message, so pins must be
//     re-matched by source_uuid — full replace's job);
//   - more than half the stored rows changed (an ordinal-shifting
//     rewrite, where full replace's source_uuid pin re-matching and
//     bulk FTS handling are the right tool).
func planSessionMessageDiff(
	stored, incoming []Message,
) (messageDiffPlan, bool) {
	if len(stored) == 0 {
		return messageDiffPlan{}, false
	}
	byOrdinal := make(map[int]Message, len(stored))
	for _, m := range stored {
		if _, dup := byOrdinal[m.Ordinal]; dup {
			return messageDiffPlan{}, false
		}
		byOrdinal[m.Ordinal] = m
	}

	var plan messageDiffPlan
	seen := make(map[int]bool, len(incoming))
	for _, m := range incoming {
		if seen[m.Ordinal] {
			return messageDiffPlan{}, false
		}
		seen[m.Ordinal] = true
		old, exists := byOrdinal[m.Ordinal]
		switch {
		case !exists:
			plan.inserts = append(plan.inserts, m)
		case !messageRowEqual(old, m):
			if old.SourceUUID != m.SourceUUID {
				return messageDiffPlan{}, false
			}
			plan.updates = append(plan.updates, messageDiffUpdate{
				id:  old.ID,
				msg: m,
			})
		}
	}
	for ord := range byOrdinal {
		if !seen[ord] {
			return messageDiffPlan{}, false
		}
	}
	if 2*len(plan.updates) > len(stored) {
		return messageDiffPlan{}, false
	}
	return plan, true
}

// applySessionMessageDiffTx persists a planned diff: changed rows
// are updated in place (keeping rowids, so pins survive and the FTS
// triggers reindex only those rows), their tool rows are rebuilt,
// and new ordinals are inserted through the normal insert path.
func applySessionMessageDiffTx(
	tx *sql.Tx, sessionID string, plan messageDiffPlan,
) error {
	if len(plan.updates) > 0 {
		updateSQL := "UPDATE messages SET " +
			messageUpdateSetClause + " WHERE id = ?"
		ids := make([]int64, 0, len(plan.updates))
		ordinals := make([]int, 0, len(plan.updates))
		msgs := make([]Message, 0, len(plan.updates))
		for _, u := range plan.updates {
			args := append(messageInsertArgs(u.msg), u.id)
			if _, err := tx.Exec(updateSQL, args...); err != nil {
				return fmt.Errorf(
					"updating message ord=%d: %w",
					u.msg.Ordinal, err,
				)
			}
			ids = append(ids, u.id)
			ordinals = append(ordinals, u.msg.Ordinal)
			msgs = append(msgs, u.msg)
		}
		if err := deleteToolRowsForMessagesTx(
			tx, sessionID, ids, ordinals,
		); err != nil {
			return err
		}
		if err := insertToolCallsTx(
			tx, resolveToolCalls(msgs, ids),
		); err != nil {
			return err
		}
		if err := insertToolResultEventsTx(
			tx, resolveToolResultEvents(msgs),
		); err != nil {
			return err
		}
	}

	if len(plan.inserts) > 0 {
		ids, err := insertMessagesTx(tx, plan.inserts)
		if err != nil {
			return err
		}
		if err := insertToolCallsTx(
			tx, resolveToolCalls(plan.inserts, ids),
		); err != nil {
			return err
		}
		if err := insertToolResultEventsTx(
			tx, resolveToolResultEvents(plan.inserts),
		); err != nil {
			return err
		}
	}
	return nil
}

// deleteToolRowsForMessagesTx clears tool_calls and
// tool_result_events for the updated messages so their rebuilt rows
// cannot duplicate the stale ones.
func deleteToolRowsForMessagesTx(
	tx *sql.Tx, sessionID string, ids []int64, ordinals []int,
) error {
	for start := 0; start < len(ids); start += diffDeleteChunkSize {
		end := min(start+diffDeleteChunkSize, len(ids))

		idArgs := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			idArgs = append(idArgs, id)
		}
		if _, err := tx.Exec(
			"DELETE FROM tool_calls WHERE message_id IN ("+
				placeholderList(len(idArgs))+")",
			idArgs...,
		); err != nil {
			return fmt.Errorf("deleting stale tool_calls: %w", err)
		}

		ordArgs := make([]any, 0, 1+end-start)
		ordArgs = append(ordArgs, sessionID)
		for _, ord := range ordinals[start:end] {
			ordArgs = append(ordArgs, ord)
		}
		if _, err := tx.Exec(
			"DELETE FROM tool_result_events WHERE session_id = ?"+
				" AND tool_call_message_ordinal IN ("+
				placeholderList(len(ordArgs)-1)+")",
			ordArgs...,
		); err != nil {
			return fmt.Errorf(
				"deleting stale tool_result_events: %w", err,
			)
		}
	}
	return nil
}

func placeholderList(n int) string {
	return strings.TrimSuffix(
		strings.Repeat("?,", n), ",",
	)
}
