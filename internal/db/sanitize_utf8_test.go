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
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeUTF8(tc.input)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.want, SanitizeUTF8(got))
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
