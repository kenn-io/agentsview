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
// silently misprice a distinct model that should stay unpriced.
//
// Keys are ranked so resolution is deterministic when several keys
// canonicalize alike: a key whose provider prefix matches the model's
// wins, then an unqualified key, then a provider-qualified key for an
// unqualified model. Distinct keys tied within one rank are ambiguous
// and stay unresolved. Keys whose provider conflicts with a qualified
// model are never considered.
func resolveCanonical[T any](m map[string]T, model string) (T, bool) {
	var zero T
	candidates := canonicalCandidates(model)
	if len(candidates) == 0 {
		return zero, false
	}

	const ranks = 3
	modelProvider := canonicalProvider(model)
	counts := make([][ranks]int, len(candidates))
	vals := make([][ranks]T, len(candidates))
	for k, v := range m {
		keyProvider := canonicalProvider(k)
		if keyProvider != "" && modelProvider != "" &&
			keyProvider != modelProvider {
			continue
		}
		kCanon := canonicalize(k)
		if kCanon == "" {
			continue
		}
		rank := keyRank(modelProvider, keyProvider)
		for i, c := range candidates {
			if kCanon == c {
				counts[i][rank]++
				vals[i][rank] = v
				break
			}
		}
	}
	for i := range candidates {
		for r := range ranks {
			if counts[i][r] == 1 {
				return vals[i][r], true
			}
			if counts[i][r] > 1 {
				return zero, false
			}
		}
	}
	return zero, false
}

// keyRank orders canonical matches: same-provider key (0), unqualified
// key (1), provider-qualified key for an unqualified model (2).
func keyRank(modelProvider, keyProvider string) int {
	switch keyProvider {
	case "":
		return 1
	case modelProvider:
		return 0
	default:
		return 2
	}
}

// canonicalCandidates returns the canonical forms of model to try, in
// decreasing specificity: as reported, with one trailing bracketed or
// parenthesized decoration removed, and additionally with a trailing
// -YYYYMMDD release date removed.
func canonicalCandidates(model string) []string {
	var candidates []string
	add := func(s string) {
		c := canonicalize(s)
		if c != "" && !slices.Contains(candidates, c) {
			candidates = append(candidates, c)
		}
	}
	add(model)
	undecorated := stripTrailingGroup(model)
	add(undecorated)
	add(stripTrailingDate(undecorated))
	return candidates
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
