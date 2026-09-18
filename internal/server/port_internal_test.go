package server

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindAvailablePortWildcardZeroRetriesCrossFamilyCollision(t *testing.T) {
	require := require.New(t)

	occupiedListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "0.0.0.0:0")
	require.NoError(err, "bind IPv4 wildcard")
	defer occupiedListener.Close()
	occupied := occupiedListener.Addr().(*net.TCPAddr).Port

	second := 0
	for range 100 {
		candidate4, listenErr := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "0.0.0.0:0")
		require.NoError(listenErr, "select second IPv4 port")
		candidate := candidate4.Addr().(*net.TCPAddr).Port
		candidate6, listenErr := net.ListenTCP("tcp6", &net.TCPAddr{
			IP:   net.IPv6unspecified,
			Port: candidate,
		})
		if listenErr != nil {
			require.NoError(candidate4.Close())
			continue
		}
		require.NoError(candidate6.Close())
		require.NoError(candidate4.Close())
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
	require.NoError(err)
	require.Equal(second, got,
		"wildcard ephemeral selection must retry a cross-family collision")
	require.Equal(2, selections)
}
