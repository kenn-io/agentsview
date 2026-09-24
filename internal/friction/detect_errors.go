package friction

import "go.kenn.io/agentsview/internal/serdejson"

// DetectErrors ports detect_errors (jilog detectors.rs:205-245): a tool
// message whose text parses as a JSON object with "success" exactly false
// and that is not expected noise.
func DetectErrors(msgs []Message, subjectID string) []Signal {
	var out []Signal
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		v, err := serdejson.Decode([]byte(m.Text))
		if err != nil {
			continue
		}
		data, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if success, ok := data["success"].(bool); !ok || success {
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
		if isExpectedNoise(noiseName, data) {
			continue
		}
		out = append(out, Signal{
			Kind:        KindError,
			SubjectID:   subjectID,
			SubjectKind: SubjectSession,
			Detector:    DetectorError,
			Text:        extractErrorMessage(data),
			ToolName:    toolName,
			Ordinal:     new(m.Ordinal),
			CallIndex:   new(m.CallIndex),
			OccurredAt:  m.Timestamp,
		})
	}
	return out
}

// extractErrorMessage ports detectors.rs:252-269: error[0] for a non-empty
// array, error for any other non-null value, else the whole object as
// compact sorted JSON.
func extractErrorMessage(data map[string]any) string {
	switch e := data["error"].(type) {
	case nil:
	case []any:
		if len(e) > 0 {
			return valueAsString(e[0])
		}
	default:
		return valueAsString(e)
	}
	return serdejson.CompactString(data)
}

func valueAsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return serdejson.CompactString(v)
}
