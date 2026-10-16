package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/sshremote"
)

func TestRemoteSyncProbePrintsVersionLine(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := executeCLIWithLegacyFlagCompat([]string{"remote-sync", "probe"}, &stdout, &stderr)

	require.NoError(t, err)
	var probe sshremote.ProbeResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &probe))
	assert.Equal(t, sshremote.ProbeResult{
		Version: version, ProtocolVersion: remotesync.ProtocolVersion,
	}, probe)
	assert.True(t, bytes.HasSuffix(stdout.Bytes(), []byte("\n")), "probe prints one line")
	assert.Empty(t, stderr.String())
}

func TestRemoteSyncServeRejectsUnknownOp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := executeCLIWithLegacyFlagCompat(
		[]string{"remote-sync", "serve", "--op", "delete"}, &stdout, &stderr)

	require.ErrorContains(t, err, `unknown remote-sync operation "delete"`)
	assert.Empty(t, stdout.String())
}

func TestRemoteSyncIsHiddenFromRootHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	require.NoError(t, executeCLIWithLegacyFlagCompat([]string{"--help"}, &stdout, &stderr))

	assert.NotContains(t, stdout.String(), "remote-sync")
}

// remoteSyncServeHelperEnv makes the test binary run the CLI with the
// arguments after "--" and exit as main does. Serve reads os.Stdin and owns
// fd 1, so only a separate process can observe its streams and exit status.
const remoteSyncServeHelperEnv = "AGENTSVIEW_REMOTE_SYNC_SERVE_HELPER"

func TestRemoteSyncServeHelperProcess(t *testing.T) {
	if os.Getenv(remoteSyncServeHelperEnv) != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	err := executeCLIWithLegacyFlagCompat(args, os.Stdout, os.Stderr)
	if err == nil {
		os.Exit(0)
	}
	os.Exit(reportCLIError(err, os.Stderr))
}

func TestRemoteSyncServeReportsFailureAsOneJSONLine(t *testing.T) {
	tests := []struct {
		name     string
		stdin    string
		wantCode sshremote.ErrorCode
	}{
		{name: "invalid request", stdin: "not json", wantCode: sshremote.CodeInternal},
		{
			name:     "wrong protocol version",
			stdin:    `{"protocol_version":0}`,
			wantCode: sshremote.CodeIncompatibleProtocol,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			dataDir := filepath.Join(home, ".agentsview")
			cmd := exec.CommandContext(t.Context(), os.Args[0],
				"-test.run=^TestRemoteSyncServeHelperProcess$", "--",
				"remote-sync", "serve", "--op", "targets")
			cmd.Env = append(os.Environ(),
				remoteSyncServeHelperEnv+"=1",
				"HOME="+home,
				"AGENTSVIEW_DATA_DIR="+dataDir,
			)
			cmd.Stdin = strings.NewReader(tt.stdin)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			err := cmd.Run()

			exitErr, ok := errors.AsType[*exec.ExitError](err)
			require.True(t, ok, "serve exited without a status: %v", err)
			assert.Equal(t, sshremote.ServeExitCode, exitErr.ExitCode())
			assert.Empty(t, stdout.String())
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			require.Len(t, lines, 1, "stderr: %q", stderr.String())
			var serveErr sshremote.ServeError
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &serveErr))
			assert.Equal(t, tt.wantCode, serveErr.Code)
			assert.NotEmpty(t, serveErr.Message)
			assert.NoDirExists(t, dataDir, "serve creates no data directory")
		})
	}
}
