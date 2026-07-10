//go:build windows

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyBackgroundLaunchLockResultTreatsWindowsErrorsAsContention(t *testing.T) {
	locked, err := classifyBackgroundLaunchLockResult(
		false, errors.New("helper-owned lock probe failed"),
	)

	assert.False(t, locked)
	require.NoError(t, err)
}
