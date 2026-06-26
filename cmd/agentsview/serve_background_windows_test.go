//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const testDetachedProcess = 0x00000008

func TestConfigureServeBackgroundCommandWindowsHidesConsole(t *testing.T) {
	attr := requireConfiguredServeBackgroundSysProcAttr(t)
	assertWindowsCreationFlags(
		t,
		attr.CreationFlags,
		createNoWindow|createNewProcessGroup,
		testDetachedProcess,
	)
}

// assertWindowsCreationFlags checks that every bit in wantSet is present in
// flags and that no bit in wantClear is set.
func assertWindowsCreationFlags(t *testing.T, flags, wantSet, wantClear uint32) {
	t.Helper()
	assert.Equal(t, wantSet, flags&wantSet)
	assert.Zero(t, flags&wantClear)
}
