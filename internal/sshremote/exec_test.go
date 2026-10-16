//go:build unix

package sshremote

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/remotesync"
)

func TestRunnerClassifiesSSHFailures(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		exit        int
		wantOffline bool
		wantSummary string
	}{
		{
			name: "dns",
			stderr: "ssh: Could not resolve hostname devbox.example.test: " +
				"Name or service not known\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name:        "refused",
			stderr:      "ssh: connect to host 192.0.2.10 port 22: Connection refused\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name:        "timed out",
			stderr:      "ssh: connect to host 192.0.2.10 port 22: Connection timed out\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name:        "macos timeout",
			stderr:      "ssh: connect to host 192.0.2.10 port 22: Operation timed out\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name:        "no route",
			stderr:      "ssh: connect to host 192.0.2.10 port 22: No route to host\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name:        "unreachable",
			stderr:      "ssh: connect to host 192.0.2.10 port 22: Network is unreachable\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name: "banner",
			stderr: "Connection timed out during banner exchange\n" +
				"Connection to UNKNOWN port 65535 timed out\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name: "jump host refused",
			stderr: "ssh: connect to host jump.example.test port 22: Connection refused\n" +
				"Connection closed by UNKNOWN port 65535\n",
			exit:        255,
			wantOffline: true,
		},
		{
			name:        "bad port config",
			stderr:      "command-line line 0: Bad port 'notaport'.\n",
			exit:        255,
			wantSummary: "command-line line 0: Bad port 'notaport'.",
		},
		{
			name:        "auth",
			stderr:      "user@devbox.example.test: Permission denied (publickey).\n",
			exit:        255,
			wantSummary: "user@devbox.example.test: Permission denied (publickey).",
		},
		{
			name: "auth after an sshd banner",
			stderr: "Authorized use only.\nAll access is logged.\n\n" +
				"user@devbox.example.test: Permission denied (publickey).\n",
			exit:        255,
			wantSummary: "user@devbox.example.test: Permission denied (publickey).",
		},
		{
			name:        "host key",
			stderr:      "Host key verification failed.\n",
			exit:        255,
			wantSummary: "Host key verification failed.",
		},
		{
			// A session that dies after it started may have moved data, so the
			// user sees it as a failure instead of a silent skip.
			name:        "dead connection after keepalive",
			stderr:      "Timeout, server devbox.example.test not responding.\n",
			exit:        255,
			wantSummary: "Timeout, server devbox.example.test not responding.",
		},
		{
			// The jump leg's teardown lines follow the real error.
			name: "target behind jump",
			stderr: "channel 0: open failed: connect failed: Connection refused\n" +
				"stdio forwarding failed\nConnection closed by UNKNOWN port 65535\n",
			exit:        255,
			wantSummary: "channel 0: open failed: connect failed: Connection refused",
		},
		{
			name: "auth failure on jump host",
			stderr: "user@jump.example.test: Permission denied (publickey).\n" +
				"Connection closed by UNKNOWN port 65535\n",
			exit:        255,
			wantSummary: "user@jump.example.test: Permission denied (publickey).",
		},
		{
			name: "jump host drops the connection",
			stderr: "kex_exchange_identification: read: Connection reset by peer\n" +
				"kex_exchange_identification: Connection closed by remote host\n" +
				"Connection closed by UNKNOWN port 65535\n",
			exit:        255,
			wantSummary: "kex_exchange_identification: read: Connection reset by peer",
		},
		{
			// With nothing but teardown lines, the last one is all there is.
			name: "only teardown lines",
			stderr: "kex_exchange_identification: Connection closed by remote host\n" +
				"Connection closed by 192.0.2.10 port 22\n",
			exit:        255,
			wantSummary: "Connection closed by 192.0.2.10 port 22",
		},
		{
			name:        "offline line but other exit",
			stderr:      "ssh: connect to host 192.0.2.10 port 22: Connection refused\n",
			exit:        1,
			wantSummary: "ssh: connect to host 192.0.2.10 port 22: Connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := Runner{Destination: "devbox", SSHPath: newFailingSSH(t, tt.stderr, tt.exit)}

			_, err := runner.Output(t.Context(), "true", nil)

			cmdErr, ok := errors.AsType[*CommandError](err)
			require.True(t, ok, "error %v is not a CommandError", err)
			assert.Equal(t, tt.exit, cmdErr.ExitCode)
			assert.Equal(t, tt.wantOffline, cmdErr.Offline)
			assert.Equal(t, tt.wantOffline, remotesync.IsHostUnavailable(err))
			if tt.wantOffline {
				assert.Equal(t, "SSH remote sync failed: host is unreachable",
					remotesync.FailureSummary(err))
				return
			}
			assert.Equal(t, "SSH remote sync failed: "+tt.wantSummary,
				remotesync.FailureSummary(err))
		})
	}
}

func TestRunnerSummaryDropsTerminalControlText(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		exit   int
		want   string
	}{
		{
			name:   "ssh stderr",
			stderr: "\\033[31mPermission\\033[0m denied\\r\\033]0;title\\007 by\\0000 host\\tnow\n",
			exit:   255,
			want:   "SSH remote sync failed: Permission denied by host now",
		},
		{
			name: "serve error",
			stderr: `{"code":"internal\\u001b[2J","message":` +
				`"disk\\r full\\u0007 on \\u001b]8;;http://example.test\\u0007host"}\n`,
			exit: ServeExitCode,
			want: "SSH remote sync failed: remote agentsview reported internal: disk full on host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := Runner{Destination: "devbox", SSHPath: newFailingSSH(t, tt.stderr, tt.exit)}

			_, err := runner.Output(t.Context(), "true", nil)

			require.Error(t, err)
			assert.Equal(t, tt.want, remotesync.FailureSummary(err))
		})
	}
}

func TestRunnerFindsServeErrorAfterLongStartupOutput(t *testing.T) {
	const notice = "remote start-up notice\n"
	noise := strings.Repeat(notice, 2*maxCapturedStderr/len(notice))
	ssh := newFailingSSH(t, noise+`{"code":"internal","message":"disk full"}`+"\n", ServeExitCode)
	runner := Runner{Destination: "devbox", SSHPath: ssh}

	_, err := runner.Output(t.Context(), "true", nil)

	cmdErr, ok := errors.AsType[*CommandError](err)
	require.True(t, ok)
	require.NotNil(t, cmdErr.Remote)
	assert.Equal(t, ServeError{Code: CodeInternal, Message: "disk full"}, *cmdErr.Remote)
	assert.LessOrEqual(t, len(cmdErr.Stderr), maxCapturedStderr)
}

func TestRunnerReportsRemoteServeError(t *testing.T) {
	tests := []struct {
		code        ErrorCode
		wantRefused bool
	}{
		{code: CodeForbidden, wantRefused: true},
		{code: CodeInternal, wantRefused: false},
		{code: CodeUnsupported, wantRefused: false},
		{code: CodeIncompatibleProtocol, wantRefused: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			stderr := `{"code":"` + string(tt.code) +
				`","message":"remote sync target is not allowed"}\n`
			runner := Runner{
				Destination: "devbox", SSHPath: newFailingSSH(t, stderr, ServeExitCode),
			}

			_, err := runner.Output(t.Context(), "true", strings.NewReader("{}"))

			cmdErr, ok := errors.AsType[*CommandError](err)
			require.True(t, ok)
			require.NotNil(t, cmdErr.Remote)
			assert.Equal(t, tt.code, cmdErr.Remote.Code)
			assert.Equal(t, tt.wantRefused, cmdErr.RemoteRefused())
			assert.False(t, remotesync.IsHostUnavailable(err))
		})
	}
}

func TestRunnerPassesSSHOptions(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_SSH_ARGS", argsFile)
	sshPath := writeExecutable(t, t.TempDir(), "ssh",
		"#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \"$FAKE_SSH_ARGS\"; done\n")
	runner := Runner{Destination: "devbox.example.test", SSHPath: sshPath}

	_, err := runner.Output(t.Context(), "true", nil)

	require.NoError(t, err)
	got, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "Compression=no",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
		"--", "devbox.example.test", "sh -c 'true'",
	}, strings.Split(strings.TrimSuffix(string(got), "\n"), "\n"))
}

func TestRunnerPassesStdinAndReturnsStdout(t *testing.T) {
	ssh := newFakeSSH(t)
	runner := Runner{Destination: "devbox", SSHPath: ssh.Path}

	out, err := runner.Output(t.Context(), "cat", strings.NewReader("hello"))

	require.NoError(t, err)
	assert.Equal(t, "hello", string(out))
}

func TestStreamCloseBeforeEOFStopsSSH(t *testing.T) {
	ssh := newFakeSSH(t)
	runner := Runner{Destination: "devbox", SSHPath: ssh.Path}
	// yes writes far more than a pipe buffer holds, so the remote blocks
	// writing once the caller stops reading.
	stream, err := runner.Stream(t.Context(), "yes", nil)
	require.NoError(t, err)
	_, err = io.ReadFull(stream, make([]byte, 4))
	require.NoError(t, err)

	closed := make(chan error, 1)
	go func() { closed <- stream.Close() }()
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-time.After(processHangGuard):
		require.FailNow(t, "Close blocked on an ssh process nobody reads")
	}

	require.ErrorIs(t, closeErr, errStreamAbandoned)
	assert.False(t, remotesync.IsHostUnavailable(closeErr))
	assert.Equal(t, closeErr, stream.Close(), "a second Close returns the same error")
}

func TestValidateScriptRejectsShellHazards(t *testing.T) {
	for _, script := range []string{"echo 'x'", "echo a\necho b", "echo hi!", `echo a\b`} {
		require.Error(t, validateScript(script), script)
	}
	assert.NoError(t, validateScript(`echo "os=$(uname -s)"`))
}
