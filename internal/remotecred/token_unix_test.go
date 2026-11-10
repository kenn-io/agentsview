//go:build !windows

package remotecred

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestTokenRejectsFIFOWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, unix.Mkfifo(p, 0o600))
	done := make(chan error, 1)
	go func() { _, err := ReadToken(p); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "token reader blocked on FIFO")
	}
	require.Error(t, validateTokenStat(&unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uint32(os.Geteuid()) + 1}))
}

func TestTokenStatRejectsNonRegularFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat unix.Stat_t
	}{
		{"character device", unix.Stat_t{Mode: unix.S_IFCHR | 0o600}},
		{"block device", unix.Stat_t{Mode: unix.S_IFBLK | 0o600}},
		{"directory", unix.Stat_t{Mode: unix.S_IFDIR | 0o600}},
		{"fifo", unix.Stat_t{Mode: unix.S_IFIFO | 0o600}},
		{"socket", unix.Stat_t{Mode: unix.S_IFSOCK | 0o600}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.stat.Uid = uint32(os.Geteuid())
			require.Error(t, validateTokenStat(&tc.stat))
		})
	}
	require.NoError(t, validateTokenStat(&unix.Stat_t{
		Mode: unix.S_IFREG | 0o600, Uid: uint32(os.Geteuid()),
	}))
}
