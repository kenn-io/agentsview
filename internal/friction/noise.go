package friction

import (
	"regexp"
	"strings"
)

// bareTimeoutRe is jilog detectors.rs:326. RE2 \d is ASCII-only (Rust's is
// Unicode); pinned by TestBareTimeoutDigitClass.
var bareTimeoutRe = regexp.MustCompile(`(?i)^command timed out after \d+ seconds?\.?$`)

// isExpectedNoise adapts jilog's content-free bash rule (detectors.rs:279-488,
// jilog#42fd) to plain-text results: a failed bash call whose text is blank
// or only the timeout sentence carries no diagnostic. Anything else is
// emitted.
func isExpectedNoise(toolName, text string) bool {
	if toolName != "bash" {
		return false
	}
	t := strings.TrimSpace(text)
	return t == "" || bareTimeoutRe.MatchString(t)
}
