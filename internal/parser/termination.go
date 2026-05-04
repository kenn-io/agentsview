package parser

// TerminationStatus describes how a parsed session appears to have
// ended. The empty string means "unknown" — caller should leave the
// stored column NULL.
type TerminationStatus string

const (
	// TerminationAwaitingUser means the agent reached a clear
	// "I'm done, your turn" stopping point: Claude end_turn,
	// Codex task_complete, or equivalent for other agents. UI
	// surfaces this as a calm "waiting" indicator.
	TerminationAwaitingUser TerminationStatus = "awaiting_user"

	// TerminationClean means the session ended for a non-orphan,
	// non-truncated reason that ISN'T explicitly "agent waiting"
	// (e.g. Claude max_tokens or stop_sequence). Treated as
	// "session done" by the UI — no special indicator.
	TerminationClean TerminationStatus = "clean"

	// TerminationToolCallPending means the last assistant message
	// emitted a tool_use that never received a matching
	// tool_result. Could be a tool currently running, a permission
	// prompt waiting on the user, or a crashed agent — the JSONL
	// can't distinguish those without runtime info.
	TerminationToolCallPending TerminationStatus = "tool_call_pending"

	// TerminationTruncated means the session file was cut off
	// mid-write (e.g. last line is invalid JSON).
	TerminationTruncated TerminationStatus = "truncated"
)

// Classify returns a status given a parsed message slice, the
// last assistant message's stop_reason (or empty when unknown),
// and a sentinel from the file scanner. Returns "" (unknown) when
// no classification can be made — for example, an empty message
// slice from an unparseable file. Truncation takes precedence over
// tool_call_pending: if the file was cut off mid-write, that's
// the stronger signal about what went wrong.
//
// stopReason values that signal "agent waiting on user input" map
// to TerminationAwaitingUser. The vocabulary differs per agent:
// Claude uses "end_turn"; Codex uses "task_complete"; pass through
// the raw string and the helper recognizes both.
func Classify(
	messages []ParsedMessage,
	stopReason string,
	fileTruncated bool,
) TerminationStatus {
	if fileTruncated {
		return TerminationTruncated
	}
	if len(messages) == 0 {
		return ""
	}
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	// awaiting_user only applies when the assistant's "I'm done"
	// signal is the actual end of the transcript. If a user
	// message follows the last assistant turn, the agent is no
	// longer parked — the user has already replied, so the UI
	// should not show a "waiting for you" indicator.
	lastIsAssistant := messages[len(messages)-1].Role == RoleAssistant
	if lastIsAssistant && isAwaitingUserStopReason(stopReason) {
		return TerminationAwaitingUser
	}
	return TerminationClean
}

// isAwaitingUserStopReason reports whether the given stop_reason
// (from any agent's vocabulary) means the agent has finished its
// turn and is parked waiting for the user. The set of accepted
// values grows as more agents are wired up.
func isAwaitingUserStopReason(stopReason string) bool {
	switch stopReason {
	case "end_turn", // Claude
		"task_complete": // Codex
		return true
	}
	return false
}

// hasOrphanedToolCall reports whether the last assistant message has
// any tool_use blocks that lack a matching tool_result. Only results
// that appear AFTER the last assistant message resolve its calls —
// an earlier message reusing the same ToolUseID (rare, but possible
// in forked sessions or malformed transcripts) must not retroactively
// mark the final unresolved call as resolved.
func hasOrphanedToolCall(messages []ParsedMessage) bool {
	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx == -1 {
		return false
	}
	last := messages[lastAssistantIdx]
	if len(last.ToolCalls) == 0 {
		return false
	}

	resolved := make(map[string]bool)
	for _, m := range messages[lastAssistantIdx+1:] {
		for _, tr := range m.ToolResults {
			if tr.ToolUseID != "" {
				resolved[tr.ToolUseID] = true
			}
		}
	}

	for _, tc := range last.ToolCalls {
		if tc.ToolUseID != "" && !resolved[tc.ToolUseID] {
			return true
		}
	}
	return false
}
