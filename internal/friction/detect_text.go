package friction

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
)

const workaroundContextRunes = 200

// DetectWorkarounds emits one signal per matching assistant message. The
// lowest-index matching pattern wins; context is limited to 200 runes.
func DetectWorkarounds(msgs []Message, subjectID string) []Signal {
	var out []Signal
	for _, m := range msgs {
		if m.Role != "assistant" || m.Text == "" {
			continue
		}
		idx, ok := firstMatch(workaroundPatterns, m.Text)
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
	var out []Signal
	for _, m := range msgs {
		if m.Role != "assistant" || m.Text == "" {
			continue
		}
		idx, ok := firstMatch(deferralPatterns, m.Text)
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
