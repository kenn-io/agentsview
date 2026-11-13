package server

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/testutil"
)

func TestFindAvailablePortWildcardZeroRetriesCrossFamilyCollision(t *testing.T) {
	testutil.RequireWildcardListen(t)
	occupiedListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "0.0.0.0:0")
	require.NoError(t, err, "bind IPv4 wildcard")
	defer occupiedListener.Close()
	occupied := occupiedListener.Addr().(*net.TCPAddr).Port

	second := 0
	for range 100 {
		candidate4, listenErr := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "0.0.0.0:0")
		require.NoError(t, listenErr, "select second IPv4 port")
		candidate := candidate4.Addr().(*net.TCPAddr).Port
		candidate6, listenErr := net.ListenTCP("tcp6", &net.TCPAddr{
			IP:   net.IPv6unspecified,
			Port: candidate,
		})
		if listenErr != nil {
			require.NoError(t, candidate4.Close())
			continue
		}
		require.NoError(t, candidate6.Close())
		require.NoError(t, candidate4.Close())
		second = candidate
		break
	}
	if second == 0 {
		t.Skip("IPv6 wildcard binding unavailable")
	}

	selections := 0
	got, err := findAvailablePort(t.Context(),
		"0.0.0.0",
		0,
		func(context.Context, string) (int, error) {
			selections++
			if selections == 1 {
				return occupied, nil
			}
			return second, nil
		},
		listenProbesFree,
	)
	require.NoError(t, err)
	require.Equal(t, second, got,
		"wildcard ephemeral selection must retry a cross-family collision")
	require.Equal(t, 2, selections)
}

func TestListenProbesRejectOccupiedPort(t *testing.T) {
	// Hold the socket throughout the probe so the port stays occupied.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		require.FailNowf(t, "test failed", "listen: %v", err)
	}
	defer ln.Close()

	occupied := ln.Addr().(*net.TCPAddr).Port

	require.False(t, listenProbesFree(t.Context(), listenProbes(t.Context(), "127.0.0.1"), occupied))
}

func TestListenProbesWildcardRejectOccupiedPort(t *testing.T) {
	// A dual-stack wildcard listen can succeed on IPv6 while an unrelated
	// process still owns the port on IPv4 (observed on macOS), so wildcard
	// availability must check the IPv4 wildcard address on its own.
	testutil.RequireWildcardListen(t)
	for _, tt := range []struct {
		name    string
		network string
		address string
	}{
		{name: "IPv4", network: "tcp4", address: "0.0.0.0:0"},
		{name: "IPv6", network: "tcp6", address: "[::]:0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ln, err := (&net.ListenConfig{}).Listen(t.Context(), tt.network, tt.address)
			if err != nil && tt.network == "tcp6" {
				t.Skipf("IPv6 unavailable: %v", err)
			}
			require.NoError(t, err, "bind %s wildcard", tt.name)
			defer ln.Close()

			occupied := ln.Addr().(*net.TCPAddr).Port

			require.False(t, listenProbesFree(t.Context(), listenProbes(t.Context(), "0.0.0.0"), occupied),
				"wildcard port selection must reject an %s-occupied port", tt.name)
		})
	}
}

// Scan behavior uses controlled availability so it does not depend on the OS
// leaving any of the 99 ports after an ephemeral listener available. Real bind
// rejection is covered separately by the listen probe tests above.
func TestFindAvailablePortSkipsOccupied(t *testing.T) {
	var probed []int
	got, err := findAvailablePort(t.Context(), "127.0.0.1", 49687,
		selectEphemeralPort,
		func(_ context.Context, probes []listenProbe, port int) bool {
			require.Equal(t, []listenProbe{{network: "tcp", host: "127.0.0.1"}}, probes)
			probed = append(probed, port)
			return port == 49689
		},
	)
	require.NoError(t, err)
	require.Equal(t, 49689, got)
	require.Equal(t, []int{49687, 49688, 49689}, probed)
}
