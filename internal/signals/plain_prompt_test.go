package signals

import (
	"regexp"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SQL stores decide the marker of a prompt made of ASCII and these
// symbols, so Go must see each symbol as a non-letter, non-space rune that
// lowercasing leaves alone. A Unicode table update that breaks this for
// any rune fails here, before the paths can count differently.
func TestPlainPromptSymbolsAreInertForMarkerRules(t *testing.T) {
	t.Parallel()
	runes := PlainPromptSymbolRunes()
	require.NotEmpty(t, runes)
	for _, r := range runes {
		assert.False(t, unicode.IsLetter(r), "U+%04X is a letter", r)
		assert.False(t, unicode.IsSpace(r), "U+%04X is a space", r)
		assert.Equal(t, r, unicode.ToLower(r), "U+%04X changes when lowercased", r)
	}
}

func TestNonPlainRunePatternMatchesOutsideTheSet(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(NonPlainRunePattern())
	for _, r := range PlainPromptSymbolRunes() {
		assert.False(t, re.MatchString(string(r)), "U+%04X is in the set", r)
	}
	for _, s := range []string{"a", "~", "\x00", "\x7f"} {
		assert.False(t, re.MatchString(s), "%q is ASCII", s)
	}
	for _, s := range []string{" ", "ª", "é", "ſ", " ", " ", "Ⓐ", "中", "\U0001FB00"} {
		assert.True(t, re.MatchString(s), "%q is outside the set", s)
	}
}
