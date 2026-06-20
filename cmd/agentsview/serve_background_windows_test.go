//go:build windows

package main

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDetachedProcess = 0x00000008

func TestConfigureServeBackgroundCommandUsesHiddenConsole(t *testing.T) {
	cmd := exec.Command("agentsview", "serve", "--background")

	configureServeBackgroundCommand(cmd)

	require.NotNil(t, cmd.SysProcAttr)
	flags := cmd.SysProcAttr.CreationFlags
	assert.NotZero(t, flags&createNoWindow)
	assert.NotZero(t, flags&createNewProcessGroup)
	assert.Zero(t, flags&testDetachedProcess)
}
