package parser

// CodexSessionSink receives the normalized operations of one Codex
// transcript decode. The decoder owns parse-time state (cursor, fork
// gate, prompt replay observation, pending-agent attribution) and emits
// through this interface; the sink owns the message stream, its
// call_id -> (message, call) index, deferred tool-result updates, and
// final ordinal normalization.
//
// The collecting implementation keeps the whole session in memory and
// reproduces the pre-streaming behavior exactly; the streaming
// implementation batches operations into a scratch store so memory is
// O(batch + unresolved state) instead of O(file size). Every semantic
// the legacy builder performed in-place maps to exactly one operation:
//
//   - AppendMessage        — append a message at the tail (sink assigns
//     the next ordinal; function-call messages register their call ids)
//   - ReserveOrdinal       — claim an ordinal slot without a message
//     (pending subagent notifications hold their position until they
//     are materialized or claimed by a late wait/spawn call)
//   - InsertMessage        — insert a message before the first message
//     with a greater ordinal (same-ordinal ties broken by timestamp,
//     then emission order), preserving the reserved slot's position
//   - AppendToolResultEvent— attach a result event to an emitted call,
//     or record a deferred update when the call id was never emitted
//     (equivalent events are deduplicated)
//   - SetCallSubagentSessionID — link an emitted call to its subagent
//   - ApplyTokenUsageToLastAssistant — attach token usage to the last
//     assistant message without usage, scanning back to the current
//     turn's user boundary
//   - InsertOrphanMessage  — materialize a pending notification at its
//     reserved ordinal, deduplicated by key
//   - Finalize             — stable-sort by ordinal (ties keep emission
//     order) and renumber 0..n-1
type CodexSessionSink interface {
	AppendMessage(m ParsedMessage) int
	ReserveOrdinal() int
	InsertMessage(m ParsedMessage) int
	AppendToolResultEvent(callID string, target *ParsedToolCallPosition, ev ParsedToolResultEvent)
	SetCallSubagentSessionID(callID string, target *ParsedToolCallPosition, sessionID string)
	ApplyTokenUsageToLastAssistant(raw string) bool
	InsertOrphanMessage(key string, m ParsedMessage) bool
	Finalize()
	Messages() []ParsedMessage
	ToolCallUpdates() []ParsedToolCallUpdate
}
