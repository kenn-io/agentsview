package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nonInteractive is the trailing option block buildSSHArgs appends
// to every invocation (see sshConnectTimeoutSecs). Kept here so the
// table below documents the full expected arg vector.
var nonInteractive = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
}

// wantSSHArgs builds an expected arg vector: the "ssh" program, any
// args emitted before the non-interactive defaults (port, ssh opts),
// the nonInteractive block, the "--" separator, the target, and the
// remote shell command. This keeps command quoting expectations
// explicit while removing the repeated default-option blocks.
func wantSSHArgs(beforeDefaults []string, target, remoteCmd string) []string {
	args := []string{"ssh"}
	args = append(args, beforeDefaults...)
	args = append(args, nonInteractive...)
	args = append(args, "--", target, remoteCmd)
	return args
}

func TestBuildSSHArgs(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		user    string
		port    int
		sshOpts []string
		cmd     string
		want    []string
	}{
		{
			name: "host only",
			host: "devbox1",
			cmd:  "echo hello",
			want: wantSSHArgs(nil, "devbox1", "sh -c 'echo hello'"),
		},
		{
			name: "host and user",
			host: "devbox1",
			user: "wes",
			cmd:  "ls -la",
			want: wantSSHArgs(nil, "wes@devbox1", "sh -c 'ls -la'"),
		},
		{
			name: "with port",
			host: "devbox1",
			user: "wes",
			port: 2222,
			cmd:  "echo hi",
			want: wantSSHArgs(
				[]string{"-p", "2222"},
				"wes@devbox1", "sh -c 'echo hi'",
			),
		},
		{
			name: "zero port ignored",
			host: "devbox1",
			cmd:  "echo hi",
			want: wantSSHArgs(nil, "devbox1", "sh -c 'echo hi'"),
		},
		{
			name: "with ssh opts before defaults",
			host: "devbox1",
			user: "wes",
			port: 2222,
			sshOpts: []string{
				"-i", "/tmp/key",
				"-o", "StrictHostKeyChecking=no",
			},
			cmd: "ls",
			want: wantSSHArgs(
				[]string{
					"-p", "2222",
					"-i", "/tmp/key",
					"-o", "StrictHostKeyChecking=no",
				},
				"wes@devbox1", "sh -c 'ls'",
			),
		},
		{
			name: "escapes single quotes",
			host: "devbox1",
			cmd:  `printf "it's fine"`,
			want: wantSSHArgs(
				nil, "devbox1", `sh -c 'printf "it'\''s fine"'`,
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildSSHArgs(
				tt.host, tt.user, tt.port,
				tt.sshOpts, tt.cmd,
			)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestBuildSSHArgs_NonInteractiveDefaults locks the security intent
// independent of arg ordering: remote sync never prompts (BatchMode)
// and bounds the connect phase (ConnectTimeout).
func TestBuildSSHArgs_NonInteractiveDefaults(t *testing.T) {
	got, err := buildSSHArgs("devbox1", "", 0, nil, "true")
	assert.NoError(t, err)
	assert.Subset(t, got, nonInteractive,
		"ssh invocation must include non-interactive defaults")
}

func TestBuildSSHArgsRejectsOptionShapedTargetParts(t *testing.T) {
	tests := []struct {
		name string
		host string
		user string
	}{
		{name: "host", host: "-oProxyCommand=sh"},
		{name: "user", host: "devbox1", user: "-lroot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildSSHArgs(tt.host, tt.user, 0, nil, "true")
			assert.Error(t, err)
			assert.Nil(t, got)
		})
	}
}
