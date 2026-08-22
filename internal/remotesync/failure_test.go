package remotesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

func TestFailureSummary(t *testing.T) {
	dialRefused := fmt.Errorf(
		"Get %q: %w",
		"http://devbox1.tailnet.ts.net:8080/api/v1/remote-sync/targets",
		&net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{
				Syscall: "connect",
				Err:     syscall.ECONNREFUSED,
			},
		},
	)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "HTTP remote sync failed",
		},
		{
			name: "pending cleanup takes precedence over retained status",
			err: &PendingCleanupError{Err: &StatusError{
				Code: 403, Detail: "private retained response",
			}},
			want: "HTTP remote sync blocked: cleanup from an earlier sync " +
				"still owns resources",
		},
		{
			name: "unauthorized status",
			err: fmt.Errorf("fetch targets: %w", &StatusError{
				Code:   401,
				Status: "401 Unauthorized",
				Detail: "invalid bearer token abc123",
			}),
			want: "HTTP remote sync failed: remote daemon rejected " +
				"the sync token (401 Unauthorized); the token for " +
				"this host in [[remote_hosts]] must match the remote " +
				"daemon's auth_token",
		},
		{
			name: "forbidden status",
			err: &StatusError{
				Code: 403, Status: "403 Forbidden",
			},
			want: "HTTP remote sync failed: remote daemon rejected " +
				"the sync token (403 Forbidden); the token for " +
				"this host in [[remote_hosts]] must match the remote " +
				"daemon's auth_token",
		},
		{
			name: "not found status",
			err: &StatusError{
				Code: 404, Status: "404 Not Found",
			},
			want: "HTTP remote sync failed: remote daemon has no " +
				"remote-sync endpoints (404 Not Found); upgrade " +
				"agentsview on the remote host",
		},
		{
			name: "server error status",
			err: &StatusError{
				Code:   500,
				Status: "500 Internal Server Error",
			},
			want: "HTTP remote sync failed: remote daemon returned " +
				"500 Internal Server Error",
		},
		{
			name: "protocol upgrade required",
			err:  &StatusError{Code: http.StatusUpgradeRequired},
			want: "HTTP remote sync failed: collector and remote daemon use " +
				"incompatible remote-sync protocol versions; upgrade " +
				"agentsview on both hosts",
		},
		{
			name: "remote-controlled reason phrase is ignored",
			err: &StatusError{
				Code:   401,
				Status: "401 go-away token=abc123 leaked",
			},
			want: "HTTP remote sync failed: remote daemon rejected " +
				"the sync token (401 Unauthorized); the token for " +
				"this host in [[remote_hosts]] must match the remote " +
				"daemon's auth_token",
		},
		{
			name: "unknown status code renders numerically",
			err: &StatusError{
				Code:   599,
				Status: "599 Vendor Specific Nonsense",
			},
			want: "HTTP remote sync failed: remote daemon returned 599",
		},
		{
			name: "incompatible protocol",
			err:  &IncompatibleProtocolError{Got: "missing", Want: "1"},
			want: "HTTP remote sync failed: collector and remote daemon use " +
				"incompatible remote-sync protocol versions; upgrade " +
				"agentsview on both hosts",
		},
		{
			name: "connection refused",
			err:  dialRefused,
			want: "HTTP remote sync failed: connection refused; " +
				"check that the remote daemon is running and bound " +
				"to a reachable address (serve --host 0.0.0.0 or " +
				"host in its config.toml), and that the url port " +
				"matches",
		},
		{
			name: "dns failure",
			err: fmt.Errorf("Get \"http://nope:8080\": %w",
				&net.DNSError{
					Err: "no such host", Name: "nope",
				}),
			want: "HTTP remote sync failed: cannot resolve the " +
				"remote host name; check the url in this " +
				"[[remote_hosts]] entry",
		},
		{
			name: "network timeout",
			err: fmt.Errorf("Get \"http://devbox:8080\": %w",
				fakeTimeoutError{}),
			want: "HTTP remote sync failed: connection timed out; " +
				"check that the remote host is reachable and the " +
				"url is correct",
		},
		{
			name: "context deadline",
			err:  fmt.Errorf("fetch: %w", context.DeadlineExceeded),
			want: "HTTP remote sync failed: connection timed out; " +
				"check that the remote host is reachable and the " +
				"url is correct",
		},
		{
			name: "unknown error stays generic",
			err: errors.New(
				"Get \"http://stored.example\": bearer secret-token rejected",
			),
			want: "HTTP remote sync failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FailureSummary(tt.err)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "tailnet.ts.net",
				"summaries must not leak the remote URL")
			assert.NotContains(t, got, "abc123",
				"summaries must not leak response bodies")
			assert.NotContains(t, got, "secret-token",
				"summaries must not leak raw error text")
		})
	}
}

func TestIsHostUnavailable(t *testing.T) {
	wrappedNetworkError := func(op string, err error) error {
		return fmt.Errorf("fetch manifest: %w", &net.OpError{
			Op:  op,
			Net: "tcp",
			Err: &os.SyscallError{Syscall: op, Err: err},
		})
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "timeout", err: fakeTimeoutError{}, want: true},
		{name: "connection refused", err: syscall.ECONNREFUSED, want: true},
		{
			name: "wrapped connection reset",
			err:  wrappedNetworkError("read", syscall.ECONNRESET),
			want: true,
		},
		{
			name: "wrapped connection aborted",
			err:  wrappedNetworkError("read", syscall.ECONNABORTED),
			want: true,
		},
		{
			name: "wrapped broken pipe",
			err:  wrappedNetworkError("write", syscall.EPIPE),
			want: true,
		},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, want: true},
		{name: "network unreachable", err: syscall.ENETUNREACH, want: true},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "request canceled", err: context.Canceled, want: false},
		{
			name: "protocol decode EOF",
			err:  fmt.Errorf("decode remote manifest: %w", io.EOF),
			want: false,
		},
		{
			name: "protocol decode unexpected EOF",
			err:  fmt.Errorf("decode remote manifest: %w", io.ErrUnexpectedEOF),
			want: false,
		},
		{name: "timeout with retained cleanup", err: &cleanupRetryTestError{
			cause: syscall.ETIMEDOUT,
		}, want: false},
		{name: "bad token", err: &StatusError{Code: 401}, want: false},
		{name: "bad host name", err: &net.DNSError{Err: "no such host"}, want: false},
		{name: "import failure", err: errors.New("persist mirror"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsHostUnavailable(tt.err))
		})
	}
}

func TestIsHostUnavailableWhenHTTPServerClosesBeforeHeaders(t *testing.T) {
	serverResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter, _ *http.Request,
	) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			serverResult <- errors.New("response writer cannot hijack")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			serverResult <- err
			return
		}
		serverResult <- conn.Close()
	}))
	t.Cleanup(server.Close)

	_, err := (HTTPSync{URL: server.URL}).fetchTargets(
		context.Background(), server.Client(),
	)
	require.NoError(t, <-serverResult)
	require.Error(t, err)
	assert.True(t, IsHostUnavailable(err))
}

func TestIsHostUnavailableWhenHTTPHeadersAreTruncated(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })

	serverResult := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		_, writeErr := io.WriteString(
			conn, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n",
		)
		serverResult <- errors.Join(writeErr, conn.Close())
	}()

	_, err = (HTTPSync{URL: "http://" + listener.Addr().String()}).fetchTargets(
		t.Context(), http.DefaultClient,
	)
	require.NoError(t, <-serverResult)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.True(t, IsHostUnavailable(err))
}

func TestIsHostUnavailableWhenHTTPArchiveBodyIsTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter, _ *http.Request,
	) {
		SetProtocolHeader(writer.Header())
		writer.Header().Set("Content-Length", "20")
		_, _ = writer.Write([]byte("short"))
	}))
	t.Cleanup(server.Close)

	root, err := (HTTPSync{URL: server.URL}).downloadAndExtract(
		t.Context(), server.Client(), TargetSet{},
	)
	assert.Empty(t, root)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.True(t, IsHostUnavailable(err))
}

func TestIsHostUnavailableWhenHTTPJSONBodyIsTruncated(t *testing.T) {
	tests := []struct {
		name  string
		fetch func(context.Context, HTTPSync, *http.Client) error
	}{
		{
			name: "targets",
			fetch: func(ctx context.Context, hs HTTPSync, client *http.Client) error {
				_, err := hs.fetchTargets(ctx, client)
				return err
			},
		},
		{
			name: "manifest",
			fetch: func(ctx context.Context, hs HTTPSync, client *http.Client) error {
				_, _, err := hs.fetchManifest(ctx, client, TargetSet{})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter, _ *http.Request,
			) {
				SetProtocolHeader(writer.Header())
				writer.Header().Set("Content-Length", "20")
				_, _ = writer.Write([]byte(`{"dirs":`))
			}))
			t.Cleanup(server.Close)

			err := tt.fetch(
				t.Context(), HTTPSync{URL: server.URL}, server.Client(),
			)
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.True(t, IsHostUnavailable(err))
		})
	}
}

func TestIsHostUnavailableKeepsCompleteMalformedJSONActionable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter, _ *http.Request,
	) {
		SetProtocolHeader(writer.Header())
		_, _ = writer.Write([]byte(`{"dirs":`))
	}))
	t.Cleanup(server.Close)

	_, err := (HTTPSync{URL: server.URL}).fetchTargets(
		t.Context(), server.Client(),
	)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.False(t, IsHostUnavailable(err))
}

func TestIsHostUnavailableWhenHTTPManifestGzipHeaderIsTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter, _ *http.Request,
	) {
		SetProtocolHeader(writer.Header())
		writer.Header().Set("Content-Encoding", "gzip")
		writer.Header().Set("Content-Length", "20")
		_, _ = writer.Write([]byte{0x1f, 0x8b})
	}))
	t.Cleanup(server.Close)

	_, _, err := (HTTPSync{URL: server.URL}).fetchManifest(
		t.Context(), server.Client(), TargetSet{},
	)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.True(t, IsHostUnavailable(err))
}
