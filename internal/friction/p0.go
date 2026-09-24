package friction

import "sort"

// DetectP0Alerts groups error signals by tool and returns sorted subject IDs
// for tools with at least P0DistinctSessionThreshold distinct sessions.
// Diagnostic subjects and subjects accepted by isSubAgent are excluded.
// A nil predicate excludes no sub-agents. The result is always non-nil.
func DetectP0Alerts(errors []Signal, isSubAgent func(subjectID string) bool) map[string][]string {
	byTool := make(map[string]map[string]struct{})
	for _, signal := range errors {
		if signal.Kind != KindError || signal.SubjectKind == SubjectDiagnostic {
			continue
		}
		if isSubAgent != nil && isSubAgent(signal.SubjectID) {
			continue
		}
		if byTool[signal.ToolName] == nil {
			byTool[signal.ToolName] = make(map[string]struct{})
		}
		byTool[signal.ToolName][signal.SubjectID] = struct{}{}
	}

	alerts := make(map[string][]string)
	for tool, subjects := range byTool {
		if len(subjects) < P0DistinctSessionThreshold {
			continue
		}
		ids := make([]string, 0, len(subjects))
		for id := range subjects {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		alerts[tool] = ids
	}
	return alerts
}
