//go:build !windows

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigureServeBackgroundCommandUnixStartsNewSession(t *testing.T) {
	attr := requireConfiguredServeBackgroundSysProcAttr(t)
	assert.True(t, attr.Setsid)
}
