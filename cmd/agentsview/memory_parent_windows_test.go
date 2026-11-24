//go:build windows

package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchProcessReadsWindowsEnvironment(t *testing.T) {
	t.Setenv("AGENTSVIEW_LAUNCH_PROCESS_TEST", "present")
	process, err := lookupGopsutilProcess(t.Context(), int32(os.Getpid()))
	require.NoError(t, err)
	env, err := processEnv(t.Context(), process)
	require.NoError(t, err)
	assert.Equal(t, "present", env["AGENTSVIEW_LAUNCH_PROCESS_TEST"])
}
