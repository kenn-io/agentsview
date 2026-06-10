package pricing

import "strings"

// NormalizeModelName converts a model id's dots to dashes so agents
// that report dotted ids (e.g. opencode's claude-opus-4.7) match the
// dashed LiteLLM pricing keys (claude-opus-4-7). Use only as a
// fallback after an exact match.
func NormalizeModelName(model string) string {
	return strings.ReplaceAll(model, ".", "-")
}

// canonicalize strips any provider prefix (after the last '/') and
// converts the string to lowercase, removing all non-alphanumeric characters.
func canonicalize(s string) string {
	if idx := strings.LastIndex(s, "/"); idx != -1 {
		s = s[idx+1:]
	}
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// Resolve looks up model in m, falling back to NormalizeModelName when
// there is no exact match, and finally falling back to case-insensitive and
// substring matches on canonicalized names.
func Resolve[T any](m map[string]T, model string) (T, bool) {
	// 1. Exact match
	if v, ok := m[model]; ok {
		return v, true
	}
	// 2. Exact match on normalized (dotted to dashed)
	if norm := NormalizeModelName(model); norm != model {
		if v, ok := m[norm]; ok {
			return v, true
		}
	}
	// 3. Case-insensitive exact match
	lowerModel := strings.ToLower(model)
	for k, v := range m {
		if strings.ToLower(k) == lowerModel {
			return v, true
		}
	}
	lowerNorm := strings.ToLower(NormalizeModelName(model))
	for k, v := range m {
		if strings.ToLower(k) == lowerNorm {
			return v, true
		}
	}

	// 4. Substring match on canonicalized names
	modelCanon := canonicalize(model)
	if modelCanon == "" {
		var zero T
		return zero, false
	}

	var bestVal T
	var bestCanonLen int
	var bestExactCanon bool

	for k, v := range m {
		kCanon := canonicalize(k)
		if kCanon == "" {
			continue
		}
		if kCanon == modelCanon {
			if !bestExactCanon || len(kCanon) > bestCanonLen {
				bestVal = v
				bestCanonLen = len(kCanon)
				bestExactCanon = true
			}
		} else if !bestExactCanon && strings.Contains(modelCanon, kCanon) {
			if len(kCanon) > bestCanonLen {
				bestVal = v
				bestCanonLen = len(kCanon)
			}
		}
	}

	if bestCanonLen > 0 {
		return bestVal, true
	}

	var zero T
	return zero, false
}
