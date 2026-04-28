package ssh

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

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
			want: []string{
				"ssh", "devbox1", "--", "sh -c 'echo hello'",
			},
		},
		{
			name: "host and user",
			host: "devbox1",
			user: "wes",
			cmd:  "ls -la",
			want: []string{
				"ssh", "wes@devbox1", "--", "sh -c 'ls -la'",
			},
		},
		{
			name: "with port",
			host: "devbox1",
			user: "wes",
			port: 2222,
			cmd:  "echo hi",
			want: []string{
				"ssh", "-p", "2222",
				"wes@devbox1", "--", "sh -c 'echo hi'",
			},
		},
		{
			name: "zero port ignored",
			host: "devbox1",
			cmd:  "echo hi",
			want: []string{
				"ssh", "devbox1", "--", "sh -c 'echo hi'",
			},
		},
		{
			name: "with ssh opts",
			host: "devbox1",
			user: "wes",
			port: 2222,
			sshOpts: []string{
				"-i", "/tmp/key",
				"-o", "StrictHostKeyChecking=no",
			},
			cmd: "ls",
			want: []string{
				"ssh", "-p", "2222",
				"-i", "/tmp/key",
				"-o", "StrictHostKeyChecking=no",
				"wes@devbox1", "--", "sh -c 'ls'",
			},
		},
		{
			name: "escapes single quotes",
			host: "devbox1",
			cmd:  `printf "it's fine"`,
			want: []string{
				"ssh", "devbox1", "--",
				`sh -c 'printf "it'\''s fine"'`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSSHArgs(
				tt.host, tt.user, tt.port,
				tt.sshOpts, tt.cmd,
			)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
