package terminaltext

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text", in: "hello world", want: "hello world"},
		{name: "preserves layout", in: "line 1\nline 2\tvalue", want: "line 1\nline 2\tvalue"},
		{name: "removes carriage returns", in: "safe\rspoof", want: "safespoof"},
		{name: "removes ANSI introducer", in: "before\x1b[1Aafter", want: "before[1Aafter"},
		{name: "removes OSC controls", in: "\x1b]52;c;ZXZpbA==\x07done", want: "]52;c;ZXZpbA==done"},
		{name: "removes C1 and invalid UTF-8", in: "a\x80b\x9fc", want: "abc"},
		{name: "preserves Unicode", in: "héllo 日本語 😀", want: "héllo 日本語 😀"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Sanitize(tc.in))
		})
	}
}
