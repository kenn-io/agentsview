package insight

import (
	"syscall"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
)

func TestWindowsArgLength_MatchesEscapeArg(t *testing.T) {
	for _, s := range []string{"", "plain", `x"y`, `x"y\`, "two words", `say "hi"`, `a\\"b c\`, `x\\\"y`, "tab\there\\", `"\"`, "日本 \"語\""} {
		assert.Equal(t, len(utf16.Encode([]rune(syscall.EscapeArg(s)))), windowsArgLength(s), s)
	}
}
