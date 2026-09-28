package db

import "fmt"

// Tool-result summaries used to be stored twice: once as
// tool_calls.result_content and once as the tool_result_events row the
// summary was derived from. On a real archive that duplication accounted for
// roughly 40% of the file, because the overwhelming majority of calls have a
// single result event and the summary is that event's content byte for byte.
//
// Such a summary is no longer stored at all. result_content_length still
// records the summary's size, so a cleared column with a non-zero length is
// the signal that the read side must re-derive the summary from the call's
// sole content-bearing event. A genuinely empty summary keeps length zero and
// is left alone, and a blocked category (whose event content is blanked as
// well) re-derives an empty string, which is exactly what it stores today.

// SoleToolResultContent ignores empty timing events when finding one payload.
func SoleToolResultContent(events []ToolResultEvent) (ToolResultEvent, bool) {
	var sole ToolResultEvent
	found := false
	for _, event := range events {
		if event.Content == "" {
			continue
		}
		if found {
			return ToolResultEvent{}, false
		}
		sole, found = event, true
	}
	return sole, found
}

// ResultContentDuplicatesSingleEvent reports whether a summary repeats the
// content of the call's sole content-bearing event verbatim.
func ResultContentDuplicatesSingleEvent(
	summary string, events []ToolResultEvent,
) bool {
	sole, ok := SoleToolResultContent(events)
	return summary != "" && ok && sole.Content == summary
}

// DedupToolCallResultSummary returns the result summary to persist for a call
// with the given result events: empty when the events already carry the same
// bytes, and the summary itself otherwise.
func DedupToolCallResultSummary(
	summary string, events []ToolResultEvent,
) string {
	if ResultContentDuplicatesSingleEvent(summary, events) {
		return ""
	}
	return summary
}

// RestoreToolCallResultContent refills the summary that the write path
// dropped, so every consumer of a loaded tool call sees the same
// ResultContent it saw when the summary was stored twice.
func RestoreToolCallResultContent(tc *ToolCall) {
	if tc.ResultContent != "" || tc.ResultContentLength == 0 {
		return
	}
	if sole, ok := SoleToolResultContent(tc.ResultEvents); ok {
		tc.ResultContent = sole.Content
	}
}

// RestoreMessageResultContent applies RestoreToolCallResultContent across a
// loaded message slice. Call it once the messages carry both their tool calls
// and their result events.
func RestoreMessageResultContent(msgs []Message) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			RestoreToolCallResultContent(&msgs[i].ToolCalls[j])
		}
	}
}

// ToolCallResultContentSQL builds the SQL expression that yields a tool
// call's display result content for readers that select the column directly
// instead of loading tool calls with their events. callAlias is the
// tool_calls alias and ordinalExpr resolves to the owning message's ordinal.
func ToolCallResultContentSQL(callAlias, ordinalExpr string) string {
	return fmt.Sprintf(`CASE
		WHEN COALESCE(%[1]s.result_content, '') <> ''
			THEN %[1]s.result_content
		WHEN COALESCE(%[1]s.result_content_length, 0) = 0 THEN ''
		ELSE COALESCE((
			SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sole_rc.content) END
			FROM (
				SELECT tre_rc.content FROM tool_result_events tre_rc
				WHERE tre_rc.session_id = %[1]s.session_id
				  AND tre_rc.tool_call_message_ordinal = %[2]s
				  AND tre_rc.call_index = COALESCE(%[1]s.call_index, 0)
				  AND COALESCE(tre_rc.content, '') <> ''
				LIMIT 2
			) sole_rc
		), '')
	END`, callAlias, ordinalExpr)
}
