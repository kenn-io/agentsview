package db

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacySanitizeUTF8 is the multi-pass sanitizer that SanitizeUTF8
// replaced. It is the reference for the differential test: the fast
// path may change how clean text is detected, never what is stored.
func legacySanitizeUTF8(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	strip := func(r rune) bool {
		return r != '\n' && r != '\t' && r != '\r' && unicode.IsControl(r)
	}
	if strings.IndexFunc(s, strip) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if strip(r) {
			return -1
		}
		return r
	}, s)
}

func TestSanitizeUTF8PreservesCleanTextAndRepairsControls(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"ascii whitespace", "text\n\tline\r", "text\n\tline\r"},
		{"unicode", "中文 café \ufffd", "中文 café \ufffd"},
		{"nul", "a\x00b", "ab"},
		{"terminal controls", "a\x1b]0;title\x07b\x7f", "a]0;titleb"},
		{"unicode controls", "a\u0080b\u009fc\u00a0", "abc\u00a0"},
		{"invalid utf8", "a\xff\xfeb\xe2\x82", "ab"},
		{"ascii bounds", " !~\x7f\x1f", " !~"},
		{"mixed byte controls", "\x00 \xff~\x7f", " ~"},
		{"clean c2 prefix", "\u00a0\u00a3\u00a9\u00bf", "\u00a0\u00a3\u00a9\u00bf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeUTF8(tc.input)
			require.Equal(t, tc.want, got)
			assert.Equal(t, tc.want, SanitizeUTF8(got), "idempotent")
			// Put controls and multi-byte runes across each 8-byte
			// block boundary and leave short tails behind them.
			for offset := range 16 {
				prefix := strings.Repeat("a", offset)
				const suffix = " ordinary text"
				assert.Equal(t, prefix+tc.want+suffix,
					SanitizeUTF8(prefix+tc.input+suffix), "offset %d", offset)
				assert.Equal(t, prefix+tc.want,
					SanitizeUTF8(prefix+tc.input), "offset %d, no suffix", offset)
			}
		})
	}
}

// TestSanitizeUTF8MatchesLegacyOnMixedInput compares SanitizeUTF8
// against the multi-pass reference on generated inputs that mix
// printable ASCII runs of every length with the byte classes the
// sanitizer treats specially.
func TestSanitizeUTF8MatchesLegacyOnMixedInput(t *testing.T) {
	fragments := []string{
		"\x00", "\x01", "\x1b", "\x1f", " ", "~", "\x7f",
		"\t", "\n", "\r", "\r\n",
		"\xff", "\xfe", "\x80", "\xc2", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80",
		"\u0080", "\u009f", "\u00a0", "\u00e9", "\u4e2d", "\ufffd",
		"\U0001f600", "\u2028", "\u200b",
	}
	rng := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	for i := range 20000 {
		b.Reset()
		for range rng.IntN(12) {
			if rng.IntN(2) == 0 {
				// Printable ASCII run; lengths straddle the 8-byte block.
				for range rng.IntN(20) {
					b.WriteByte(byte(0x20 + rng.IntN(0x7f-0x20)))
				}
				continue
			}
			b.WriteString(fragments[rng.IntN(len(fragments))])
		}
		input := b.String()
		want := legacySanitizeUTF8(input)
		got := SanitizeUTF8(input)
		require.Equalf(t, want, got, "case %d input %q", i, input)
		require.Equalf(t, got, SanitizeUTF8(got), "case %d not idempotent", i)
	}
}

func BenchmarkSanitizeTranscriptText(b *testing.B) {
	for _, bc := range []struct{ name, line string }{
		{"ascii", "A transcript contains ordinary text, punctuation, and newlines.\n"},
		{"unicode", "Mixed text: café, naïve, 中文字符, and emoji \U0001f600 per line.\n"},
		{"needs_repair", "Terminal output \x1b[31mred\x1b[0m with a stray NUL \x00 byte.\n"},
	} {
		text := strings.Repeat(bc.line, 256)
		b.Run(bc.name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				SanitizeUTF8(text)
			}
		})
	}
}
