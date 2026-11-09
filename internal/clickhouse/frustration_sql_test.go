package clickhouse

import (
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
)

// ClickHouse decides the marker of a prompt made of ASCII and these
// symbols, so Go must see each symbol as a non-letter, non-space rune that
// lowercasing leaves alone. A Unicode table update that breaks this for
// any rune fails here, before the two paths can count differently.
func TestPlainPromptSymbolsAreInertForMarkerRules(t *testing.T) {
	t.Parallel()
	check := func(r rune) {
		assert.False(t, unicode.IsLetter(r), "U+%04X is a letter", r)
		assert.False(t, unicode.IsSpace(r), "U+%04X is a space", r)
		assert.Equal(t, r, unicode.ToLower(r), "U+%04X changes when lowercased", r)
	}
	for _, rg := range chPlainPromptSymbols.R16 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			check(r)
		}
	}
	for _, rg := range chPlainPromptSymbols.R32 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			check(r)
		}
	}
}
