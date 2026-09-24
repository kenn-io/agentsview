package friction

// DetectInterruptions emits one signal per interrupted row (spec §6.8,
// D36). BuildSessionInput collects those rows in
// SessionInput.Interruptions because they are system rows the
// correction stream drops (D10). Text stays empty: the row is a fixed
// marker, and the finding's title is one identity per session.
func DetectInterruptions(marks []Message, subjectID string) []Signal {
	var out []Signal
	for _, m := range marks {
		out = append(out, Signal{
			Kind:        KindInterruption,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    DetectorInterruption,
			Ordinal:     new(m.Ordinal),
			OccurredAt:  m.Timestamp,
		})
	}
	return out
}
