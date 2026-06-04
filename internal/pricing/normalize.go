package pricing

import "strings"

// NormalizeModelName converts a model id's dots to dashes so agents
// that report dotted ids (e.g. opencode's claude-opus-4.7) match the
// dashed LiteLLM pricing keys (claude-opus-4-7). Use only as a
// fallback after an exact match.
func NormalizeModelName(model string) string {
	return strings.ReplaceAll(model, ".", "-")
}

// Resolve looks up model in m, falling back to NormalizeModelName when
// there is no exact match. Shared by the sqlite and postgres lookups.
func Resolve[T any](m map[string]T, model string) (T, bool) {
	if v, ok := m[model]; ok {
		return v, true
	}
	if norm := NormalizeModelName(model); norm != model {
		if v, ok := m[norm]; ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}
