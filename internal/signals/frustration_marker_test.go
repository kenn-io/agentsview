package signals

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frustrationEdgePrompts are prompts where a one-pass rewrite of
// IsFrustrationMarker is most likely to differ from the reference: Unicode
// spaces and case mappings that change byte lengths or reach ASCII,
// letters that are neither upper nor lower, invalid UTF-8, fences, and
// prompts at the length and ratio thresholds.
var frustrationEdgePrompts = []string{
	"",
	"wtf",
	"wtf wtf w",
	"wtf wtf wt",
	"   this is broken   ",
	"this\u00a0is\u2028broken",
	"come\n\t on, run it",
	"come ```x``` on, run it",
	"you bro\u212ae the build",
	"doe\u017f not work at all",
	"D\u0130D YOU READ IT",
	"\u0130\u0130\u0130\u0130\u0130",
	"\u023a\u023a\u023a\u023a\u023a",
	"\xff\xfe\xfd\xfc\xfb",
	"\xc5 \xc5 \xc5 \xc5 \xc5",
	"ABC def GHI jkl mno",
	"ABC def GHI jkl mno pqr",
	"AB CD ef gh ij",
	"\u01c5\u01c5 \u01c5\u01c5 \u01c5\u01c5 ok",
	"\u24b6\u24b7 \u24b8\u24b9 \u24ba\u24bb now",
	"\u03a3\u03a3 \u039b\u039b \u03a9\u03a9 ok go",
	"STRASSE \u00dfTRASSE ok fine",
	"```FUCK FUCK FUCK``` please handle it",
	"``` unclosed FUCK FUCK FUCK",
	"why won\u2019t it build",
	"why won't it build",
	"???",
	"what?? ok!! fine??",
	"x\u0085\u0085\u0085\u0085\u0085\u0085\u0085\u0085y",
}

// frustrationCorpusPieces build random prompts. They mix phrase fragments,
// every kind of whitespace, capitals, and the runes in
// frustrationEdgePrompts, so random joins form phrases across spaces and
// fences and land on both sides of the caps ratio.
var frustrationCorpusPieces = []string{
	" ", "  ", "\t", "\n", "\r\n", "\v", "\f", "\u0085", "\u00a0", "\u2028", "\u3000",
	"```", "```go\nPANIC NOW\n```",
	"!", "!!", "?", "??", ".", "'", "\u2019", "-", "1",
	"wtf", "WTF", "come", "on", "why", "won't", "this", "is", "broken",
	"doesn't", "does", "not", "work", "still", "same", "error", "you",
	"broke", "fuck", "FUCK", "fucking",
	"API", "URL", "OK", "I", "a", "please", "fix", "the", "test", "Build",
	"\u212a", "\u0130", "\u017f", "\u01c5", "\u24b6", "\u00aa", "\u03a3",
	"\u03c3", "\u00df", "\u023a", "\u4e2d\u6587", "\U0001f600", "\xff", "\xc5",
}

func randomFrustrationPrompt(r *rand.Rand) string {
	var b strings.Builder
	for range r.IntN(24) {
		b.WriteString(frustrationCorpusPieces[r.IntN(len(frustrationCorpusPieces))])
	}
	return b.String()
}

func TestIsFrustrationMarkerMatchesReference(t *testing.T) {
	prompts := append([]string(nil), frustrationEdgePrompts...)
	r := rand.New(rand.NewPCG(7, 11))
	for range 200_000 {
		prompts = append(prompts, randomFrustrationPrompt(r))
	}
	markers := 0
	for _, prompt := range prompts {
		want := referenceIsFrustrationMarker(prompt)
		require.Equal(t, want, IsFrustrationMarker(prompt), "%q", prompt)
		require.Equal(t, referenceNormalizePrompt(prompt), normalizePrompt(prompt), "%q", prompt)
		if want {
			markers++
		}
	}
	// Both answers must be common, or agreement would prove little.
	assert.Greater(t, markers, len(prompts)/10)
	assert.Less(t, markers, len(prompts)*9/10)
}

func TestIsFrustrationMarkerExamples(t *testing.T) {
	for prompt, want := range map[string]bool{
		"wtf":                                   false,
		"come\n\t on, run it":                   true,
		"you bro\u212ae the build":              true,
		"why won\u2019t it build":               false,
		"AB CD ef gh ij":                        true,
		"ABC def GHI jkl mno pqr":               false,
		"\u24b6\u24b7 \u24b8\u24b9 ok fine now": false,
		"```FUCK FUCK FUCK``` please handle it": false,
	} {
		assert.Equal(t, want, IsFrustrationMarker(prompt), "%q", prompt)
	}
}

func FuzzIsFrustrationMarkerMatchesReference(f *testing.F) {
	for _, prompt := range frustrationEdgePrompts {
		f.Add(prompt)
	}
	f.Fuzz(func(t *testing.T, prompt string) {
		require.Equal(t, referenceIsFrustrationMarker(prompt), IsFrustrationMarker(prompt), "%q", prompt)
		require.Equal(t, referenceNormalizePrompt(prompt), normalizePrompt(prompt), "%q", prompt)
	})
}
