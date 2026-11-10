package signals

import (
	"fmt"
	"strings"
	"unicode"
)

// PlainPromptSymbols are the non-ASCII runes a prompt may contain and still
// have its frustration marker decided by a SQL store. Each is a symbol,
// punctuation, format, or private-use rune: not a letter, not whitespace,
// and unchanged by lowercasing. Over such a prompt the RE2 regular
// expressions ClickHouse and DuckDB run (UTF-8, ASCII \b) see the same
// words, spaces, and boundaries IsFrustrationMarker sees in Go. Prompts
// with any other non-ASCII rune go to Go. The set covers the dashes,
// quotes, arrows, check marks, box drawing, and emoji common in prompts.
var PlainPromptSymbols = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00A1, Hi: 0x00A9, Stride: 1},
		{Lo: 0x00AB, Hi: 0x00B4, Stride: 1},
		{Lo: 0x00B6, Hi: 0x00B9, Stride: 1},
		{Lo: 0x00BB, Hi: 0x00BF, Stride: 1},
		{Lo: 0x00D7, Hi: 0x00F7, Stride: 0x20},
		{Lo: 0x200B, Hi: 0x2027, Stride: 1},
		{Lo: 0x202A, Hi: 0x202E, Stride: 1},
		{Lo: 0x2030, Hi: 0x205E, Stride: 1},
		{Lo: 0x2060, Hi: 0x206F, Stride: 1},
		{Lo: 0x2190, Hi: 0x23FF, Stride: 1},
		{Lo: 0x2500, Hi: 0x27BF, Stride: 1},
		{Lo: 0x2B00, Hi: 0x2BFF, Stride: 1},
		{Lo: 0xE000, Hi: 0xF8FF, Stride: 1},
		{Lo: 0xFE00, Hi: 0xFE0F, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1F000, Hi: 0x1FAFF, Stride: 1},
	},
}

// NonPlainRunePattern returns an RE2 character class that matches a rune
// that is neither ASCII nor in PlainPromptSymbols. A prompt is plain when
// it is valid UTF-8 and the class matches nowhere in it.
func NonPlainRunePattern() string {
	var b strings.Builder
	b.WriteString(`[^\x00-\x7F`)
	add := func(lo, hi, stride uint32) {
		if stride == 1 {
			fmt.Fprintf(&b, `\x{%X}-\x{%X}`, lo, hi)
			return
		}
		for r := lo; r <= hi; r += stride {
			fmt.Fprintf(&b, `\x{%X}`, r)
		}
	}
	for _, r := range PlainPromptSymbols.R16 {
		add(uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
	}
	for _, r := range PlainPromptSymbols.R32 {
		add(r.Lo, r.Hi, r.Stride)
	}
	b.WriteString(`]`)
	return b.String()
}

// PlainPromptSymbolRunes returns every rune of PlainPromptSymbols, for
// tests that check how a store treats each one.
func PlainPromptSymbolRunes() []rune {
	var runes []rune
	for _, rg := range PlainPromptSymbols.R16 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			runes = append(runes, r)
		}
	}
	for _, rg := range PlainPromptSymbols.R32 {
		for r := rune(rg.Lo); r <= rune(rg.Hi); r += rune(rg.Stride) {
			runes = append(runes, r)
		}
	}
	return runes
}
