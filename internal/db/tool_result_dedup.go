package db

import (
	"fmt"
	"slices"
	"strings"
)

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
//
// A blank summary with a kept length has a second, newer meaning too: a
// provider-history restore can deliberately leave the chronologically latest
// event blank, with only its archived length kept, when the archive held a
// length but no text (ArchiveContentTranscripts/ArchiveContentUsage). That
// event explicitly marks itself with SummaryParticipates so every consumer
// below can tell "the latest state is blank on purpose" apart from "an
// ordinary empty timing event that never had anything to say" and from "a
// different, unrelated event elsewhere in the same call happens to be
// blank." ToolResultEventParticipates is the one predicate all of them use.

// ToolResultEventParticipates reports whether event should be treated as
// contributing to, or itself deciding, a call's summary. PrepareToolResultEvent
// and a provider-history restore are the only writers of SummaryParticipates;
// everything else infers it from whether the event has real text.
func ToolResultEventParticipates(event ToolResultEvent) bool {
	if event.SummaryParticipates != nil {
		return *event.SummaryParticipates
	}
	return strings.TrimSpace(event.Content) != ""
}

// lastParticipatingEventIsBlank reports whether the chronologically last
// participating event (walking backward, skipping any trailing event that
// doesn't participate) has blank content. That is the signal that a call's
// blank, length-only summary is a deliberately withheld correction, never
// safe to reconstruct from some earlier, unrelated event's content. No
// participating event at all reports false, deferring to the normal
// sole-event search below.
func lastParticipatingEventIsBlank(events []ToolResultEvent) bool {
	for _, event := range slices.Backward(events) {
		if !ToolResultEventParticipates(event) {
			continue
		}
		return event.Content == ""
	}
	return false
}

// SoleToolResultContent finds the call's one content-bearing event, ignoring
// empty timing events, and refuses outright when the chronologically latest
// participating event is itself a deliberately withheld, blank correction —
// that state must never be mistaken for a dedup match against some earlier,
// unrelated event's content.
func SoleToolResultContent(events []ToolResultEvent) (ToolResultEvent, bool) {
	if lastParticipatingEventIsBlank(events) {
		return ToolResultEvent{}, false
	}
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
// ResultContent it saw when the summary was stored twice. SoleToolResultContent
// itself refuses when the chronologically latest event is a withheld
// correction, so this stays the plain "blank summary with a kept length"
// check its name suggests.
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
// It reproduces RestoreToolCallResultContent's own logic: refill from the
// sole content-bearing event, unless the chronologically latest participating
// event (by event_index, matching PairToolResultsContext's own append order)
// is itself blank — summary_participates NULL infers from whether that
// event has content, the same default ToolResultEventParticipates applies.
func ToolCallResultContentSQL(callAlias, ordinalExpr string) string {
	return fmt.Sprintf(`CASE
		WHEN COALESCE(%[1]s.result_content, '') <> ''
			THEN %[1]s.result_content
		WHEN COALESCE(%[1]s.result_content_length, 0) = 0 THEN ''
		WHEN COALESCE((
			SELECT CASE WHEN COALESCE(tre_last.content, '') = '' THEN 1 ELSE 0 END
			FROM tool_result_events tre_last
			WHERE tre_last.session_id = %[1]s.session_id
			  AND tre_last.tool_call_message_ordinal = %[2]s
			  AND tre_last.call_index = COALESCE(%[1]s.call_index, 0)
			  AND COALESCE(tre_last.summary_participates,
				CASE WHEN COALESCE(tre_last.content, '') <> '' THEN 1 ELSE 0 END) = 1
			ORDER BY tre_last.event_index DESC
			LIMIT 1
		), 0) = 1 THEN ''
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
