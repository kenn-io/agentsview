package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeMCPHTTPAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		addr          string
		allowInsecure bool
		want          string
		wantErr       bool
	}{
		{"empty", "", false, "", true},
		{"bare port", "8085", false, "127.0.0.1:8085", false},
		{"colon port", ":8085", false, "127.0.0.1:8085", false},
		{"explicit loopback v4", "127.0.0.1:8085", false, "127.0.0.1:8085", false},
		{"explicit loopback v6", "[::1]:8085", false, "[::1]:8085", false},
		{"localhost", "localhost:8085", false, "localhost:8085", false},
		{"non-loopback rejected", "192.168.1.5:8085", false, "", true},
		{"all-interfaces rejected", "0.0.0.0:8085", false, "", true},
		{"non-loopback opted in", "192.168.1.5:8085", true, "192.168.1.5:8085", false},
		{"all-interfaces opted in", "0.0.0.0:8085", true, "0.0.0.0:8085", false},
		{"not a port", "notaport", false, "", true},
		// Empty host with a port still binds all interfaces, so it must be
		// rejected without the opt-in.
		{"empty host footgun", "[]:8085", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeMCPHTTPAddr(tc.addr, tc.allowInsecure)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMCPListenerAuth(t *testing.T) {
	t.Parallel()
	// Loopback without require_auth is local-trust: no listener auth, even
	// when a token happens to be configured.
	tok, err := mcpListenerAuth("127.0.0.1:8085", "", false)
	require.NoError(t, err)
	assert.Empty(t, tok)
	tok, err = mcpListenerAuth("[::1]:8085", "abc", false)
	require.NoError(t, err)
	assert.Empty(t, tok, "loopback bind does not enforce a token without require_auth")

	// require_auth forces auth even on loopback, so a forwarded port is
	// never an unauthenticated surface.
	tok, err = mcpListenerAuth("127.0.0.1:8085", "abc", true)
	require.NoError(t, err)
	assert.Equal(t, "abc", tok, "require_auth enforces the token on loopback")

	// require_auth on loopback without a token is refused.
	_, err = mcpListenerAuth("127.0.0.1:8085", "", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth token")

	// Non-loopback with a token enforces it.
	tok, err = mcpListenerAuth("192.168.1.5:8085", "abc", false)
	require.NoError(t, err)
	assert.Equal(t, "abc", tok)

	// Non-loopback without a token is refused (no unauthenticated remote surface).
	_, err = mcpListenerAuth("192.168.1.5:8085", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth token")
}

func TestNewMCPCommand_Wiring(t *testing.T) {
	t.Parallel()
	cmd := newMCPCommand()
	assert.Equal(t, "mcp", cmd.Use)
	assert.Equal(t, groupData, cmd.GroupID)
	assert.True(t, cmd.SilenceUsage)

	for _, name := range []string{
		"http", "http-allow-insecure", "server", "server-token-file", "pg",
	} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "missing flag --%s", name)
	}
}

func TestRootCommand_RegistersMCP(t *testing.T) {
	t.Parallel()
	root := newRootCommand()
	var found bool
	for _, c := range root.Commands() {
		if c.Use == "mcp" {
			found = true
			break
		}
	}
	assert.True(t, found, "root command should register the mcp subcommand")
}
