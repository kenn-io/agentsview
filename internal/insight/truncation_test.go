package insight

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLogLineUTF8(t *testing.T) {
	assert.Equal(t, "a... [truncated 4 bytes]", truncateLogLine("a\u65e5z", 3))
}
