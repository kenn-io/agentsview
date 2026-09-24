package friction

import (
	"regexp"
	"strings"
)

// chatCorrectionPatterns is jilog detectors.rs:89-100, verbatim. Go RE2's
// \b is ASCII-only where Rust's is Unicode-aware; the difference is
// pinned by TestChatCorrectionWordBoundary (spec Open question 8).
var chatCorrectionPatterns = compileAll([]string{
	`(?i)^no[,.! ]`,
	`(?i)\bdon'?t\b`,
	`(?i)\bdo not\b`,
	`(?i)\bplease stop\b`,
	`(?i)\bstop (doing|replying|posting|answering|sending|using|adding)\b`,
	`(?i)\bwrong\b`,
	`(?i)\bincorrect\b`,
	`(?i)\bnot (that|what|like that|right|the right)\b`,
	`(?i)\bshould(n'?t| not| never)\b`,
	`(?i)\bthat'?s not\b`,
})

func compileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// firstMatch returns the index of the lowest-index pattern that matches
// anywhere in text (RegexSet semantics: declaration order, not position).
func firstMatch(res []*regexp.Regexp, text string) (int, bool) {
	for i, re := range res {
		if re.MatchString(text) {
			return i, true
		}
	}
	return 0, false
}

// DetectCorrections ports detect_corrections (jilog detectors.rs:108-168):
// an assistant→user→assistant window whose user text is at most 200 bytes
// raw and at least 15 bytes trimmed.
func DetectCorrections(msgs []Message, subjectID string) []Signal {
	return detectCorrections(msgs, subjectID, false)
}

// DetectCorrectionsChat ports detect_corrections_chat (detectors.rs:116-118):
// the coding rule plus a corrective-language gate on the trimmed text.
func DetectCorrectionsChat(msgs []Message, subjectID string) []Signal {
	return detectCorrections(msgs, subjectID, true)
}

func detectCorrections(msgs []Message, subjectID string, chat bool) []Signal {
	var out []Signal
	for i := 0; i+2 < len(msgs); i++ {
		a, u, b := msgs[i], msgs[i+1], msgs[i+2]
		if a.Role != "assistant" || u.Role != "user" || b.Role != "assistant" {
			continue
		}
		if u.HadToolResult {
			continue
		}
		if len(u.Text) > MaxCorrectionLength {
			continue
		}
		trimmed := strings.TrimSpace(u.Text)
		if len(trimmed) < MinCorrectionLength {
			continue
		}
		detector := DetectorCorrectionCoding
		if chat {
			if _, ok := firstMatch(chatCorrectionPatterns, trimmed); !ok {
				continue
			}
			detector = DetectorCorrectionChat
		}
		out = append(out, Signal{
			Kind:        KindCorrection,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    detector,
			Text:        u.Text,
			Ordinal:     new(u.Ordinal),
			OccurredAt:  u.Timestamp,
		})
	}
	return out
}
