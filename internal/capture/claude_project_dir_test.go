package capture

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClaudeProjectDirLongPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX absolute path")
	}
	cwd := "/" + strings.Repeat("a", 200)
	assert.Equal(t, "-"+strings.Repeat("a", 199)+"-b6ymvl", ClaudeProjectDir(cwd))
}
