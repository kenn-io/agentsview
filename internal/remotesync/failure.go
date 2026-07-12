package remotesync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
	var pending *PendingCleanupError
	if errors.As(err, &pending) {
		return "HTTP remote sync blocked: cleanup from an earlier sync " +
			"still owns resources"
	}

	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		// statusLabel derives the display text locally from the
		// numeric code: the response status line (and Detail) are
		// remote-controlled and must never reach the summary.
		switch statusErr.Code {
		case 401, 403:
			return fmt.Sprintf(
				"HTTP remote sync failed: remote daemon rejected the "+
					"sync token (%s); the token for this host in "+
					"[[remote_hosts]] must match the remote daemon's "+
					"auth_token", statusLabel(statusErr.Code),
			)
		case 404:
			return fmt.Sprintf(
				"HTTP remote sync failed: remote daemon has no "+
					"remote-sync endpoints (%s); upgrade agentsview on "+
					"the remote host", statusLabel(statusErr.Code),
			)
		default:
			return fmt.Sprintf(
				"HTTP remote sync failed: remote daemon returned %s",
				statusLabel(statusErr.Code),
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

// statusLabel renders an HTTP status for user-facing messages using
// only the locally-known status text for the code, never the
// remote-supplied reason phrase.
func statusLabel(code int) string {
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return strconv.Itoa(code)
}
