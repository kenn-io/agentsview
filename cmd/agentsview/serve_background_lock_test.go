//go:build !windows

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyBackgroundLaunchLockResultPreservesNonWindowsErrors(t *testing.T) {
	wantErr := errors.New("lock I/O failed")
	locked, err := classifyBackgroundLaunchLockResult(false, wantErr)

	assert.False(t, locked)
	require.ErrorIs(t, err, wantErr)
}
