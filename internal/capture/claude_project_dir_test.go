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

func TestCaptureLegacyWorkDirEncoding(t *testing.T) {
	value := "segment-😀-" + strings.Repeat("a", 210)
	assert.Equal(t, "segment---"+strings.Repeat("a", 210), encodeClaudeWorkDir(value))
}
