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
// single event. A call whose summary is genuinely empty keeps length zero and
// is left alone, and a blocked category (whose event content is blanked as
// well) re-derives an empty string, which is exactly what it stores today.

// ResultContentDuplicatesSingleEvent reports whether a summary repeats the
// content of the call's only result event verbatim.
func ResultContentDuplicatesSingleEvent(
	summary string, events []ToolResultEvent,
) bool {
	return summary != "" && len(events) == 1 &&
		events[0].Content == summary
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
	if tc.ResultContent != "" || tc.ResultContentLength == 0 ||
		len(tc.ResultEvents) != 1 {
		return
	}
	tc.ResultContent = tc.ResultEvents[0].Content
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
			SELECT CASE WHEN COUNT(*) = 1 THEN MIN(tre_rc.content) END
			FROM tool_result_events tre_rc
			WHERE tre_rc.session_id = %[1]s.session_id
			  AND tre_rc.tool_call_message_ordinal = %[2]s
			  AND tre_rc.call_index = COALESCE(%[1]s.call_index, 0)
		), '')
	END`, callAlias, ordinalExpr)
}
