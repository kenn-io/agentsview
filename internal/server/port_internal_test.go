package server

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindAvailablePortWildcardZeroRetriesCrossFamilyCollision(t *testing.T) {
	occupiedListener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err, "bind IPv4 wildcard")
	defer occupiedListener.Close()
	occupied := occupiedListener.Addr().(*net.TCPAddr).Port

	second := 0
	for range 100 {
		candidate4, listenErr := net.Listen("tcp4", "0.0.0.0:0")
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
	got, err := findAvailablePort(
		"0.0.0.0",
		0,
		func(string) (int, error) {
			selections++
			if selections == 1 {
				return occupied, nil
			}
			return second, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, second, got,
		"wildcard ephemeral selection must retry a cross-family collision")
	require.Equal(t, 2, selections)
}
