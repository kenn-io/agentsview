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
