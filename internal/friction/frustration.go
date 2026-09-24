package friction

import "go.kenn.io/agentsview/internal/signals"

// frustrationTextRunes bounds the stored frustration text (spec §6.8).
const frustrationTextRunes = 200

// DetectFrustration persists agentsview's frustration markers as
// friction (spec §6.8, D36): one signal per user message for which
// signals.IsFrustrationMarker is true. The analytics rescan keeps its
// own count; this only records occurrences.
func DetectFrustration(msgs []Message, subjectID string) []Signal {
	var out []Signal
	for _, m := range msgs {
		if m.Role != "user" || !signals.IsFrustrationMarker(m.Text) {
			continue
		}
		out = append(out, Signal{
			Kind:        KindFrustration,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    DetectorFrustration,
			Text:        TruncateRunes(m.Text, frustrationTextRunes),
			Ordinal:     new(m.Ordinal),
			OccurredAt:  m.Timestamp,
		})
	}
	return out
}
