package remotesync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
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
