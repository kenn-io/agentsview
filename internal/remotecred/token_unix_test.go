//go:build !windows

package remotecred

import (
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenRejectsFIFOAndDevice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, unix.Mkfifo(p, 0600))
	done := make(chan error, 1)
	go func() { _, err := ReadToken(p); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "token reader blocked on FIFO")
	}
	_, err := ReadToken("/dev/null")
	require.Error(t, err)
	require.Error(t, validateTokenStat(&unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uint32(os.Geteuid()) + 1}))
}
