//go:build !windows

package remotesync

import (
	"bytes"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteArchiveSkipsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		done <- WriteArchive(&buf, TargetSet{ExtraFiles: []string{fifo}})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "WriteArchive blocked on FIFO")
	}
}
