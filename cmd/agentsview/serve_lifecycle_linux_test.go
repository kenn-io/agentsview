//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestStopDaemonProcessTreatsZombieAsExited(t *testing.T) {
	for _, tt := range []struct {
		name  string
		start func(*testing.T) int
	}{
		{name: "graceful", start: startSleepProcess},
		{name: "forced", start: startTERMIgnoringProcess},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setStartProbeTickForTest(t, 10*time.Millisecond)
			dir := runtimeTestDir(t)
			// Neither helper reaps the child until cleanup, so stop must
			// release the runtime record while the child is still a zombie.
			pid := tt.start(t)
			identity, ok := daemon.ReadProcessIdentity(pid)
			require.True(t, ok)
			path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
				PID:               pid,
				Network:           daemon.NetworkTCP,
				Address:           "127.0.0.1:1",
				ProcessIdentityV2: identity,
			})
			require.NoError(t, err)

			require.NoError(t, stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), time.Second))
			assert.NoFileExists(t, path, "an exited daemon must release its runtime record")
		})
	}
}
