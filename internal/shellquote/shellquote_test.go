package shellquote

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPOSIX(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"/homes/b", "/homes/b"},
		{"", "''"},
		{"/homes/My Codex", "'/homes/My Codex'"},
		{"/homes/it's", `'/homes/it'\''s'`},
		{"$HOME", "'$HOME'"},
	} {
		assert.Equal(t, tt.want, POSIX(tt.in), tt.in)
	}
}

func TestPowerShell(t *testing.T) {
	assert.Equal(t, `'C:\homes\codex two'`, PowerShell(`C:\homes\codex two`))
	assert.Equal(t, `'C:\it''s'`, PowerShell(`C:\it's`))
	assert.Equal(t, "'$env:HOME'", PowerShell("$env:HOME"), "single quotes keep $ literal")
}
