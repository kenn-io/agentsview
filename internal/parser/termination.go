package parser

// TerminationStatus describes how a parsed session appears to have
// ended. The empty string means "unknown" — caller should leave the
// stored column NULL.
type TerminationStatus string

const (
	TerminationClean           TerminationStatus = "clean"
	TerminationToolCallPending TerminationStatus = "tool_call_pending"
	TerminationTruncated       TerminationStatus = "truncated"
)

// Classify returns a status given a parsed message slice and a
// sentinel from the file scanner. Returns "" (unknown) when no
// classification can be made — for example, an empty message slice
// from an unparseable file. Truncation takes precedence over
// tool_call_pending: if the file was cut off mid-write, that's the
// stronger signal about what went wrong.
func Classify(messages []ParsedMessage, fileTruncated bool) TerminationStatus {
	if fileTruncated {
		return TerminationTruncated
	}
	if len(messages) == 0 {
		return ""
	}
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	return TerminationClean
}

// hasOrphanedToolCall reports whether the last assistant message has
// any tool_use blocks that lack a matching tool_result anywhere in
// the message slice.
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
	for _, m := range messages {
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
