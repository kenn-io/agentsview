package duckdb

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAttachTimeout(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"zero selects default", 0, DefaultAttachTimeout},
		{"negative disables", -1, 0},
		{"negative duration disables", -5 * time.Second, 0},
		{"positive used as-is", 3 * time.Second, 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveAttachTimeout(tt.configured))
		})
	}
}

func TestQuackDialAddress(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
		wantOK bool
	}{
		{"native host:port", "quack:127.0.0.1:9494", "127.0.0.1:9494", true},
		{"native localhost", "quack:localhost:8765", "localhost:8765", true},
		{
			"native with userinfo",
			"quack:user:pass@192.0.2.5:9494",
			"192.0.2.5:9494",
			true,
		},
		{
			"native with query and fragment",
			"quack:127.0.0.1:9494?foo=bar#frag",
			"127.0.0.1:9494",
			true,
		},
		{"native ipv6", "quack:[::1]:9494", "[::1]:9494", true},
		{"native missing port", "quack:example.com", "", false},
		{
			"http default port",
			"quack:http://example.com/db",
			"example.com:80",
			true,
		},
		{
			"https default port",
			"quack:https://example.com/db",
			"example.com:443",
			true,
		},
		{
			"http explicit port",
			"quack:http://example.com:9000/db",
			"example.com:9000",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := quackDialAddress(tt.rawURL)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// closedLoopbackPort returns a loopback host:port that nothing is listening on
// by opening and immediately closing a listener.
func closedLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "allocate probe listener")
	addr := ln.Addr().String()
	require.NoError(t, ln.Close(), "close probe listener")
	return addr
}

// unresponsiveListener accepts connections and holds them open without ever
// writing a byte, simulating a server stuck in (for example) an SSL handshake.
func unresponsiveListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "start unresponsive listener")
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			// Never respond; just keep the connection open.
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln
}

func TestPreflightQuackDial(t *testing.T) {
	t.Run("closed port fails fast", func(t *testing.T) {
		addr := closedLoopbackPort(t)
		start := time.Now()
		err := preflightQuackDial("quack:"+addr, time.Second)
		require.Error(t, err)
		assert.Less(t, time.Since(start), time.Second)
		assert.Contains(t, err.Error(), "connecting to quack endpoint")
	})

	t.Run("listening port passes", func(t *testing.T) {
		ln := unresponsiveListener(t)
		err := preflightQuackDial("quack:"+ln.Addr().String(), time.Second)
		assert.NoError(t, err)
	})

	t.Run("undeterminable address skips check", func(t *testing.T) {
		// No port: no dial target can be derived, so preflight is a no-op.
		assert.NoError(t, preflightQuackDial("quack:example.com", time.Second))
	})
}

func TestRunWithAttachTimeout(t *testing.T) {
	t.Run("times out on unresponsive server", func(t *testing.T) {
		ln := unresponsiveListener(t)
		timeout := 150 * time.Millisecond
		start := time.Now()
		err := runWithAttachTimeout(
			"quack:"+ln.Addr().String(), timeout, func() error {
				// Faithfully reproduce the hang: connect (accepted) then
				// block on a read that never returns because the server
				// never responds.
				conn, dialErr := net.Dial("tcp", ln.Addr().String())
				if dialErr != nil {
					return dialErr
				}
				defer conn.Close()
				buf := make([]byte, 1)
				_, readErr := conn.Read(buf)
				return readErr
			},
		)
		elapsed := time.Since(start)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Contains(t, err.Error(), "attach_timeout")
		assert.Less(t, elapsed, time.Second, "should give up near the timeout")
		assert.GreaterOrEqual(t, elapsed, timeout)
	})

	t.Run("returns attach result when it completes", func(t *testing.T) {
		assert.NoError(t, runWithAttachTimeout("quack:host", time.Second, func() error {
			return nil
		}))
		sentinel := errors.New("attach boom")
		err := runWithAttachTimeout("quack:host", time.Second, func() error {
			return sentinel
		})
		assert.ErrorIs(t, err, sentinel)
	})

	t.Run("disabled timeout runs inline", func(t *testing.T) {
		sentinel := errors.New("inline boom")
		err := runWithAttachTimeout("quack:host", 0, func() error {
			return sentinel
		})
		assert.ErrorIs(t, err, sentinel)
	})
}

func TestOpenQuackClientPreflightFailsFast(t *testing.T) {
	// A closed loopback port makes the TCP preflight fail before the quack
	// extension is even loaded, so an unreachable endpoint returns promptly
	// instead of hanging.
	addr := closedLoopbackPort(t)
	start := time.Now()
	client, err := openQuackClient(
		"quack:"+addr, "token", false, 2*time.Second,
	)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Contains(t, err.Error(), "connecting to quack endpoint")
}
