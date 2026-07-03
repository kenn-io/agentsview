package remotesync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// FailureSummary maps an HTTP remote sync error to a short,
// actionable message that is safe to surface through the API and
// CLI. Raw errors can embed the full remote URL and response
// bodies, so callers log the raw error locally and hand users this
// summary instead. Unrecognized errors collapse to the generic
// message rather than leaking their text.
func FailureSummary(err error) string {
	const generic = "HTTP remote sync failed"
	if err == nil {
		return generic
	}

	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.Code {
		case 401, 403:
			return fmt.Sprintf(
				"HTTP remote sync failed: remote daemon rejected the "+
					"sync token (%s); the token for this host in "+
					"[[remote_hosts]] must match the remote daemon's "+
					"auth_token", statusErr.Status,
			)
		case 404:
			return fmt.Sprintf(
				"HTTP remote sync failed: remote daemon has no "+
					"remote-sync endpoints (%s); upgrade agentsview on "+
					"the remote host", statusErr.Status,
			)
		default:
			return fmt.Sprintf(
				"HTTP remote sync failed: remote daemon returned %s",
				statusErr.Status,
			)
		}
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "HTTP remote sync failed: connection refused; check " +
			"that the remote daemon is running and bound to a " +
			"reachable address (serve --host 0.0.0.0 or host in its " +
			"config.toml), and that the url port matches"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "HTTP remote sync failed: cannot resolve the remote " +
			"host name; check the url in this [[remote_hosts]] entry"
	}

	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return "HTTP remote sync failed: connection timed out; check " +
			"that the remote host is reachable and the url is correct"
	}

	return generic
}
