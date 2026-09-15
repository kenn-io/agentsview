package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
			require.Equal(t, tc.want, SanitizeUTF8(got))
			// Put controls and multi-byte runes across each block boundary.
			for offset := range 16 {
				prefix := strings.Repeat("a", offset)
				const suffix = " ordinary text"
				require.Equal(t, prefix+tc.want+suffix,
					SanitizeUTF8(prefix+tc.input+suffix), "offset %d", offset)
			}
		})
	}
}

func BenchmarkSanitizeTranscriptText(b *testing.B) {
	text := strings.Repeat("A transcript contains ordinary text, punctuation, and newlines.\n", 256)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for b.Loop() {
		SanitizeUTF8(text)
	}
}
