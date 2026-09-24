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
	// ContentFailureKnown also represents a precomputed negative verdict.
	// Set only after the row's category and result content are final.
	ContentFailureKnown bool
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

	s.FailureSignalCount, s.ConsecutiveFailureMax = countFailures(calls)
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
	if c.ContentFailureKnown || c.ContentFailure {
		return c.ContentFailure
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

// retryRunMinLen is the shortest run of identical consecutive calls
// that counts as retrying.
const retryRunMinLen = 3

// ToolRun is one maximal run of consecutive calls with the same
// ToolName and byte-identical InputJSON, at least retryRunMinLen
// long. First and Last are the run's outermost calls.
type ToolRun struct {
	ToolName    string
	Count       int
	First, Last CallPos
}

// RetryRuns returns every maximal retry run in call order. It is the
// per-occurrence form of countRetries: countRetries == Σ(Count-1).
func RetryRuns(calls []ToolCallRow) []ToolRun {
	var runs []ToolRun
	start := 0
	for i := 1; i <= len(calls); i++ {
		if i < len(calls) &&
			calls[i].ToolName == calls[i-1].ToolName &&
			calls[i].InputJSON == calls[i-1].InputJSON {
			continue
		}
		if n := i - start; n >= retryRunMinLen {
			runs = append(runs, ToolRun{
				ToolName: calls[start].ToolName,
				Count:    n,
				First:    toolCallPos(calls[start]),
				Last:     toolCallPos(calls[i-1]),
			})
		}
		start = i
	}
	return runs
}

// countRetries counts retried calls: each run of 3+ consecutive calls
// with the same ToolName AND identical InputJSON adds (count - 1).
func countRetries(calls []ToolCallRow) int {
	total := 0
	for _, r := range RetryRuns(calls) {
		total += r.Count - 1
	}
	return total
}

func toolCallPos(c ToolCallRow) CallPos {
	return CallPos{MessageOrdinal: c.MessageOrdinal, CallIndex: c.CallIndex}
}

const (
	editChurnWindow  = 3
	editChurnMaxSpan = 10
)

// EditChurn is one churned file: FilePath is the raw file_path from
// the tool input. Count and First/Last describe the file's first
// churn cluster (see churnCluster).
type EditChurn struct {
	FilePath    string
	Count       int
	First, Last CallPos
}

// EditChurnFiles returns one entry per churned file, in the order the
// files were first edited. It is the per-occurrence form of
// countEditChurn: countEditChurn == len(EditChurnFiles).
func EditChurnFiles(calls []ToolCallRow) []EditChurn {
	type fileEdits struct {
		ords []int
		pos  []CallPos
	}
	var order []string
	byFile := map[string]fileEdits{}
	for _, c := range calls {
		if c.Category != "Edit" && c.Category != "Write" {
			continue
		}
		path := extractFilePath(c.InputJSON)
		if path == "" {
			continue
		}
		fe, ok := byFile[path]
		if !ok {
			order = append(order, path)
		}
		fe.ords = append(fe.ords, c.MessageOrdinal)
		fe.pos = append(fe.pos, toolCallPos(c))
		byFile[path] = fe
	}
	var out []EditChurn
	for _, path := range order {
		fe := byFile[path]
		start, end, ok := churnCluster(
			fe.ords, editChurnWindow, editChurnMaxSpan,
		)
		if !ok {
			continue
		}
		out = append(out, EditChurn{
			FilePath: path,
			Count:    end - start + 1,
			First:    fe.pos[start],
			Last:     fe.pos[end],
		})
	}
	return out
}

// countEditChurn counts churn events for Edit/Write calls.
// One churn event = 3+ edits to the same file within a 10-ordinal
// span.
func countEditChurn(calls []ToolCallRow) int {
	return len(EditChurnFiles(calls))
}

// churnCluster finds the first contiguous window of windowSize edits
// that hasChurnWindow accepts, then extends it over the following
// edits while the whole cluster still spans fewer than maxSpan
// ordinals. ok is true exactly when hasChurnWindow is.
func churnCluster(
	ordinals []int, windowSize, maxSpan int,
) (start, end int, ok bool) {
	n := len(ordinals)
	for i := 0; i <= n-windowSize; i++ {
		lo, hi := ordinals[i], ordinals[i]
		for j := i + 1; j < i+windowSize; j++ {
			lo, hi = min(lo, ordinals[j]), max(hi, ordinals[j])
		}
		if hi-lo >= maxSpan {
			continue
		}
		end := i + windowSize - 1
		for end+1 < n {
			nlo := min(lo, ordinals[end+1])
			nhi := max(hi, ordinals[end+1])
			if nhi-nlo >= maxSpan {
				break
			}
			lo, hi, end = nlo, nhi, end+1
		}
		return i, end, true
	}
	return 0, 0, false
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
