package gittrailer

import (
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvocationUsesLiveProcessLifetime(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no process lifetime lookup on this platform")
	}
	pid := strconv.Itoa(os.Getpid())
	first, err := invocationForPID(pid)
	require.NoError(t, err)
	again, err := invocationForPID(pid)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.Len(t, first, 64)
	for _, invalid := range []string{"", "0", "-1", "4294967296", "x", "1\n"} {
		_, err := invocationForPID(invalid)
		require.Error(t, err)
	}
}
