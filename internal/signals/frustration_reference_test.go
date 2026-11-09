package signals

import (
	"strings"
	"unicode"
)

// referenceIsFrustrationMarker is IsFrustrationMarker in its original
// step-by-step form: each step uses the standard library and the regular
// expressions directly, with no fused passes or prefilters. Tests compare
// IsFrustrationMarker with it, so a change that alters any prompt's result
// fails.
func referenceIsFrustrationMarker(content string) bool {
	normalized := referenceNormalizePrompt(content)
	if len(normalized) < FrustrationMinBytes {
		return false
	}
	if frustrationPhraseRe.MatchString(normalized) {
		return true
	}
	words := strings.FieldsFunc(codeFenceRe.ReplaceAllString(content, " "), func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	if len(words) < FrustrationCapsMinWords {
		return false
	}
	total, caps := 0, 0
	for _, word := range words {
		if len([]rune(word)) < 2 {
			continue
		}
		total++
		hasLower, hasUpper := false, false
		for _, r := range word {
			hasLower = hasLower || unicode.IsLower(r)
			hasUpper = hasUpper || unicode.IsUpper(r)
		}
		if hasUpper && !hasLower {
			caps++
		}
	}
	if total < FrustrationCapsMinWords {
		return false
	}
	return float64(caps)/float64(total) >= FrustrationCapsRatio
}

// referenceNormalizePrompt is normalizePrompt as first written.
func referenceNormalizePrompt(content string) string {
	withoutCode := codeFenceRe.ReplaceAllString(content, " ")
	lower := strings.ToLower(strings.TrimSpace(withoutCode))
	var b strings.Builder
	inSpace, wrote := false, false
	for _, r := range lower {
		if unicode.IsSpace(r) {
			if wrote {
				inSpace = true
			}
			continue
		}
		if inSpace {
			b.WriteByte(' ')
			inSpace = false
		}
		b.WriteRune(r)
		wrote = true
	}
	return b.String()
}
