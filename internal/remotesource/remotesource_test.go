package remotesource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/sshremote"
)

func TestFor(t *testing.T) {
	tests := []struct {
		name    string
		host    config.RemoteHost
		want    remotesync.Source
		wantSSH bool
		wantErr string
	}{
		{
			name: "http with token",
			host: config.RemoteHost{
				Host: "devbox", URL: "https://devbox.example.test", Token: "secret",
			},
			want: remotesync.HTTPSource{URL: "https://devbox.example.test", Token: "secret"},
		},
		{
			name: "explicit http with token",
			host: config.RemoteHost{
				Host: "devbox", Transport: config.RemoteTransportHTTP,
				URL: "https://devbox.example.test", Token: "secret",
			},
			want: remotesync.HTTPSource{URL: "https://devbox.example.test", Token: "secret"},
		},
		{
			name:    "http without token",
			host:    config.RemoteHost{Host: "devbox", URL: "https://devbox.example.test"},
			wantErr: "http remote sync token is required",
		},
		{
			name:    "ssh needs no token",
			host:    config.RemoteHost{Host: "devbox", Transport: config.RemoteTransportSSH},
			wantSSH: true,
		},
		{
			name:    "unknown transport",
			host:    config.RemoteHost{Host: "devbox", Transport: "sftp"},
			wantErr: `invalid remote transport "sftp"; use http or ssh`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := For(tt.host, Options{Version: "dev", DataDir: t.TempDir()})

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, source)
				return
			}
			require.NoError(t, err)
			if tt.wantSSH {
				assert.IsType(t, &sshremote.Source{}, source)
				return
			}
			assert.Equal(t, tt.want, source)
		})
	}
}
