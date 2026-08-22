//go:build windows

package capture

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRejectsWindowsBatchProducerAndPersistsFailure(t *testing.T) {
	providerRoot := t.TempDir()
	secureTestProviderRoot(providerRoot)
	workDir := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "usage.json")
	markerPath := filepath.Join(t.TempDir(), "executed")
	shimPath := filepath.Join(t.TempDir(), "claude.cmd")
	require.NoError(t, os.WriteFile(
		shimPath, []byte("@echo executed>\""+markerPath+"\"\r\n"), 0o600,
	))

	outcome, err := Run(t.Context(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "windows-batch-rejected",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: providerRoot, WorkDir: workDir, ClaudeWorkDir: workDir,
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
		Command:           []string{shimPath},
		Streams:           Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:            testLimits(),
	})

	require.ErrorContains(t, err, "batch shim")
	assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
	assert.NoFileExists(t, markerPath)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonChildStartFailed, result.Reporting.Reason)
	assert.Nil(t, result.Execution.ExitCode)
}
