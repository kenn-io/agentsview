package pricing

import (
	"slices"
	"strings"
)

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
// there is no exact match, then case-insensitive matches, and finally
// canonical matches with curated trailing decorations stripped.
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
	// 4. Canonical match with curated decoration stripping
	return resolveCanonical(m, model)
}

// resolveCanonical matches the canonicalized model name exactly against
// canonicalized keys, retrying with a trailing bracketed or
// parenthesized decoration removed ("claude-fable-5[1m]",
// "Gemini 3.5 Flash (Medium)") and then a trailing -YYYYMMDD release
// date removed. Earlier (less-stripped) candidates win. Arbitrary
// substring matching is deliberately avoided: a shorter pricing key
// inside a longer model name (gpt-5.5 inside gpt-5.5-codex) would
// silently misprice a distinct model that should stay unpriced. When
// both the model and a key carry a provider prefix, the prefixes must
// match so one provider's model never takes another's pricing.
func resolveCanonical[T any](m map[string]T, model string) (T, bool) {
	var candidates []string
	addCandidate := func(s string) {
		c := canonicalize(s)
		if c != "" && !slices.Contains(candidates, c) {
			candidates = append(candidates, c)
		}
	}
	addCandidate(model)
	undecorated := stripTrailingGroup(model)
	addCandidate(undecorated)
	addCandidate(stripTrailingDate(undecorated))
	if len(candidates) == 0 {
		var zero T
		return zero, false
	}

	var bestVal T
	bestIdx := len(candidates)
	modelProvider := canonicalProvider(model)
	for k, v := range m {
		if p := canonicalProvider(k); p != "" &&
			modelProvider != "" && p != modelProvider {
			continue
		}
		kCanon := canonicalize(k)
		if kCanon == "" {
			continue
		}
		for i, c := range candidates[:bestIdx] {
			if kCanon == c {
				bestVal = v
				bestIdx = i
				break
			}
		}
	}
	if bestIdx < len(candidates) {
		return bestVal, true
	}
	var zero T
	return zero, false
}

// canonicalProvider returns the canonicalized provider prefix of a
// model name ("openai/gpt-5.5" -> "openai"), or "" when unqualified.
func canonicalProvider(s string) string {
	idx := strings.LastIndex(s, "/")
	if idx <= 0 {
		return ""
	}
	var sb strings.Builder
	for _, r := range strings.ToLower(s[:idx]) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// stripTrailingGroup removes one trailing parenthesized or bracketed
// decoration: "Gemini 3.5 Flash (Medium)" -> "Gemini 3.5 Flash",
// "claude-fable-5[1m]" -> "claude-fable-5".
func stripTrailingGroup(s string) string {
	t := strings.TrimRight(s, " ")
	var open string
	switch {
	case strings.HasSuffix(t, ")"):
		open = "("
	case strings.HasSuffix(t, "]"):
		open = "["
	default:
		return s
	}
	i := strings.LastIndex(t, open)
	if i <= 0 {
		return s
	}
	return strings.TrimRight(t[:i], " ")
}

// stripTrailingDate removes a trailing -YYYYMMDD release-date suffix:
// "claude-opus-4-7-20260101" -> "claude-opus-4-7".
func stripTrailingDate(s string) string {
	i := strings.LastIndex(s, "-")
	if i <= 0 || len(s)-i-1 != 8 {
		return s
	}
	for _, r := range s[i+1:] {
		if r < '0' || r > '9' {
			return s
		}
	}
	return s[:i]
}
