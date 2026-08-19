package signals

import (
	"regexp"
	"strings"
)

// ToolCallRow is populated from a JOIN of tool_calls + messages.
type ToolCallRow struct {
	ToolName       string
	Category       string // "Bash", "Edit", "Write", "Read", "Search"
	InputJSON      string
	ResultContent  string
	MessageOrdinal int
	CallIndex      int
	EventStatus    string // "", "completed", "errored", "cancelled", "running"
	// ContentFailure is a pre-computed content-heuristic verdict used by
	// streaming writers whose rows carry placeholder result content (the
	// real summary lives in staging). When the last event carries no
	// status, IsFailure prefers this verdict over re-scanning
	// ResultContent. Legacy rows leave it false and fall through to the
	// content scan as before.
	ContentFailure bool
}

// ToolHealthSignals holds computed health metrics for a session's
// tool calls.
type ToolHealthSignals struct {
	FailureSignalCount    int
	RetryCount            int
	EditChurnCount        int
	ConsecutiveFailureMax int
}

var (
	goRoutineRe  = regexp.MustCompile(`goroutine \d+`)
	exitStatusRe = regexp.MustCompile(
		`exit (?:status|code) ([1-9]\d*)`,
	)
)

// ComputeToolHealth computes health signals from an ordered slice
// of tool call rows. Pure computation, no DB access.
func ComputeToolHealth(calls []ToolCallRow) ToolHealthSignals {
	var s ToolHealthSignals

	s.FailureSignalCount, s.ConsecutiveFailureMax =
		countFailures(calls)
	s.RetryCount = countRetries(calls)
	s.EditChurnCount = countEditChurn(calls)

	return s
}

// IsFailure returns true when a tool call represents a failure,
// either by event status, by a pre-computed content verdict, or
// by content heuristics.
func IsFailure(c ToolCallRow) bool {
	if c.EventStatus != "" {
		return c.EventStatus == "errored" ||
			c.EventStatus == "cancelled"
	}
	if c.ContentFailure {
		return true
	}
	return isContentFailure(c.Category, c.ResultContent)
}

func isContentFailure(category, content string) bool {
	switch category {
	case "Bash":
		return isBashFailure(content)
	case "Edit", "Write":
		return strings.Contains(content, "FAILED")
	default:
		return false
	}
}

func isBashFailure(content string) bool {
	if strings.Contains(content, "command not found") {
		return true
	}
	if strings.Contains(content, "Permission denied") {
		return true
	}
	if strings.Contains(
		content,
		"Traceback (most recent call last)",
	) {
		return true
	}
	if goRoutineRe.MatchString(content) {
		return true
	}
	if hasJSStackTrace(content) {
		return true
	}
	if exitStatusRe.MatchString(content) {
		return hasErrorCompanion(content)
	}
	return false
}

// hasJSStackTrace returns true when content has 3+ consecutive
// lines starting with "  at ".
func hasJSStackTrace(content string) bool {
	consecutive := 0
	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasPrefix(line, "  at ") {
			consecutive++
			if consecutive >= 3 {
				return true
			}
		} else {
			consecutive = 0
		}
	}
	return false
}

// hasErrorCompanion checks for error indicators that elevate a
// non-zero exit code into a real failure.
func hasErrorCompanion(content string) bool {
	companions := []string{
		"command not found",
		"No such file",
		"Permission denied",
		"fatal:",
		"panic:",
	}
	for _, c := range companions {
		if strings.Contains(content, c) {
			return true
		}
	}
	// Stack trace patterns also count as companions.
	if strings.Contains(
		content,
		"Traceback (most recent call last)",
	) {
		return true
	}
	if goRoutineRe.MatchString(content) {
		return true
	}
	return hasJSStackTrace(content)
}

func countFailures(
	calls []ToolCallRow,
) (failures, maxStreak int) {
	streak := 0
	for _, c := range calls {
		if IsFailure(c) {
			failures++
			streak++
			if streak > maxStreak {
				maxStreak = streak
			}
		} else {
			streak = 0
		}
	}
	return failures, maxStreak
}

// countRetries counts retried calls using a sliding window.
// 3+ consecutive calls with same ToolName AND identical InputJSON
// = (count - 1) retries per group.
func countRetries(calls []ToolCallRow) int {
	if len(calls) < 3 {
		return 0
	}

	total := 0
	runLen := 1

	for i := 1; i < len(calls); i++ {
		if calls[i].ToolName == calls[i-1].ToolName &&
			calls[i].InputJSON == calls[i-1].InputJSON {
			runLen++
		} else {
			if runLen >= 3 {
				total += runLen - 1
			}
			runLen = 1
		}
	}
	if runLen >= 3 {
		total += runLen - 1
	}
	return total
}

// countEditChurn counts churn events for Edit/Write calls.
// One churn event = 3+ edits to the same file within a 10-ordinal
// span.
func countEditChurn(calls []ToolCallRow) int {
	// Collect ordinals per file path.
	fileOrdinals := map[string][]int{}
	for _, c := range calls {
		if c.Category != "Edit" && c.Category != "Write" {
			continue
		}
		path := extractFilePath(c.InputJSON)
		if path == "" {
			continue
		}
		fileOrdinals[path] = append(
			fileOrdinals[path], c.MessageOrdinal,
		)
	}

	churn := 0
	for _, ords := range fileOrdinals {
		if hasChurnWindow(ords, 3, 10) {
			churn++
		}
	}
	return churn
}

// extractFilePath extracts file_path from InputJSON using simple
// string search to avoid JSON parsing overhead.
func extractFilePath(input string) string {
	marker := `"file_path":"`
	idx := strings.Index(input, marker)
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := strings.Index(input[start:], `"`)
	if end < 0 {
		return ""
	}
	return input[start : start+end]
}

// hasChurnWindow checks whether any sliding window of size
// windowSize in the ordinals slice fits within maxSpan ordinals.
// Ordinals need not be sorted -- we check all combinations.
func hasChurnWindow(
	ordinals []int, windowSize, maxSpan int,
) bool {
	n := len(ordinals)
	if n < windowSize {
		return false
	}
	// Check every contiguous window of windowSize in the
	// ordinals slice (already in insertion order).
	for i := 0; i <= n-windowSize; i++ {
		lo, hi := ordinals[i], ordinals[i]
		for j := i + 1; j < i+windowSize; j++ {
			if ordinals[j] < lo {
				lo = ordinals[j]
			}
			if ordinals[j] > hi {
				hi = ordinals[j]
			}
		}
		if hi-lo < maxSpan {
			return true
		}
	}
	return false
}
