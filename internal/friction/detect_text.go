package friction

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// The workaround patterns and labels port jilog detectors.rs:31-52. They are
// parallel by index; the patterns intentionally have no word boundaries.
var (
	workaroundPatterns = compileAll([]string{
		`(?i)for now`,
		`(?i)temporary`,
		`(?i)workaround`,
		`(?i)hardcoded`,
		`(?i)TODO`,
		`(?i)FIXME`,
		`(?i)quick fix`,
		`(?i)hack`,
	})
	workaroundLabels = []string{
		"for now", "temporary", "workaround", "hardcoded",
		"TODO", "FIXME", "quick fix", "hack",
	}
	workaroundMarkers = []string{
		"for now", "temporary", "workaround", "hardcoded",
		"todo", "fixme", "quick fix", "hack",
	}
)

// The deferral patterns and labels port jilog detectors.rs:55-78.
var (
	deferralPatterns = compileAll([]string{
		`(?i)\bI'?ll come back to (this|that|it)`,
		`(?i)\bdeferr?ing (this|that|it|until)`,
		`(?i)\bdefer (this|that|it)(?: (to|until|for))?`,
		`(?i)\bpunt(ing)? on (this|that|it)`,
		`(?i)\bleav(e|ing) (this|that|it) for (later|now|next)`,
		`(?i)\bskipping for now`,
		`(?i)\bpark(ing)? (this|that|it) for now`,
		`(?i)\bnext session`,
		`(?i)\bcircle back (to|on)`,
	})
	deferralLabels = []string{
		"come back later", "deferring", "defer", "punt", "leave for later",
		"skipping for now", "park for now", "next session", "circle back",
	}
	deferralMarkers = []string{
		"come back", "defer", "defer", "punt", "leav",
		"skipping", "park", "next session", "circle back",
	}
)

const workaroundContextRunes = 200

// firstTextMatch skips each pattern whose required literal is absent. A marker
// for one pattern must not trigger full-text scans by every earlier pattern.
// The regex still decides boundaries, case folding, and declaration precedence.
func firstTextMatch(text, lower string, patterns []*regexp.Regexp, markers []string) (int, bool) {
	for i, marker := range markers {
		before, _, found := strings.Cut(lower, marker)
		if !found {
			continue
		}
		// No match can precede its first required marker. Map rune positions
		// back to the original text because case folding can change byte widths.
		// Only "I'?ll " can precede a marker in these patterns (five bytes).
		// Keep one more byte so the regex sees the original word boundary.
		runes := utf8.RuneCountInString(before)
		start := 0
		for pos := range text {
			if runes == 0 {
				start = max(0, pos-6)
				break
			}
			runes--
		}
		if patterns[i].MatchString(text[start:]) {
			return i, true
		}
	}
	return 0, false
}

// foldAssistantTexts case-folds each assistant message for marker search, so
// the workaround and deferral detectors share one folded copy per message.
func foldAssistantTexts(msgs []Message) []string {
	folded := make([]string, len(msgs))
	for i, m := range msgs {
		if m.Role == "assistant" {
			// SimpleFold also equates long s with ASCII s; ToLower already maps Kelvin K.
			folded[i] = strings.ReplaceAll(strings.ToLower(m.Text), "ſ", "s")
		}
	}
	return folded
}

// DetectWorkarounds emits one signal per matching assistant message. The
// lowest-index matching pattern wins; context is limited to 200 runes.
func DetectWorkarounds(msgs []Message, subjectID string) []Signal {
	return detectWorkarounds(msgs, foldAssistantTexts(msgs), subjectID)
}

func detectWorkarounds(msgs []Message, folded []string, subjectID string) []Signal {
	var out []Signal
	for i, m := range msgs {
		if m.Role != "assistant" || m.Text == "" {
			continue
		}
		idx, ok := firstTextMatch(m.Text, folded[i], workaroundPatterns, workaroundMarkers)
		if !ok {
			continue
		}
		out = append(out, Signal{
			Kind:        KindWorkaround,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    DetectorWorkaround,
			Label:       workaroundLabels[idx],
			Text:        TruncateRunes(m.Text, workaroundContextRunes),
			Ordinal:     new(m.Ordinal),
			OccurredAt:  m.Timestamp,
		})
	}
	return out
}

// DetectDeferrals emits one label-only signal per matching assistant message.
// Its pattern precedence is independent of DetectWorkarounds.
func DetectDeferrals(msgs []Message, subjectID string) []Signal {
	return detectDeferrals(msgs, foldAssistantTexts(msgs), subjectID)
}

func detectDeferrals(msgs []Message, folded []string, subjectID string) []Signal {
	var out []Signal
	for i, m := range msgs {
		if m.Role != "assistant" || m.Text == "" {
			continue
		}
		idx, ok := firstTextMatch(m.Text, folded[i], deferralPatterns, deferralMarkers)
		if !ok {
			continue
		}
		out = append(out, Signal{
			Kind:        KindDeferral,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    DetectorDeferral,
			Label:       deferralLabels[idx],
			Ordinal:     new(m.Ordinal),
			OccurredAt:  m.Timestamp,
		})
	}
	return out
}
