package friction

// DetectErrors emits one signal per failed tool message that is not
// expected noise. It adapts jilog's detect_errors (detectors.rs:205-269):
// the caller sets Failed and Text from the archived tool call instead of
// passing a {"error","success"} JSON envelope, and Text is the error
// message.
func DetectErrors(msgs []Message, subjectID string) []Signal {
	var out []Signal
	for _, m := range msgs {
		if m.Role != "tool" || !m.Failed {
			continue
		}
		toolName := m.ToolName
		if toolName == "" {
			toolName = "unknown"
		}
		noiseName := m.NoiseName
		if noiseName == "" {
			noiseName = toolName
		}
		if isExpectedNoise(noiseName, m.Text) {
			continue
		}
		out = append(out, Signal{
			Kind:        KindError,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    DetectorError,
			Text:        m.Text,
			ToolName:    toolName,
			Ordinal:     new(m.Ordinal),
			CallIndex:   new(m.CallIndex),
			OccurredAt:  m.Timestamp,
		})
	}
	return out
}
