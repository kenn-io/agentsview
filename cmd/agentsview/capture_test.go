package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/capture"
)

func TestCaptureRunUsesReportingFailureExitCodeBeforeProducerStarts(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	cmd := newCaptureRunCommand()
	cmd.SetArgs([]string{
		"--provider", "claude",
		"--occurrence", "ci-occurrence",
		"--capture-dir", filepath.Join(t.TempDir(), "capture"),
		"--result", filepath.Join(t.TempDir(), "result.json"),
		"--", "unused-producer",
	})

	_, err := cmd.ExecuteC()
	require.Error(t, err)
	assert.Equal(t, capture.ReportFailureExitCode, exitCodeFromError(err))
}
