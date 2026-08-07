// ABOUTME: Diff-based session message replacement: updates only the
// ABOUTME: rows that changed instead of delete+reinserting the session.
package db

import (
	"context"
	"reflect"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// messageRowEqual reports whether two messages have the same canonical
// message, tool-call, and tool-result projections. Deep comparison is required
// because nullable canonical fields are independently allocated pointers whose
// values, rather than pointer identities, define persistence equality.
func messageRowEqual(a, b Message) bool {
	aMessages, aCalls, aResults, err := CanonicalMessageRows([]Message{a})
	if err != nil {
		return false
	}
	bMessages, bCalls, bResults, err := CanonicalMessageRows([]Message{b})
	if err != nil {
		return false
	}
	return reflect.DeepEqual(aMessages, bMessages) &&
		reflect.DeepEqual(aCalls, bCalls) &&
		reflect.DeepEqual(aResults, bResults)
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
) (messageDiffPlan, []Message, bool, bool) {
	stored, err := db.GetAllMessages(
		context.Background(), sessionID,
	)
	if err != nil {
		return messageDiffPlan{}, nil, false, false
	}
	plan, useDiff := planSessionMessageDiff(stored, msgs)
	return plan, stored, useDiff, true
}

func transcriptMessagesEqual(stored, incoming []Message) bool {
	if len(stored) != len(incoming) {
		return false
	}
	byOrdinal := make(map[int]Message, len(stored))
	for _, msg := range stored {
		if _, exists := byOrdinal[msg.Ordinal]; exists {
			return false
		}
		byOrdinal[msg.Ordinal] = msg
	}
	seen := make(map[int]bool, len(incoming))
	for _, msg := range incoming {
		if seen[msg.Ordinal] {
			return false
		}
		seen[msg.Ordinal] = true
		old, exists := byOrdinal[msg.Ordinal]
		if !exists || !transcriptMessageEqual(old, msg) {
			return false
		}
	}
	return true
}

func transcriptMessageEqual(a, b Message) bool {
	comparableTranscriptMessage := func(msg Message) Message {
		msg.ID = 0
		msg.ContentLength = 0
		msg.TokenUsage = nil
		msg.ClaudeMessageID = ""
		msg.ClaudeRequestID = ""
		msg.SourceType = ""
		msg.SourceUUID = ""
		msg.SourceParentUUID = ""
		msg.IsSidechain = false
		msg.Timestamp = canonicalTranscriptTimestamp(msg.Timestamp)
		msg.ToolCalls = append([]ToolCall(nil), msg.ToolCalls...)
		for i := range msg.ToolCalls {
			msg.ToolCalls[i].ResultContentLength = 0
			msg.ToolCalls[i].ResultEvents = append(
				[]ToolResultEvent(nil),
				msg.ToolCalls[i].ResultEvents...,
			)
			for j := range msg.ToolCalls[i].ResultEvents {
				msg.ToolCalls[i].ResultEvents[j].ContentLength = 0
				msg.ToolCalls[i].ResultEvents[j].Timestamp = canonicalTranscriptTimestamp(
					msg.ToolCalls[i].ResultEvents[j].Timestamp,
				)
			}
		}
		return msg
	}
	return messageRowEqual(
		comparableTranscriptMessage(a),
		comparableTranscriptMessage(b),
	)
}

func canonicalTranscriptTimestamp(value string) string {
	parsed, err := bunmodel.ParseTimestamp(value)
	if err != nil {
		return value
	}
	return parsed.Time.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
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
	ctx context.Context, tx bun.IDB, sessionID string, plan messageDiffPlan,
) error {
	messages := make([]Message, 0, len(plan.updates)+len(plan.inserts))
	ordinals := make([]int, 0, cap(messages))
	for _, update := range plan.updates {
		messages = append(messages, update.msg)
		ordinals = append(ordinals, update.msg.Ordinal)
	}
	for _, message := range plan.inserts {
		messages = append(messages, message)
		ordinals = append(ordinals, message.Ordinal)
	}
	messageRows, callRows, resultRows, err := CanonicalMessageRows(messages)
	if err != nil {
		return err
	}
	return RepairMessageRows(
		ctx, tx, sessionID, ordinals, messageRows, callRows, resultRows,
	)
}
